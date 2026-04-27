package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"
	"github.com/gin-gonic/gin"

	"github.com/hochfrequenz/go-sap-btp-cf-template/internal/btp"
)

// Test_logLevelFromEnv_MapsKnownAndUnknown pins the LOG_LEVEL contract
// that lives in the README §"Logging" section: debug/info/error are
// recognised, empty defaults to INFO, and anything else (crucially
// "warn", which the template deliberately does not map) also falls
// back to INFO. A future refactor that tries to be "helpful" by
// mapping warn → WarnLevel would trip this test.
func Test_logLevelFromEnv_MapsKnownAndUnknown(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"", slog.LevelInfo},
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"error", slog.LevelError},
		{"warn", slog.LevelInfo},     // deliberately not mapped
		{"warning", slog.LevelInfo},  // deliberately not mapped
		{"trace", slog.LevelInfo},    // unknown → INFO
		{"nonsense", slog.LevelInfo}, // unknown → INFO
	}
	for _, c := range cases {
		t.Setenv("LOG_LEVEL", c.in)
		got := logLevelFromEnv()
		then.AssertThat(t, got, is.EqualTo(c.want))
	}
}

// Test_recoverPanic_EmitsTypedEnvelope pins the recovery contract: a
// panicking handler must produce a JSON btp.ErrorEnvelope with code
// = "internal", not Gin's plain-text "Internal Server Error". A future
// refactor that reverts to gin.Recovery() trips this test.
func Test_recoverPanic_EmitsTypedEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(recoverPanic(), btp.RequestID())
	r.GET("/boom", func(_ *gin.Context) {
		panic("kaboom")
	})

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusInternalServerError))
	then.AssertThat(t, w.Header().Get("Content-Type"),
		is.EqualTo("application/json; charset=utf-8"))

	var env btp.ErrorEnvelope
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Error.Code, is.EqualTo(btp.CodeInternal))
	then.AssertThat(t, env.Error.Message, is.EqualTo("internal error"))
	// Crucially: the panic value must NOT leak to the client. "kaboom"
	// belongs in the operator-side log line via AbortError, not in the
	// envelope message.
	then.AssertThat(t, strings.Contains(w.Body.String(), "kaboom"), is.False())

	// RequestID middleware ran inside the recovery wrap, so the envelope
	// carries the same ID the access log would record. A panic before
	// RequestID would emit an empty request_id — acceptable for that
	// pre-request edge case, but normal handler-internal panics should
	// always carry one.
	then.AssertThat(t, env.Error.RequestID != "", is.True())
}

// Test_recoverPanic_PreservesClientSuppliedRequestID pins that an
// X-Request-ID header from a trusted upstream (e.g. the approuter)
// flows through into the panic envelope, not just into normal access
// logs. A correlated panic + log + client header is the shortest path
// from a customer report to the operator-side line.
func Test_recoverPanic_PreservesClientSuppliedRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(recoverPanic(), btp.RequestID())
	r.GET("/boom", func(_ *gin.Context) {
		panic("kaboom")
	})

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	req.Header.Set("X-Request-ID", "rid-from-approuter")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var env btp.ErrorEnvelope
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Error.RequestID, is.EqualTo("rid-from-approuter"))
}

// Test_requestLog_OmitsQueryStringAndClaims pins the deliberate-omission
// policy documented on requestLog: the access log records method, path,
// status, duration, client IP, and request ID — never query string,
// never JWT claims. Each would leak identifiers or PII that a handler's
// own slog line is the right place for.
//
// A fork that later adds `"query", c.Request.URL.RawQuery` to the log
// attrs trips this test.
func Test_requestLog_OmitsQueryStringAndClaims(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(btp.RequestID(), requestLog(logger))
	// Pretend an authenticator upstream has dropped claims in the
	// context — the generic access log must not read or emit them.
	r.Use(func(c *gin.Context) {
		c.Set("jwtClaims", map[string]any{
			"user_name": "alice@example.invalid",
			"email":     "alice@example.invalid",
		})
		c.Next()
	})
	r.GET("/api/thing", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	// Deliberately messy query string — must not land in the log line.
	req := httptest.NewRequest(http.MethodGet,
		"/api/thing?secret=leak&owner=alice@example.invalid", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))

	// Parse the captured JSON log line.
	var entry map[string]any
	then.AssertThat(t, json.Unmarshal(buf.Bytes(), &entry), is.Nil())

	// Assert the full buffer does not contain the forbidden values.
	// Even if a future dev adds a new attr with a different key name,
	// a grep over the line must not turn up the secret or the email.
	raw := buf.String()
	then.AssertThat(t, strings.Contains(raw, "secret"), is.False())
	then.AssertThat(t, strings.Contains(raw, "leak"), is.False())
	then.AssertThat(t, strings.Contains(raw, "alice@example.invalid"), is.False())
	then.AssertThat(t, strings.Contains(raw, "jwtClaims"), is.False())
	// And pin the fields we DO expect.
	method, _ := entry["method"].(string)
	path, _ := entry["path"].(string)
	then.AssertThat(t, method, is.EqualTo("GET"))
	then.AssertThat(t, path, is.EqualTo("/api/thing"))
	// Request ID was generated by the middleware; just assert non-empty.
	rid, _ := entry["request_id"].(string)
	then.AssertThat(t, rid != "", is.True())
}
