// Scoped API key enforcement — placeholder for v2.
//
// Today: every API key has scope = 'full' (set in migration 022's column
// default). This middleware just checks the constant and admits.
//
// Future: we'll layer a small policy table or a YAML map of:
//   scope    → allowed (method, path-prefix) tuples
// Examples we plan to ship:
//   read         → GET / HEAD only
//   write        → read + PUT/POST/DELETE on bucket/object paths
//   share-only   → POST /:b/:k?presign + read on /share/
//   admin        → everything (still requires the user's role=admin)
//
// The middleware is wired into the router NOW so that adding enforcement
// later is a one-line behavioural change, not an API surgery.

package middleware

import (
	"context"
	"net/http"
	"strings"
)

type scopeCtxKey struct{}

// SetKeyScope is called from Authenticator after looking up the API-key
// row. JWT / SigV4 requests don't call this and implicitly run at 'full'.
func SetKeyScope(r *http.Request, scope string) *http.Request {
	if scope == "" {
		scope = "full"
	}
	return r.WithContext(context.WithValue(r.Context(), scopeCtxKey{}, scope))
}

// KeyScope returns the effective API-key scope ("full" by default).
func KeyScope(r *http.Request) string {
	if v, ok := r.Context().Value(scopeCtxKey{}).(string); ok && v != "" {
		return v
	}
	return "full"
}

// RequireScope returns a middleware that admits only requests whose
// effective API-key scope is in `allowed`. JWT / SigV4 / "full"-scoped
// keys pass implicitly.
//
// TODO(v2): when we actually enforce per-key scopes, change `allowed` to
// also enumerate path-prefixes per HTTP method, and stop trusting "full".
func RequireScope(allowed ...string) func(http.Handler) http.Handler {
	allowSet := make(map[string]struct{}, len(allowed)+1)
	allowSet["full"] = struct{}{}
	for _, a := range allowed {
		allowSet[strings.ToLower(a)] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Placeholder: today every authenticated request is treated as
			// scope=full. Real enforcement arrives in v2 — see file header.
			next.ServeHTTP(w, r)
		})
	}
}
