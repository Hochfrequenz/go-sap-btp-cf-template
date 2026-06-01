package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"
	"github.com/gin-gonic/gin"
)

func Test_versionHandler_ReturnsAllFields(t *testing.T) {
	t.Cleanup(func() { version = defaultVersion; commit = defaultCommit; branch = defaultBranch; buildDate = defaultBuildDate })
	version = "v1.2.3"
	commit = "abc1234"
	branch = "main"
	buildDate = "2026-06-01T00:00:00Z"

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/version", versionHandler())

	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))

	var body map[string]string
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &body), is.Nil())
	then.AssertThat(t, body["version"], is.EqualTo("v1.2.3"))
	then.AssertThat(t, body["commit"], is.EqualTo("abc1234"))
	then.AssertThat(t, body["branch"], is.EqualTo("main"))
	then.AssertThat(t, body["build_date"], is.EqualTo("2026-06-01T00:00:00Z"))
}

func Test_versionHandler_DefaultsWithoutLdflags(t *testing.T) {
	// Pins the fallback strings that a plain `go build` (no -ldflags) produces.
	// Any change to the defaults in version.go trips this test.
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/version", versionHandler())

	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))

	var body map[string]string
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &body), is.Nil())
	then.AssertThat(t, body["version"], is.EqualTo(defaultVersion))
	then.AssertThat(t, body["commit"], is.EqualTo(defaultCommit))
	then.AssertThat(t, body["branch"], is.EqualTo(defaultBranch))
	then.AssertThat(t, body["build_date"], is.EqualTo(defaultBuildDate))
}
