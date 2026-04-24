package adtcheckrun_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/hochfrequenz/go-sap-btp-cf-template/examples/adtcheckrun"
	"github.com/hochfrequenz/go-sap-btp-cf-template/internal/btp"
)

// fakeMutator is the canonical handler-test double for CSRF-flavoured
// endpoints: a one-method fake satisfying btp.OnPremMutator. It
// records the arguments the handler passed (so the test can assert
// request shape) and returns a canned response (so the test can
// assert response translation).
//
// It does NOT model the CSRF handshake — that's the Service's
// concern, fully tested in internal/btp/service_csrf_test.go. The
// handler only needs to prove it calls the mutator with the right
// shape; a fake that pretends the handshake already happened is the
// simplest thing that works.
type fakeMutator struct {
	// captured inputs
	gotDest    string
	gotMethod  string
	gotPath    string
	gotBody    []byte
	gotHeaders http.Header

	// canned outputs
	resp *http.Response
	err  error
}

func (f *fakeMutator) CallOnPremiseMutating(_ context.Context, dest, method, path string,
	headers http.Header, body io.Reader) (*http.Response, error) {
	f.gotDest, f.gotMethod, f.gotPath = dest, method, path
	f.gotHeaders = headers.Clone()
	if body != nil {
		f.gotBody, _ = io.ReadAll(body)
	}
	return f.resp, f.err
}

func stubJWTClaims(claims jwt.MapClaims) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("jwtClaims", claims)
		c.Next()
	}
}

func newRouter(fake *fakeMutator) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(stubJWTClaims(jwt.MapClaims{"user_name": "test-user@example"}))
	r.POST("/adt-checkrun", adtcheckrun.Handler(fake))
	return r
}

func Test_Handler_PostsCorrectADTPayload(t *testing.T) {
	fake := &fakeMutator{
		resp: &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/vnd.sap.adt.checkmessages+xml"}},
			Body:          io.NopCloser(strings.NewReader(`<?xml version="1.0"?><messages/>`)),
			ContentLength: -1,
		},
	}
	r := newRouter(fake)

	body := `{"object_uri":"/sap/bc/adt/programs/programs/zmy_program/source/main"}`
	req := httptest.NewRequest(http.MethodPost, "/adt-checkrun", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))
	then.AssertThat(t, strings.Contains(w.Body.String(), "<messages/>"), is.True())

	// The handler hits /sap/bc/adt/checkruns via the mutator.
	then.AssertThat(t, fake.gotDest, is.EqualTo("HF_S4"))
	then.AssertThat(t, fake.gotMethod, is.EqualTo(http.MethodPost))
	then.AssertThat(t, fake.gotPath, is.EqualTo("/sap/bc/adt/checkruns"))
	// Content-Type must be the ADT check-object type — ABAP's XML
	// parser rejects text/xml here.
	then.AssertThat(t, fake.gotHeaders.Get("Content-Type"),
		is.EqualTo("application/vnd.sap.adt.checkobjects+xml"))
	// Body carries the requested ObjectURI verbatim — the rest of
	// the XML is scaffolding.
	then.AssertThat(t, strings.Contains(string(fake.gotBody),
		"/sap/bc/adt/programs/programs/zmy_program/source/main"), is.True())
}

func Test_Handler_RejectsNonADTObjectURI(t *testing.T) {
	// The object_uri validator tag requires the path to start with
	// /sap/bc/adt/. Anything else — a typo, a REST path, an empty
	// string — must 400 before svc.CallOnPremiseMutating is touched.
	fake := &fakeMutator{}
	r := newRouter(fake)

	body := `{"object_uri":"/sap/bc/rest/not_adt"}`
	req := httptest.NewRequest(http.MethodPost, "/adt-checkrun", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusBadRequest))
	var env btp.ErrorEnvelope
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Error.Code, is.EqualTo(btp.CodeInvalidRequest))
	// Crucial: the fake was NOT called. Validation short-circuited.
	then.AssertThat(t, fake.gotDest, is.EqualTo(""))
}

func Test_Handler_SurfacesUpstreamErrorAs502(t *testing.T) {
	fake := &fakeMutator{err: errors.New("on-prem system unreachable")}
	r := newRouter(fake)

	body := `{"object_uri":"/sap/bc/adt/programs/programs/zx/source/main"}`
	req := httptest.NewRequest(http.MethodPost, "/adt-checkrun", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusBadGateway))
	var env btp.ErrorEnvelope
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Error.Code, is.EqualTo(btp.CodeUpstreamUnreachable))
	// Underlying Go error text must not leak into the response body.
	then.AssertThat(t,
		strings.Contains(w.Body.String(), "on-prem system unreachable"), is.False())
}
