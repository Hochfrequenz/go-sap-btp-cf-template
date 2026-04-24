package btp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/hochfrequenz/go-sap-btp-cloud-foundry-mwe/internal/btp"
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
