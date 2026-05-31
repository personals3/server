// Tiny transactional email layer.
//
// When SMTP_HOST is set we relay via plain net/smtp (LOGIN auth). Otherwise
// we write a .eml file to {STORAGE_ROOT}/.email-outbox/ and log a preview
// — the admin panel surfaces the outbox so an operator can read the OTP
// even before they wire real SMTP.
//
// Templates are minimal Go text/template strings inlined here. Adding a
// new transactional template = a new function + Render call below.
package email

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/google/uuid"
)

// Config is what `Load` builds out of env.
type Config struct {
	Host     string
	Port     string
	User     string
	Pass     string
	From     string
	FromName string
	// ReplyTo, if set, becomes the `Reply-To:` header on every outbound
	// message. Pattern: `From: noreply@... / Reply-To: support@...` —
	// the From stays "noreply" for visual hint, but if someone hits
	// reply their mail goes to a real, monitored address.
	ReplyTo  string
	UseTLS   bool
	// OutboxDir is where rendered emails land in dev-mode (no SMTP). Defaults
	// to {STORAGE_ROOT}/.email-outbox. The admin panel reads from here.
	OutboxDir string
}

func Load(storageRoot string) Config {
	return Config{
		Host:      os.Getenv("SMTP_HOST"),
		Port:      strDefault(os.Getenv("SMTP_PORT"), "587"),
		User:      os.Getenv("SMTP_USER"),
		Pass:      os.Getenv("SMTP_PASS"),
		From:      strDefault(os.Getenv("SMTP_FROM"), "no-reply@personals3.local"),
		FromName:  strDefault(os.Getenv("SMTP_FROM_NAME"), "PersonalS3"),
		ReplyTo:   os.Getenv("SMTP_REPLY_TO"),  // optional; empty = no header
		UseTLS:    os.Getenv("SMTP_TLS") != "0",
		OutboxDir: filepath.Join(strDefault(storageRoot, "/storage"), ".email-outbox"),
	}
}

func (c Config) Live() bool { return c.Host != "" }

// Mailer is the runtime sender — created once per process.
type Mailer struct {
	cfg Config
}

func New(cfg Config) *Mailer {
	if !cfg.Live() {
		_ = os.MkdirAll(cfg.OutboxDir, 0o755)
		log.Printf("email: SMTP not configured — using outbox at %s", cfg.OutboxDir)
	}
	return &Mailer{cfg: cfg}
}

// Send delivers (or drops to outbox) one message. Subject + bodyText are
// required; bodyHTML is optional. The To slice is comma-joined into the
// envelope; for transactional mail you almost always send to one address.
func (m *Mailer) Send(to []string, subject, bodyText, bodyHTML string) error {
	msg := buildMIME(m.cfg.From, m.cfg.FromName, m.cfg.ReplyTo, to, subject, bodyText, bodyHTML)
	if m.cfg.Live() {
		return m.sendSMTP(to, msg)
	}
	return m.writeOutbox(to, subject, msg)
}

// --- transport implementations ----------------------------------------------

func (m *Mailer) sendSMTP(to []string, msg []byte) error {
	addr := m.cfg.Host + ":" + m.cfg.Port
	var auth smtp.Auth
	if m.cfg.User != "" {
		auth = smtp.PlainAuth("", m.cfg.User, m.cfg.Pass, m.cfg.Host)
	}
	if !m.cfg.UseTLS {
		return smtp.SendMail(addr, auth, m.cfg.From, to, msg)
	}
	// STARTTLS path — go's smtp.SendMail doesn't do this cleanly; we drive
	// the conversation manually so we work on Gmail/Mailgun/SES/etc.
	c, err := smtp.Dial(addr)
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}
	defer c.Close()
	if err := c.StartTLS(&tls.Config{ServerName: m.cfg.Host}); err != nil {
		return fmt.Errorf("starttls: %w", err)
	}
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}
	if err := c.Mail(m.cfg.From); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	for _, t := range to {
		if err := c.Rcpt(t); err != nil {
			return fmt.Errorf("rcpt %s: %w", t, err)
		}
	}
	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := wc.Write(msg); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("close data: %w", err)
	}
	return c.Quit()
}

// writeOutbox is the dev fallback. One .eml per message — RFC822-ish so
// most mail clients (Thunderbird, Apple Mail) can preview them too.
func (m *Mailer) writeOutbox(to []string, subject string, msg []byte) error {
	if err := os.MkdirAll(m.cfg.OutboxDir, 0o755); err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	name := fmt.Sprintf("%s-%s-%s.eml", stamp, uuid.New().String()[:8],
		safeFilename(strings.Join(to, "_")))
	full := filepath.Join(m.cfg.OutboxDir, name)
	if err := os.WriteFile(full, msg, 0o600); err != nil {
		return err
	}
	log.Printf("email: outbox → %s  [to=%s] [subj=%q]",
		full, strings.Join(to, ","), subject)
	return nil
}

// --- MIME builder -----------------------------------------------------------

func buildMIME(from, fromName, replyTo string, to []string, subject, text, html string) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "From: %s <%s>\r\n", fromName, from)
	if replyTo != "" {
		// Mail clients honour Reply-To when the user hits Reply, even
		// though the From may be a no-reply address. Lets us drop bounces
		// to noreply@ while still giving humans a real address.
		fmt.Fprintf(&buf, "Reply-To: %s\r\n", replyTo)
	}
	fmt.Fprintf(&buf, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&buf, "Subject: %s\r\n", subject)
	fmt.Fprintf(&buf, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&buf, "MIME-Version: 1.0\r\n")
	if html != "" {
		boundary := "ps3" + uuid.New().String()[:12]
		fmt.Fprintf(&buf, "Content-Type: multipart/alternative; boundary=\"%s\"\r\n", boundary)
		fmt.Fprintf(&buf, "\r\n")
		fmt.Fprintf(&buf, "--%s\r\nContent-Type: text/plain; charset=\"UTF-8\"\r\n\r\n%s\r\n", boundary, text)
		fmt.Fprintf(&buf, "--%s\r\nContent-Type: text/html; charset=\"UTF-8\"\r\n\r\n%s\r\n", boundary, html)
		fmt.Fprintf(&buf, "--%s--\r\n", boundary)
	} else {
		fmt.Fprintf(&buf, "Content-Type: text/plain; charset=\"UTF-8\"\r\n\r\n%s\r\n", text)
	}
	return buf.Bytes()
}

// --- helpers ----------------------------------------------------------------

func strDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func safeFilename(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '.', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) > 80 {
		out = out[:80]
	}
	return string(out)
}

// --- templates --------------------------------------------------------------
//
// Inlined for now — the set is tiny. If it grows past ~10 templates,
// move them to embedded files.

// Every user-facing template uses {{.SupportEmail}} to surface a real,
// monitored address. Replies on the underlying email also go there
// (Reply-To header), but spelling it out in the body means users who
// dig the message out of an archive 6 months later still know where to
// turn.

const tmplVerifyEmail = `Hi {{.Name}},

Welcome to {{.Brand}}. Use this one-time code to finish setting up your account:

    {{.Code}}

The code expires in {{.TTLMinutes}} minutes. If you didn't request this, ignore this email — your address won't be used.

Need help, or did the code expire?
  Reply to this email, or write to {{.SupportEmail}}.

— {{.Brand}}
(this address doesn't accept replies — please use {{.SupportEmail}} instead.)
`

const tmplPasswordReset = `Hi,

Someone (hopefully you) asked to reset the password for {{.Email}} on {{.Brand}}.

Use this one-time code:

    {{.Code}}

It expires in {{.TTLMinutes}} minutes and works once. If you didn't ask for this, you can safely ignore this email.

Stuck? Reply to this email or contact {{.SupportEmail}}.

— {{.Brand}}
(this address doesn't accept replies — please use {{.SupportEmail}} instead.)
`

const tmplAccountApproved = `Hi {{.Name}},

Your access request for {{.Brand}} has been approved.

  Email:       {{.Email}}
  Quota:       {{.QuotaHuman}}
  Sign in at:  {{.LoginURL}}

You'll need to set a password the first time — we sent a separate "verify your email" message with a one-time code.

Questions? Reply here or write to {{.SupportEmail}}.

— {{.Brand}}
(this address doesn't accept replies — please use {{.SupportEmail}} instead.)
`

const tmplAccountDenied = `Hi,

Your access request for {{.Brand}} was not approved at this time.

{{if .Note}}Note from the administrator:

    {{.Note}}

{{end}}If you think this was a mistake, reply to this email or write to {{.SupportEmail}}.

— {{.Brand}}
(this address doesn't accept replies — please use {{.SupportEmail}} instead.)
`

const tmplQuotaApproved = `Hi {{.Name}},

Your quota request for {{.Brand}} was approved.

  New quota:  {{.NewQuotaHuman}}
  Granted:    {{.GrantedHuman}}{{if .Note}}

  Note: {{.Note}}{{end}}

Need a different amount, or have a question? Write to {{.SupportEmail}}.

— {{.Brand}}
(this address doesn't accept replies — please use {{.SupportEmail}} instead.)
`

const tmplQuotaDenied = `Hi {{.Name}},

Your quota request for {{.Brand}} was not approved at this time.

{{if .Note}}Note from the administrator:

    {{.Note}}

{{end}}You can submit another request anytime from your dashboard, or write to {{.SupportEmail}}.

— {{.Brand}}
(this address doesn't accept replies — please use {{.SupportEmail}} instead.)
`

const tmplAdminNewAccountRequest = `Hi admin,

A new {{.Brand}} access request is waiting:

  Email:    {{.Email}}
  Name:     {{.Name}}
  Reason:   {{.Reason}}

Approve or deny in the dashboard: {{.AdminURL}}

— {{.Brand}}
`

const tmplAdminNewQuotaRequest = `Hi admin,

{{.UserEmail}} asked for more space on {{.Brand}}:

  Current quota:  {{.CurrentHuman}}
  Requested:      {{.RequestedHuman}} more
  Reason:         {{.Reason}}

Approve or deny in the dashboard: {{.AdminURL}}

— {{.Brand}}
`

const tmplConfirmDelete = `Hi {{.Name}},

You asked to permanently delete your {{.Brand}} account. To confirm,
enter this one-time code in the dashboard:

    {{.Code}}

The code expires in {{.TTLMinutes}} minutes. Once you confirm:
  - your buckets, files, share links, and API keys are deleted
  - your email address can be used to request a new account later
  - this CANNOT be undone (no trash, no grace period)

If you DIDN'T ask to delete your account, ignore this email — nothing
changes — and contact {{.SupportEmail}} immediately. Someone may have
your password.

— {{.Brand}}
(this address doesn't accept replies — please use {{.SupportEmail}} instead.)
`

const tmplAdminAccountDeleted = `Hi admin,

The user {{.Email}} has self-deleted their {{.Brand}} account.

  Confirmed at:   {{.When}}
  IP at request:  {{.IP}}

All their data has been removed. No action needed.

— {{.Brand}}
`

// Render fills a template + bodyVars and returns (text, html) — for now
// html mirrors text wrapped in a <pre>; can be styled later without
// changing call sites.
func Render(tmpl string, vars any) (string, string, error) {
	t, err := template.New("e").Parse(tmpl)
	if err != nil {
		return "", "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, vars); err != nil {
		return "", "", err
	}
	text := buf.String()
	html := "<pre style=\"font: 14px ui-monospace,Menlo,monospace; white-space: pre-wrap; max-width: 560px;\">" +
		htmlEscape(text) + "</pre>"
	return text, html, nil
}

func htmlEscape(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	).Replace(s)
}

// Templates returns the set so callers can pick by name and avoid
// importing each constant individually.
var Templates = map[string]string{
	"verify_email":              tmplVerifyEmail,
	"password_reset":            tmplPasswordReset,
	"account_approved":          tmplAccountApproved,
	"account_denied":            tmplAccountDenied,
	"quota_approved":            tmplQuotaApproved,
	"quota_denied":              tmplQuotaDenied,
	"confirm_delete":            tmplConfirmDelete,
	"admin_new_account_request": tmplAdminNewAccountRequest,
	"admin_new_quota_request":   tmplAdminNewQuotaRequest,
	"admin_account_deleted":     tmplAdminAccountDeleted,
}

// ErrTemplateNotFound is returned by SendTemplate if name isn't registered.
var ErrTemplateNotFound = errors.New("email template not found")

// SendTemplate is the high-level helper — given a template name, recipient,
// and variables, renders + dispatches. Returns the rendered text body so
// the caller (e.g. admin endpoint) can include it in the response if useful.
func (m *Mailer) SendTemplate(to, subject, templateName string, vars any) (string, error) {
	tmpl, ok := Templates[templateName]
	if !ok {
		return "", ErrTemplateNotFound
	}
	text, html, err := Render(tmpl, vars)
	if err != nil {
		return "", err
	}
	if err := m.Send([]string{to}, subject, text, html); err != nil {
		return "", err
	}
	return text, nil
}

// FormatBytes returns "100 MB" / "1.5 GB" — used in template vars.
func FormatBytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(u), 0
	for x := n / u; x >= u; x /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
