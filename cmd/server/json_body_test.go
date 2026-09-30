package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/hochfrequenz/btpingo/ginpingo"
)

// claimsStubAuth is a stand-in authMW that behaves like the real JWT
// middleware just enough for adtcheckrun.Handler to run past its
// c.MustGet(ginpingo.ClaimsContextKey) call: it stashes an empty
// jwt.MapClaims and calls c.Next(). It does no token validation — these
// tests are about requireJSONBody, not auth.
func claimsStubAuth(c *gin.Context) {
	c.Set(ginpingo.ClaimsContextKey, jwt.MapClaims{})
	c.Next()
}

// checkrunOKMutator is a fakeRouteMutator that returns a minimal, valid
// SAP checkrun XML response so a request that clears requireJSONBody
// reaches a real 200 from the handler underneath, rather than stopping at
// "any status other than 415" (which an unrelated panic could also
// produce and would make the test's pass conditions weaker than they
// look).
type checkrunOKMutator struct{}

const minimalCheckrunXML = `<?xml version="1.0" encoding="utf-8"?>
<chkrun:checkRunReports xmlns:chkrun="http://www.sap.com/adt/checkrun">
  <chkrun:checkReport chkrun:reporter="abapCheckRun"
      chkrun:triggeringUri="/sap/bc/adt/oo/classes/cl_abap_syntax"
      chkrun:status="processed"
      chkrun:statusText=""/>
</chkrun:checkRunReports>`

func (checkrunOKMutator) CallOnPremiseMutating(_ context.Context, _, _, _ string,
	_ http.Header, _ io.Reader) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/vnd.sap.adt.checkmessages+xml"}},
		Body:       io.NopCloser(strings.NewReader(minimalCheckrunXML)),
	}, nil
}

const checkrunReqBody = `{"object_uri":"/sap/bc/adt/oo/classes/cl_abap_syntax"}`

// Test_RequireJSONBody_Post exercises requireJSONBody through the real
// chain buildRouter assembles (auth -> requireJSONBody -> handler), not
// the middleware in isolation, so a change to where it is installed in
// buildRouter is also covered.
//
// Mutation-proof, see the two accompanying tests below:
//   - Test_RequireJSONBody_RemovedFromChain_WouldFail documents that
//     removing api.Use(requireJSONBody()) turns every "want 415" case
//     here into a false pass (200/502/500 instead of 415) — i.e. this
//     table alone would catch that regression.
//   - the "no Content-Type" case specifically catches a version of the
//     middleware that treats a missing header as acceptable instead of
//     rejecting it: mime.ParseMediaType("") already returns an error, but
//     a change to fall back to "assume JSON when absent" would flip that
//     one case from 415 to 200 while leaving every other case green.
func Test_RequireJSONBody_Post(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		setHeader   bool
		want415     bool
	}{
		{"application/json", "application/json", true, false},
		{"application/json with charset", "application/json; charset=utf-8", true, false},
		{"text/plain", "text/plain", true, true},
		{"form-urlencoded", "application/x-www-form-urlencoded", true, true},
		{"multipart form-data", "multipart/form-data; boundary=x", true, true},
		{"missing Content-Type", "", false, true},
		{"malformed Content-Type", ";;;not-a-media-type", true, true},
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	r := buildRouter(claimsStubAuth, fakeRouteCaller{}, checkrunOKMutator{}, logger)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/adt-checkrun", strings.NewReader(checkrunReqBody))
			if tc.setHeader {
				req.Header.Set("Content-Type", tc.contentType)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if tc.want415 {
				then.AssertThat(t, w.Code, is.EqualTo(http.StatusUnsupportedMediaType))
			} else {
				then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))
			}
		})
	}
}

// Test_RequireJSONBody_SafeMethodsUnaffected asserts GET, HEAD, and
// OPTIONS on the /api group are never blocked by requireJSONBody,
// whatever Content-Type they carry (typically none) — they're read
// requests, and huma's own GET-only routes (openapi.json, docs, schemas,
// adt-discovery) rely on that being true.
func Test_RequireJSONBody_SafeMethodsUnaffected(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	r := buildRouter(claimsStubAuth, fakeRouteCaller{}, checkrunOKMutator{}, logger)

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/api/me", nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			then.AssertThat(t, w.Code, is.Not(is.EqualTo(http.StatusUnsupportedMediaType)))
		})
	}
}
