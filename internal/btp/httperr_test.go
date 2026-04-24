package btp_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"
	"github.com/gin-gonic/gin"

	"github.com/hochfrequenz/go-sap-btp-cf-template/internal/btp"
)

// Test_AbortError_WritesEnvelopeAndLeaksNothing is the core contract:
// the response body is the typed envelope, the underlying Go error is
// not in the body, and the handler chain is aborted.
func Test_AbortError_WritesEnvelopeAndLeaksNothing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/boom", func(c *gin.Context) {
		btp.AbortError(c, http.StatusBadGateway, btp.CodeUpstreamUnreachable,
			"on-premise call failed",
			errors.New("sensitive: sap-host unreachable; internal token=abcd1234"))
	})
	r.GET("/never", func(c *gin.Context) {
		// If AbortError did not call Abort, this would run after /boom's
		// middleware-style invocation — asserting it isn't is a job for
		// a separate middleware chain test. Kept as a safety net only.
		t.Fatal("should never run")
	})

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusBadGateway))
	then.AssertThat(t, w.Header().Get("Content-Type"),
		is.EqualTo("application/json; charset=utf-8"))

	var env btp.ErrorEnvelope
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Error.Code, is.EqualTo(btp.CodeUpstreamUnreachable))
	then.AssertThat(t, env.Error.Message, is.EqualTo("on-premise call failed"))
	// No request ID middleware installed, so the field is omitted.
	then.AssertThat(t, env.Error.RequestID, is.EqualTo(""))
	// The crucial assertion: the underlying err.Error() must not be
	// anywhere in the response body.
	then.AssertThat(t, strings.Contains(w.Body.String(), "sensitive"),
		is.False())
	then.AssertThat(t, strings.Contains(w.Body.String(), "abcd1234"),
		is.False())
}

// Test_AbortError_IncludesRequestIDWhenSet mirrors how the future
// RequestID middleware will populate c.Set("request_id", ...). Pinning
// the behaviour here means the middleware PR doesn't need to touch
// AbortError to get request-ID propagation.
func Test_AbortError_IncludesRequestIDWhenSet(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("request_id", "rid-42"); c.Next() })
	r.GET("/boom", func(c *gin.Context) {
		btp.AbortError(c, http.StatusUnauthorized, btp.CodeUnauthorized,
			"missing bearer token", nil)
	})

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var env btp.ErrorEnvelope
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Error.RequestID, is.EqualTo("rid-42"))
}
