package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"
	"github.com/gin-gonic/gin"

	"github.com/hochfrequenz/btpingo"
)

// Test_RequireJSONBody_Methods runs requireJSONBody on a bare engine that
// registers every method, so HEAD/OPTIONS/DELETE actually reach the
// middleware (buildRouter has no such routes; gin 404s before group
// middleware runs).
func Test_RequireJSONBody_Methods(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(requireJSONBody())
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions,
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		r.Handle(m, "/x", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	}

	cases := []struct {
		name, method, contentType, body string
		want                            int
	}{
		{"GET text/plain body", http.MethodGet, "text/plain", "x", http.StatusNoContent},
		{"HEAD text/plain body", http.MethodHead, "text/plain", "x", http.StatusNoContent},
		{"OPTIONS text/plain body", http.MethodOptions, "text/plain", "x", http.StatusNoContent},
		{"DELETE no body no CT", http.MethodDelete, "", "", http.StatusNoContent},
		{"DELETE text/plain body", http.MethodDelete, "text/plain", "x", http.StatusUnsupportedMediaType},
		{"PUT text/plain body", http.MethodPut, "text/plain", "x", http.StatusUnsupportedMediaType},
		{"POST no body no CT", http.MethodPost, "", "", http.StatusUnsupportedMediaType},
		{"POST upper-case type", http.MethodPost, "Application/JSON", "{}", http.StatusNoContent},
		{"POST trailing semicolon", http.MethodPost, "application/json;", "{}", http.StatusNoContent},
		{"PATCH merge-patch+json", http.MethodPatch, "application/merge-patch+json", "{}", http.StatusNoContent},
		{"POST text/plus+json is not application/*", http.MethodPost, "text/x+json", "{}", http.StatusUnsupportedMediaType},
	}
	// Unknown length (ContentLength -1, as for a chunked body) counts as a
	// body: io.MultiReader hides the length from httptest.NewRequest.
	t.Run("DELETE unknown-length body no CT", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/x", io.MultiReader(strings.NewReader("x")))
		then.AssertThat(t, req.ContentLength, is.EqualTo(int64(-1)))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		then.AssertThat(t, w.Code, is.EqualTo(http.StatusUnsupportedMediaType))
	})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body == "" {
				req = httptest.NewRequest(tc.method, "/x", nil)
			} else {
				req = httptest.NewRequest(tc.method, "/x", strings.NewReader(tc.body))
			}
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			then.AssertThat(t, w.Code, is.EqualTo(tc.want))
			if tc.want == http.StatusUnsupportedMediaType {
				var env btpingo.ErrorEnvelope
				then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
				then.AssertThat(t, env.Error.Code, is.EqualTo(btpingo.CodeInvalidRequest))
			}
		})
	}
}

// Test_RequireJSONBody_SiblingGroupBypasses proves the README's escape
// hatch actually works with gin: a route that must accept a non-JSON body
// (e.g. a fork's ginpingo.ProxyHandler forwarding XML) is registered on a
// second r.Group("/api", authMW) that never calls requireJSONBody, sharing
// the "/api" prefix with the strict group instead of replacing it.
func Test_RequireJSONBody_SiblingGroupBypasses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	authMW := func(c *gin.Context) { c.Next() }

	strict := r.Group("/api")
	strict.Use(authMW, requireJSONBody())
	strict.POST("/strict", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	raw := r.Group("/api", authMW)
	raw.POST("/raw", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	strictReq := httptest.NewRequest(http.MethodPost, "/api/strict", strings.NewReader("<x/>"))
	strictReq.Header.Set("Content-Type", "text/xml")
	strictW := httptest.NewRecorder()
	r.ServeHTTP(strictW, strictReq)
	then.AssertThat(t, strictW.Code, is.EqualTo(http.StatusUnsupportedMediaType))

	rawReq := httptest.NewRequest(http.MethodPost, "/api/raw", strings.NewReader("<x/>"))
	rawReq.Header.Set("Content-Type", "text/xml")
	rawW := httptest.NewRecorder()
	r.ServeHTTP(rawW, rawReq)
	then.AssertThat(t, rawW.Code, is.EqualTo(http.StatusNoContent))
}
