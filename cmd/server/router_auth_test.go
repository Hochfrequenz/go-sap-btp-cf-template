package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// publicRoutes are the routes buildRouter intentionally mounts OUTSIDE the
// JWT-guarded /api group. Anything not in this set MUST go through authMW —
// if a demo route (or any future route) lands outside /api, it is reachable
// without a token, the exact class of bug #125 is about.
//
// Keep this list in sync with buildRouter in main.go: GET /healthz and
// GET /version are the only two routes registered on r directly, before the
// api := r.Group("/api") + api.Use(authMW) line.
var publicRoutes = map[string]bool{
	"GET /healthz": true,
	"GET /version": true,
}

// Test_RouterAllowList_AuthGated asserts every route buildRouter mounts,
// other than the public probes above, actually passes through the auth
// middleware. It builds the router with a middleware that unconditionally
// aborts with 418 (never c.Next()s into the real handler) and asserts every
// non-public route answers 418 — i.e. the request was intercepted by authMW,
// not served by the handler underneath.
//
// This is a different failure mode than Test_RouterAllowList: that test
// catches a route landing at an unexpected PATH; this one catches a route
// landing at an EXPECTED path but outside the authenticated group (e.g. a
// demo mounted on r instead of api).
func Test_RouterAllowList_AuthGated(t *testing.T) {
	teapot := func(c *gin.Context) { c.AbortWithStatus(http.StatusTeapot) }
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	r := buildRouter(teapot, fakeRouteCaller{}, fakeRouteMutator{}, logger)

	for _, ri := range r.Routes() {
		key := ri.Method + " " + ri.Path
		if publicRoutes[key] {
			continue
		}
		t.Run(key, func(t *testing.T) {
			path := routeParamValues(ri.Path)
			req := httptest.NewRequest(ri.Method, path, nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusTeapot {
				t.Errorf("%s: got status %d, want %d (authMW did not run — route escaped the JWT-guarded group)",
					key, rec.Code, http.StatusTeapot)
			}
		})
	}
}

// routeParamValues substitutes a concrete value for every gin path
// parameter (":schema" etc.) so httptest.NewRequest gets a routable
// concrete path rather than the literal gin pattern.
func routeParamValues(pattern string) string {
	segments := strings.Split(pattern, "/")
	for i, seg := range segments {
		if strings.HasPrefix(seg, ":") {
			segments[i] = "placeholder"
		}
	}
	return strings.Join(segments, "/")
}
