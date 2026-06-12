package auth

// AWS Signature Version 4 verification.
// Implements just enough of https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_sigv-create-signed-request.html
// for S3 clients (aws-cli, boto3, aws-sdk-*) to authenticate against this server.
//
// Supports both:
//   - x-amz-content-sha256: <hex of body SHA-256>       (default)
//   - x-amz-content-sha256: UNSIGNED-PAYLOAD            (streaming uploads)
//
// Does NOT yet support:
//   - "STREAMING-AWS4-HMAC-SHA256-PAYLOAD" (chunked signing) — uncommon
//   - Pre-signed URLs (signed query strings)            — TODO
//   - Multipart upload signing through trailers          — uncommon

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

var debugSigV4 = os.Getenv("DEBUG_SIGV4") != ""

const (
	algorithm      = "AWS4-HMAC-SHA256"
	unsignedPayloadSentinel = "UNSIGNED-PAYLOAD"
	emptyPayloadSHA256      = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// SigV4Components is the parsed Authorization header.
type SigV4Components struct {
	AccessKeyID    string
	Date           string // YYYYMMDD
	Region         string
	Service        string
	SignedHeaders  []string
	Signature      string
}

// ParseAuthHeader splits "AWS4-HMAC-SHA256 Credential=AKID/20240115/us-east-1/s3/aws4_request,
// SignedHeaders=host;x-amz-date, Signature=hex..." into its parts.
func ParseAuthHeader(h string) (*SigV4Components, error) {
	if !strings.HasPrefix(h, algorithm+" ") {
		return nil, errors.New("not a SigV4 header")
	}
	rest := strings.TrimPrefix(h, algorithm+" ")

	c := &SigV4Components{}
	for _, part := range splitCommaWithSpaces(rest) {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("malformed component: %q", part)
		}
		k, v := kv[0], kv[1]
		switch k {
		case "Credential":
			scope := strings.Split(v, "/")
			if len(scope) != 5 {
				return nil, fmt.Errorf("bad credential scope: %q", v)
			}
			c.AccessKeyID = scope[0]
			c.Date = scope[1]
			c.Region = scope[2]
			c.Service = scope[3]
			if scope[4] != "aws4_request" {
				return nil, errors.New("credential scope must end in aws4_request")
			}
		case "SignedHeaders":
			c.SignedHeaders = strings.Split(v, ";")
			sort.Strings(c.SignedHeaders)
		case "Signature":
			c.Signature = v
		}
	}
	if c.AccessKeyID == "" || c.Signature == "" || len(c.SignedHeaders) == 0 {
		return nil, errors.New("missing required credential parts")
	}
	return c, nil
}

// splitCommaWithSpaces handles "Credential=A/B/C, SignedHeaders=x;y, Signature=abc".
// Just splitting on "," would work, but headers may have whitespace.
func splitCommaWithSpaces(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// maxSignedPayloadBytes caps how much body VerifySigV4 buffers to check a
// signed payload hash. Verifying a signed payload requires the whole body
// in memory, so an arbitrarily large signed PUT could balloon the API
// process. Clients uploading more than this must send
// x-amz-content-sha256: UNSIGNED-PAYLOAD (aws-cli/boto3 already do over
// HTTPS) or use multipart. Streaming verification
// (STREAMING-AWS4-HMAC-SHA256-PAYLOAD) is in FUTURE_PLANS.
const maxSignedPayloadBytes = 64 << 20 // 64 MiB

// ErrSignedPayloadTooLarge is surfaced to the client verbatim — tell them
// exactly how to proceed.
var ErrSignedPayloadTooLarge = fmt.Errorf(
	"signed payload larger than %d MiB is not supported — send "+
		"x-amz-content-sha256: UNSIGNED-PAYLOAD for large uploads, or use "+
		"multipart", maxSignedPayloadBytes>>20)

// VerifySigV4 returns nil if the request's signature matches a freshly-computed
// one using the given secret key. Returns an error describing the mismatch
// otherwise. The body is read into memory (capped at maxSignedPayloadBytes)
// if the request's signed payload hash is not UNSIGNED-PAYLOAD.
func VerifySigV4(r *http.Request, comps *SigV4Components, secretKey string) error {
	// 1. Hash the payload (or use sentinel)
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		// AWS CLI doesn't always send this for GET/HEAD — assume empty body.
		payloadHash = emptyPayloadSHA256
	}
	if payloadHash != unsignedPayloadSentinel {
		// We need to verify the body matches; this reads it all.
		if r.ContentLength > maxSignedPayloadBytes {
			return ErrSignedPayloadTooLarge
		}
		bodyHash, err := hashRequestBody(r)
		if err != nil {
			return fmt.Errorf("hash body: %w", err)
		}
		if bodyHash != payloadHash {
			return errors.New("body hash mismatch")
		}
	}

	// 2. Canonical request
	canonicalReq := buildCanonicalRequest(r, comps.SignedHeaders, payloadHash)

	// 3. String to sign
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		return errors.New("missing x-amz-date header")
	}
	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request",
		comps.Date, comps.Region, comps.Service)
	stringToSign := strings.Join([]string{
		algorithm,
		amzDate,
		credentialScope,
		hexSHA256([]byte(canonicalReq)),
	}, "\n")

	// 4. Signing key (derived from secret + scope)
	signingKey := deriveSigningKey(secretKey, comps.Date, comps.Region, comps.Service)

	// 5. Expected signature
	expected := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	if !hmac.Equal([]byte(expected), []byte(comps.Signature)) {
		if debugSigV4 {
			log.Printf("SIGV4 MISMATCH\nClient sig:    %s\nComputed sig:  %s\nCanonicalRequest:\n%s\n--- end ---\nStringToSign:\n%s\n--- end ---",
				comps.Signature, expected, canonicalReq, stringToSign)
		}
		return errors.New("signature mismatch")
	}

	// Sanity: reject signatures older than 15 minutes (replay protection)
	t, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return fmt.Errorf("bad amz-date: %w", err)
	}
	if time.Since(t) > 15*time.Minute {
		return errors.New("signature too old (>15min)")
	}

	return nil
}

func buildCanonicalRequest(r *http.Request, signedHeaders []string, payloadHash string) string {
	// For S3 SigV4: the canonical URI is the path AS-WIRE (already percent-
	// encoded by the client). Re-encoding would double-encode %20 → %2520.
	//
	// We prefer X-Original-URI (set by nginx, contains the raw HTTP request
	// line) over r.URL.Path (which Go has decoded for us).
	var uri string
	query := canonicalQueryString(r.URL.Query())

	if orig := r.Header.Get("X-Original-URI"); orig != "" {
		if i := strings.IndexByte(orig, '?'); i >= 0 {
			uri = orig[:i]
			if u, err := url.ParseQuery(orig[i+1:]); err == nil {
				query = canonicalQueryString(u)
			}
		} else {
			uri = orig
		}
		// orig is already wire-encoded — DO NOT re-encode it.
	} else {
		// Fallback: we don't have the raw URI from nginx. Encode the
		// decoded path back to its canonical form ourselves.
		uri = canonicalURI(r.URL.Path)
	}
	if uri == "" {
		uri = "/"
	}

	// Sorted "name:value\n" lines for the signed headers
	hdr := strings.Builder{}
	for _, name := range signedHeaders {
		// Special: "host" comes from r.Host, not r.Header.
		var val string
		if name == "host" {
			val = r.Host
		} else {
			val = r.Header.Get(name)
		}
		hdr.WriteString(name)
		hdr.WriteByte(':')
		hdr.WriteString(strings.TrimSpace(val))
		hdr.WriteByte('\n')
	}

	return strings.Join([]string{
		r.Method,
		uri,
		query,
		hdr.String(),
		strings.Join(signedHeaders, ";"),
		payloadHash,
	}, "\n")
}

// canonicalURI URI-encodes each segment of the path per RFC 3986, but does NOT
// double-encode "/". S3 requires the path to be URI-encoded EXCEPT the slashes.
func canonicalURI(p string) string {
	segments := strings.Split(p, "/")
	for i, s := range segments {
		segments[i] = awsURIEncode(s, false)
	}
	return strings.Join(segments, "/")
}

func canonicalQueryString(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(q))
	for _, k := range keys {
		vals := q[k]
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, awsURIEncode(k, true)+"="+awsURIEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// awsURIEncode follows AWS's spec — like url.QueryEscape but treats slash + a
// few extras differently, and uppercase hex.
func awsURIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'),
			c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		case c == '/':
			if encodeSlash {
				b.WriteString("%2F")
			} else {
				b.WriteByte(c)
			}
		default:
			b.WriteString(fmt.Sprintf("%%%02X", c))
		}
	}
	return b.String()
}

func deriveSigningKey(secret, date, region, service string) []byte {
	kDate    := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion  := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return kSigning
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// hashRequestBody reads the entire body into memory, replaces r.Body with
// a fresh reader, and returns the hex SHA-256.
//
// For UNSIGNED-PAYLOAD requests this isn't called, which is critical for
// large file uploads where buffering would blow memory. Bodies over
// maxSignedPayloadBytes are rejected — the LimitReader also covers
// chunked requests that carry no Content-Length.
func hashRequestBody(r *http.Request) (string, error) {
	if r.Body == nil {
		return emptyPayloadSHA256, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSignedPayloadBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(body)) > maxSignedPayloadBytes {
		return "", ErrSignedPayloadTooLarge
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	return hexSHA256(body), nil
}

// GenerateS3Credentials creates an (AKID, secret) pair in the AWS format.
// AKID is "AKIA" + 16 chars (20 total); secret is 40-char base64-ish.
func GenerateS3Credentials() (accessKeyID, secretKey string, err error) {
	idBytes := make([]byte, 8) // 8 raw → 16 hex
	if _, err := readRandom(idBytes); err != nil {
		return "", "", err
	}
	accessKeyID = "AKIA" + strings.ToUpper(hex.EncodeToString(idBytes))

	secretBytes := make([]byte, 30) // 30 raw → 40 base64 chars
	if _, err := readRandom(secretBytes); err != nil {
		return "", "", err
	}
	secretKey = strings.TrimRight(base64URL(secretBytes), "=")
	return accessKeyID, secretKey, nil
}
