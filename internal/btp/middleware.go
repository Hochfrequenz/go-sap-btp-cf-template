package btp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"slices"

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
// Keep this as a string constant (not a typed key) so the key survives
// a future package extract without ceremony: both the middleware and
// the callers (even cross-package) just look up by the same literal.
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
// The check is exact — `User` matches `User`, not `user`, not `User.Admin`.
// Partial matching via strings.Contains is the classic bug here
// (`Unauthorized-User` would match `User`); keeping the check strict
// means a fork cannot accidentally widen access by tweaking the code.
//
// Install AFTER JWTValidator.Middleware() so "jwtClaims" is already in
// the context:
//
//	api := r.Group("/api")
//	api.Use(validator.Middleware())
//	api.GET("/admin", btp.RequireScope("Admin"), adminHandler)
//
// On failure, uses AbortError so the response body is the typed
// CodeForbidden envelope rather than a bare 403.
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
// of strings) or the OAuth 2 bare-string shape (space-separated). Both
// are valid in the wild; normalising at the read site keeps
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
		// OAuth-2 "scope" is space-separated; splitFields avoids empty
		// entries on double-space or leading-space input.
		return splitFields(v)
	}
	return nil
}

func splitFields(s string) []string {
	var out []string
	start := -1
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ' ' {
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}
	return out
}
