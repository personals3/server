// In-app docs server — dogfooded on PersonalS3 itself.
//
// Markdown files live in the "ps3-docs" public bucket (uploaded via
// `ps3 cp`). This endpoint lists the bucket's objects, derives a title
// + summary by peeking at the first ~60 lines of each, and returns
// PUBLIC URLs the dashboard can render without re-authenticating.
//
// Falling back to disk if the bucket doesn't exist keeps dev / fresh
// installs sane until someone runs the bootstrap script.
//
// Two endpoints:
//   GET /docs            → {docs: [{slug, title, summary, url}]}
//   GET /docs/{slug}     → 302 redirect to the public bucket URL
//
// Authenticated (any logged-in user) — though the URLs themselves are
// public, so a copy-pasted link works from anywhere.

package handlers

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/httpx"
	"github.com/personals3/api/internal/storage"
)

type DocsHandler struct {
	DB         *pgxpool.Pool
	FS         *storage.FS
	BucketName string // default "ps3-docs"
	// DocsRoot is the on-disk fallback used when the docs bucket isn't
	// populated yet (fresh install, dev box). Defaults to /app/docs.
	DocsRoot string
	// PublicBaseURL builds absolute URLs the dashboard and external
	// clients can use directly — e.g. https://docs.example.com. When
	// empty, falls back to the incoming request's scheme://host so a
	// single-host deploy still gets shareable links.
	PublicBaseURL string
}

type DocEntry struct {
	Slug    string `json:"slug"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Section string `json:"section,omitempty"`
	// URL is an ABSOLUTE link to the raw markdown on the public bucket.
	// Empty when serving from on-disk fallback.
	URL string `json:"url,omitempty"`
	// PageURL is the rendered HTML viewer for this doc. Always populated.
	PageURL string `json:"pageUrl,omitempty"`
}

func (h *DocsHandler) bucketName() string {
	if h.BucketName != "" {
		return h.BucketName
	}
	return "ps3-docs"
}

func (h *DocsHandler) root() string {
	if h.DocsRoot != "" {
		return h.DocsRoot
	}
	if r := os.Getenv("DOCS_ROOT"); r != "" {
		return r
	}
	return "/app/docs"
}

// List enumerates docs from the bucket first; if the bucket doesn't
// exist OR has zero objects, falls back to walking DocsRoot on disk.
// Each entry includes both:
//
//   url     — absolute link to the raw markdown on the public bucket
//   pageUrl — absolute link to the styled HTML view (this API)
//
// Both are absolute so the dashboard (or anyone with the URL) can fetch
// from any origin.
func (h *DocsHandler) List(w http.ResponseWriter, r *http.Request) {
	base := h.absoluteBase(r)
	if entries, ok := h.listFromBucket(r.Context()); ok && len(entries) > 0 {
		for i := range entries {
			if entries[i].URL != "" && strings.HasPrefix(entries[i].URL, "/") {
				entries[i].URL = base + entries[i].URL
			}
			entries[i].PageURL = base + "/api/docs/" + entries[i].Slug + ".html"
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"docs":   entries,
			"source": "bucket",
			"bucket": h.bucketName(),
		})
		return
	}
	// Fallback — disk
	disk := h.listFromDisk()
	for i := range disk {
		disk[i].PageURL = base + "/api/docs/" + disk[i].Slug + ".html"
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"docs":   disk,
		"source": "disk",
	})
}

// absoluteBase returns the scheme+host portion the client used to reach
// this API. Uses PublicBaseURL when configured (e.g. an explicit
// https://docs.example.com), otherwise derives from the request itself.
// Honors X-Forwarded-Proto / X-Forwarded-Host so reverse-proxy setups
// (nginx, caddy, cloudflared) produce the right URL.
func (h *DocsHandler) absoluteBase(r *http.Request) string {
	if h.PublicBaseURL != "" {
		return strings.TrimRight(h.PublicBaseURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
		scheme = fp
	}
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = fh
	}
	return scheme + "://" + host
}

// listFromBucket pulls all .md objects in the docs bucket and builds
// DocEntries with public URLs.
func (h *DocsHandler) listFromBucket(ctx context.Context) ([]DocEntry, bool) {
	if h.DB == nil || h.FS == nil {
		return nil, false
	}
	var bucketID string
	var isPublic bool
	err := h.DB.QueryRow(ctx,
		`SELECT id::text, COALESCE(is_public, false)
		   FROM buckets WHERE name = $1`, h.bucketName(),
	).Scan(&bucketID, &isPublic)
	if err != nil {
		return nil, false
	}
	rows, err := h.DB.Query(ctx, `
		SELECT key, storage_path FROM objects
		 WHERE bucket_id = $1 AND NOT is_deleted AND key LIKE '%.md'
		 ORDER BY key`, bucketID)
	if err != nil {
		return nil, false
	}
	defer rows.Close()

	docs := make([]DocEntry, 0, 32)
	for rows.Next() {
		var key, storagePath string
		if err := rows.Scan(&key, &storagePath); err != nil {
			continue
		}
		slug := strings.TrimSuffix(key, ".md")
		title, summary := extractTitleAndSummary(storagePath)
		section := ""
		if i := strings.Index(slug, "/"); i > 0 {
			section = slug[:i]
		}
		url := ""
		if isPublic {
			url = "/public/" + h.bucketName() + "/" + key
		}
		docs = append(docs, DocEntry{
			Slug: slug, Title: title, Summary: summary,
			Section: section, URL: url,
		})
	}
	return docs, true
}

// listFromDisk is the fallback when the bucket isn't bootstrapped.
func (h *DocsHandler) listFromDisk() []DocEntry {
	root := h.root()
	var docs []DocEntry
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		slug := strings.TrimSuffix(rel, ".md")
		title, summary := extractTitleAndSummary(path)
		section := ""
		if i := strings.Index(slug, "/"); i > 0 {
			section = slug[:i]
		}
		docs = append(docs, DocEntry{
			Slug: slug, Title: title, Summary: summary, Section: section,
		})
		return nil
	})
	return docs
}

// GetHTML returns a standalone HTML page with the doc rendered using
// the same Markdown rules + typography as the in-app reader. This means
// every doc has a permalink that works from any browser, no dashboard
// required.
//
// URL: /api/docs/{slug}.html
func (h *DocsHandler) GetHTML(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSuffix(chi.URLParam(r, "*"), ".html")
	if slug == "" || strings.Contains(slug, "..") || strings.HasPrefix(slug, "/") {
		http.Error(w, "invalid slug", http.StatusBadRequest)
		return
	}
	body, err := h.readMarkdown(r.Context(), slug)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	title := firstH1OrDefault(body, slug)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=60")
	_, _ = w.Write([]byte(renderHTMLPage(title, slug, string(body), h.absoluteBase(r))))
}

// readMarkdown reads a doc by slug — bucket first, then disk fallback.
func (h *DocsHandler) readMarkdown(ctx context.Context, slug string) ([]byte, error) {
	if h.DB != nil {
		var bucketID string
		if err := h.DB.QueryRow(ctx,
			`SELECT id::text FROM buckets WHERE name = $1`, h.bucketName(),
		).Scan(&bucketID); err == nil {
			var storagePath string
			if err := h.DB.QueryRow(ctx, `
				SELECT storage_path FROM objects
				 WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted`,
				bucketID, slug+".md",
			).Scan(&storagePath); err == nil {
				if b, err := os.ReadFile(storagePath); err == nil {
					return b, nil
				}
			}
		}
	}
	full := filepath.Join(h.root(), slug+".md")
	cleaned, _ := filepath.Abs(full)
	rootAbs, _ := filepath.Abs(h.root())
	if !strings.HasPrefix(cleaned, rootAbs) {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(cleaned)
}

// firstH1OrDefault peeks at the first ~30 lines for a `# heading`.
// Falls back to the slug's tail when none found.
func firstH1OrDefault(b []byte, slug string) string {
	for _, ln := range strings.SplitN(string(b), "\n", 30) {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "# ") {
			return strings.TrimSpace(ln[2:])
		}
	}
	if i := strings.LastIndex(slug, "/"); i >= 0 {
		return slug[i+1:]
	}
	return slug
}

// Get returns the raw markdown for one doc. Prefers a redirect to the
// public bucket URL when available; falls back to streaming from disk
// for non-bucket docs (and for backward compat with old callers that
// still hit this endpoint directly).
func (h *DocsHandler) Get(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "*")
	if strings.Contains(slug, "..") || strings.HasPrefix(slug, "/") {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_SLUG", "invalid doc slug")
		return
	}

	// Bucket path — redirect to the public URL if available.
	if h.DB != nil {
		var bucketID string
		var isPublic bool
		err := h.DB.QueryRow(r.Context(),
			`SELECT id::text, COALESCE(is_public, false)
			   FROM buckets WHERE name = $1`, h.bucketName(),
		).Scan(&bucketID, &isPublic)
		if err == nil && isPublic {
			var exists bool
			_ = h.DB.QueryRow(r.Context(), `
				SELECT TRUE FROM objects
				 WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted`,
				bucketID, slug+".md",
			).Scan(&exists)
			if exists {
				http.Redirect(w, r,
					"/public/"+h.bucketName()+"/"+slug+".md",
					http.StatusFound)
				return
			}
		}
	}

	// Disk fallback
	full := filepath.Join(h.root(), slug+".md")
	cleaned, _ := filepath.Abs(full)
	rootAbs, _ := filepath.Abs(h.root())
	if !strings.HasPrefix(cleaned, rootAbs) {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_SLUG", "doc escapes docs root")
		return
	}
	b, err := os.ReadFile(cleaned)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_DOC", "doc not found")
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

// extractTitleAndSummary reads the first ~30 lines of a markdown file
// and returns (firstH1, firstParagraph). Quick + cheap; doesn't try to
// be a full markdown parser.
func extractTitleAndSummary(path string) (string, string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return filepath.Base(path), ""
	}
	lines := strings.Split(string(b), "\n")
	var title, summary string
	inFence := false
	for i, ln := range lines {
		if i > 60 {
			break
		}
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if title == "" && strings.HasPrefix(t, "# ") {
			title = strings.TrimPrefix(t, "# ")
			continue
		}
		if title != "" && summary == "" && t != "" &&
			!strings.HasPrefix(t, "#") && !strings.HasPrefix(t, "---") &&
			!strings.HasPrefix(t, "|") {
			// Take the first sentence, capped at ~160 chars.
			summary = t
			if dot := strings.Index(summary, ". "); dot > 0 && dot < 160 {
				summary = summary[:dot+1]
			}
			if len(summary) > 160 {
				summary = summary[:157] + "..."
			}
			break
		}
	}
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	return title, summary
}
