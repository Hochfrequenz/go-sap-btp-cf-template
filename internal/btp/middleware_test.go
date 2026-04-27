package btp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/hochfrequenz/go-sap-btp-cf-template/internal/btp"
)

func Test_RequestID_GeneratesWhenAbsent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(btp.RequestID())
	r.GET("/x", func(c *gin.Context) {
		rid, _ := c.Get(btp.RequestIDContextKey)
		c.String(http.StatusOK, rid.(string))
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))
	rid := w.Header().Get(btp.RequestIDHeader)
	then.AssertThat(t, rid != "", is.True())
	then.AssertThat(t, len(rid), is.EqualTo(16))
	// Body echoed from c.Get must match the header — both sides of the
	// contract read from the same source.
	then.AssertThat(t, w.Body.String(), is.EqualTo(rid))
}

func Test_RequestID_PreservesInbound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(btp.RequestID())
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(btp.RequestIDHeader, "external-rid-42")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Header().Get(btp.RequestIDHeader), is.EqualTo("external-rid-42"))
}

func Test_RequestID_PropagatesThroughContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(btp.RequestID())
	r.GET("/x", func(c *gin.Context) {
		got := btp.RequestIDFromContext(c.Request.Context())
		c.String(http.StatusOK, got)
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(btp.RequestIDHeader, "ctx-rid")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Body.String(), is.EqualTo("ctx-rid"))
}

func Test_RequestIDFromContext_NoMiddleware(t *testing.T) {
	// A bare context without the middleware yields "". Handlers that
	// want the ID unconditionally should either install the middleware
	// or tolerate the empty string.
	then.AssertThat(t, btp.RequestIDFromContext(context.Background()), is.EqualTo(""))
}

// stubClaimsMiddleware simulates JWTValidator.Middleware for scope tests
// without standing up a JWKS / token pair. We only need the "jwtClaims"
// key to be populated; RequireScope is tested end-to-end elsewhere.
func stubClaimsMiddleware(claims jwt.MapClaims) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("jwtClaims", claims)
		c.Next()
	}
}

func Test_RequireScope_AllowsWhenScopeArrayContains(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(stubClaimsMiddleware(jwt.MapClaims{
		"scope": []any{"User", "Admin"},
	}))
	r.GET("/admin", btp.RequireScope("Admin"), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))
	then.AssertThat(t, w.Body.String(), is.EqualTo("ok"))
}

func Test_RequireScope_AllowsWhenScopeStringContains(t *testing.T) {
	// OAuth-2 bare-string scope claim: "User Admin".
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(stubClaimsMiddleware(jwt.MapClaims{"scope": "User Admin"}))
	r.GET("/admin", btp.RequireScope("Admin"), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))
}

func Test_RequireScope_RejectsMissingScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(stubClaimsMiddleware(jwt.MapClaims{
		"scope": []any{"User"},
	}))
	r.GET("/admin", btp.RequireScope("Admin"), func(c *gin.Context) {
		t.Fatal("handler should not run")
	})

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusForbidden))
	var env btp.ErrorEnvelope
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Error.Code, is.EqualTo(btp.CodeForbidden))
}

// Test_RequireScope_RejectsPartialMatch pins the strict-equality rule:
// "User" must not grant access to a scope called "UserAdmin". If the
// check ever regresses to strings.Contains, this test fails — exactly
// the kind of scope-expansion bug that goes unnoticed until it matters.
func Test_RequireScope_RejectsPartialMatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(stubClaimsMiddleware(jwt.MapClaims{"scope": []any{"Unauthorized-User"}}))
	r.GET("/x", btp.RequireScope("User"), func(c *gin.Context) {
		t.Fatal("handler should not run")
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusForbidden))
}

// Test_RequireScope_RejectsPrefixMatch pins against the complementary
// regression: "Admin.Read" must not grant "Admin". If the check ever
// becomes strings.HasPrefix, this test fails. This kind of bug is
// insidious because lots of scope schemes use dotted hierarchy.
func Test_RequireScope_RejectsPrefixMatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(stubClaimsMiddleware(jwt.MapClaims{"scope": []any{"Admin.Read"}}))
	r.GET("/x", btp.RequireScope("Admin"), func(c *gin.Context) {
		t.Fatal("handler should not run")
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusForbidden))
}

// Test_RequireScope_AllowsWhenScopeStringSliceContains covers the
// []string wire shape that extractScopes explicitly supports but none
// of the other tests exercise.
func Test_RequireScope_AllowsWhenScopeStringSliceContains(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(stubClaimsMiddleware(jwt.MapClaims{
		"scope": []string{"User", "Admin"},
	}))
	r.GET("/admin", btp.RequireScope("Admin"), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))
}

// Test_RequireScope_StringClaimWithDoubleSpaces pins strings.Fields
// behaviour on the whitespace-separated scope shape: double spaces,
// tabs, and leading/trailing whitespace must all resolve to the same
// set of scope tokens.
func Test_RequireScope_StringClaimWithDoubleSpaces(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Deliberately messy: leading space, double space between tokens, trailing tab.
	r.Use(stubClaimsMiddleware(jwt.MapClaims{"scope": "  User   Admin\t"}))
	r.GET("/admin", btp.RequireScope("Admin"), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))
}

func Test_RequireScope_RejectsWhenMiddlewareAbsent(t *testing.T) {
	// No stubClaimsMiddleware — jwtClaims is absent. RequireScope must
	// treat that as forbidden, not as "let through".
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", btp.RequireScope("User"), func(c *gin.Context) {
		t.Fatal("handler should not run")
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusForbidden))
}

// Test_MaxBodySize_RejectsOversizedContentLength pins the fast path:
// a POST whose Content-Length exceeds the limit is rejected with a
// typed CodeRequestTooLarge envelope before the body is read at all.
func Test_MaxBodySize_RejectsOversizedContentLength(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(btp.RequestID(), btp.MaxBodySize(1024)) // 1 KiB cap for the test
	bodyReadCount := 0
	r.POST("/x", func(c *gin.Context) {
		// Should never run — middleware aborts before us.
		_, _ = io.Copy(io.Discard, c.Request.Body)
		bodyReadCount++
		c.String(http.StatusOK, "ok")
	})

	// 2 KiB body, well over the 1 KiB cap. Use bytes.Repeat to keep the
	// test source small.
	body := bytes.Repeat([]byte("A"), 2048)
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusRequestEntityTooLarge))
	var env btp.ErrorEnvelope
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Error.Code, is.EqualTo(btp.CodeRequestTooLarge))
	then.AssertThat(t, env.Error.RequestID != "", is.True())
	// Handler MUST NOT have run — the whole point of the fast path.
	then.AssertThat(t, bodyReadCount, is.EqualTo(0))
}

// Test_MaxBodySize_AllowsBodiesUnderLimit pins that bodies at or under
// the limit pass through to the handler unchanged.
func Test_MaxBodySize_AllowsBodiesUnderLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(btp.MaxBodySize(1024))
	r.POST("/x", func(c *gin.Context) {
		raw, err := io.ReadAll(c.Request.Body)
		then.AssertThat(t, err, is.Nil())
		c.String(http.StatusOK, string(raw))
	})

	body := bytes.Repeat([]byte("B"), 1024) // exactly at the limit
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))
	then.AssertThat(t, w.Body.Len(), is.EqualTo(1024))
}

// Test_MaxBodySize_CapsLyingContentLength pins the slow path: a client
// that sends Content-Length: 0 (or no header at all) but actually
// streams more than the limit hits the wrapped reader's MaxBytesError.
// The body never grows past `limit` bytes in memory — that's the
// invariant this middleware exists for.
func Test_MaxBodySize_CapsLyingContentLength(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(btp.MaxBodySize(1024))
	var readErr error
	var readBytes int
	r.POST("/x", func(c *gin.Context) {
		var raw []byte
		raw, readErr = io.ReadAll(c.Request.Body)
		readBytes = len(raw)
		c.String(http.StatusOK, "")
	})

	body := bytes.Repeat([]byte("C"), 4096) // 4 KiB actual body
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(body))
	req.ContentLength = -1 // simulate chunked / unknown length
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// The body read must error and never have read past the cap.
	then.AssertThat(t, readErr, is.Not(is.Nil()))
	then.AssertThat(t, readBytes <= 1024, is.True())
}

// Test_RequestID_PopulatesErrorEnvelope ties the two middlewares
// together: when AbortError fires in a later handler, the envelope
// carries the ID set by RequestID.
func Test_RequestID_PopulatesErrorEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(btp.RequestID())
	r.GET("/boom", func(c *gin.Context) {
		btp.AbortError(c, http.StatusBadGateway, btp.CodeUpstreamUnreachable,
			"on-premise call failed", nil)
	})

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	req.Header.Set(btp.RequestIDHeader, "envelope-rid")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var env btp.ErrorEnvelope
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Error.RequestID, is.EqualTo("envelope-rid"))
}
