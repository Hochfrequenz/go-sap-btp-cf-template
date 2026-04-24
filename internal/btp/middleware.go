package btp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// RequestIDHeader is the inbound/outbound HTTP header carrying the
// request ID. Downstream systems (approuter, BTP app-logging) use this
// exact spelling; keeping it as a constant means a fork that changes
// it doesn't silently break correlation with the platform.
const RequestIDHeader = "X-Request-Id"

// RequestIDContextKey is the Gin context key under which the middleware
// stashes the request ID. `AbortError` reads it via c.Get("request_id")
// to populate the error envelope; handlers that want to annotate their
// slog lines with the request ID should do the same.
//
// This is a string because Gin's c.Set/c.Get map uses string keys.
// The context.Context propagation path uses a separately-defined
// typed key (requestIDCtxKey{}) — that's the canonical Go pattern for
// ctx.Value to avoid cross-package collisions (go vet / staticcheck
// flag string keys on context.WithValue).
const RequestIDContextKey = "request_id"

// RequestID reads an existing X-Request-Id header or generates a
// short hex token, stashes it in the Gin context under
// RequestIDContextKey, and echoes it back in the response. Installing
// this on the top-level router means every handler, every error
// envelope, and the access log share one correlation ID.
//
// The generated ID is 16 hex chars (8 random bytes) — short enough to
// eyeball, wide enough to be effectively unique within a single
// deployment's log retention. A UUID is fine too; this is meant as the
// template default, forks can swap.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader(RequestIDHeader)
		if rid == "" {
			rid = newRequestID()
		}
		c.Set(RequestIDContextKey, rid)
		c.Writer.Header().Set(RequestIDHeader, rid)
		// Also propagate through the request context so handlers using
		// slog.InfoContext pick it up if they wire an slog handler that
		// reads the value (templates typically don't, but making the
		// value reachable from ctx means it is there when they do).
		ctx := context.WithValue(c.Request.Context(), requestIDCtxKey{}, rid)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// RequestIDFromContext returns the request ID stored by the RequestID
// middleware on the request Context, or "" if the middleware isn't in
// the chain. Handlers that need the ID outside a Gin context (e.g. a
// background goroutine they kick off) can read it here.
func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(requestIDCtxKey{}).(string)
	return v
}

type requestIDCtxKey struct{}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand never errors on supported platforms; if it
		// somehow does, a constant placeholder is safer than
		// panicking a request-handling goroutine.
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// RequireScope aborts the request with 403 forbidden unless the
// validated JWT's "scope" claim contains the exact scope string given.
//
// The check is exact — `User` matches `User`, not `user`, not `User.Admin`,
// not `Unauthorized-User`. Partial matching via strings.Contains /
// strings.HasPrefix / strings.HasSuffix is the classic bug here; keeping
// the check strict means a fork cannot accidentally widen access by
// tweaking the code.
//
// **XSUAA qualified-scope gotcha.** Real XSUAA tokens emit scopes
// qualified with xsappname + tenant, e.g.
//
//	"go-btp-mwe!t1234.Admin"
//
// not a bare `"Admin"`. Pass the qualified string you actually see in
// the token — the value of `XSUAACredentials.XSAppName` plus `!t` plus
// the tenant suffix plus `.` plus the scope template name from
// `xs-security.json`. Concretely: look at `/api/me`'s response once and
// copy the exact shape out of the `scope` claim. Forks that pass a bare
// name will 403 every request — this is the single most common
// surprise when wiring a first scope-gated route.
//
// Install AFTER JWTValidator.Middleware() so "jwtClaims" is already in
// the context:
//
//	api := r.Group("/api")
//	api.Use(validator.Middleware())
//	api.GET("/admin", btp.RequireScope("go-btp-mwe!t1234.Admin"), adminHandler)
//
// On failure, AbortError writes the typed CodeForbidden envelope. The
// underlying miss is NOT logged (err = nil) because 403 is a routine
// authz decision rather than a server-side error; forks that want the
// miss logged can wrap RequireScope and log themselves.
func RequireScope(scope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := c.Get("jwtClaims")
		if !ok {
			// JWTValidator.Middleware missing from the chain — treat
			// as forbidden so a mis-wired router cannot expose a
			// scope-gated endpoint by accident.
			AbortError(c, http.StatusForbidden, CodeForbidden,
				"scope check requires authenticated user", nil)
			return
		}
		claims, ok := raw.(jwt.MapClaims)
		if !ok {
			AbortError(c, http.StatusForbidden, CodeForbidden,
				"scope check requires authenticated user", nil)
			return
		}
		scopes := extractScopes(claims)
		if !slices.Contains(scopes, scope) {
			AbortError(c, http.StatusForbidden, CodeForbidden,
				"missing required scope", nil)
			return
		}
		c.Next()
	}
}

// extractScopes reads the "scope" claim in either XSUAA shape (an array
// of strings) or the OAuth 2 bare-string shape (whitespace-separated).
// Both are valid in the wild; normalising at the read site keeps
// RequireScope strict without needing two near-identical code paths.
func extractScopes(claims jwt.MapClaims) []string {
	switch v := claims["scope"].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, s := range v {
			if str, ok := s.(string); ok {
				out = append(out, str)
			}
		}
		return out
	case []string:
		return v
	case string:
		// strings.Fields splits on any Unicode whitespace and drops
		// empty entries, so double spaces / tabs / leading-trailing
		// whitespace all Just Work.
		return strings.Fields(v)
	}
	return nil
}
