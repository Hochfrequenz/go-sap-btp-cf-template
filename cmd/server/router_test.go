package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// fakeGuard satisfies the unexported routeGuard interface for tests.
// Its middleware is a pass-through — the route-table test never sends a
// request through it; it only needs a value that lets buildRouter
// type-check, without the live JWKS fetch NewJWTValidator performs.
type fakeGuard struct{}

func (fakeGuard) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) { c.Next() }
}

// fakeRouteCaller is a one-method fake satisfying btp.OnPremCaller, in the
// shape of examples/adtdiscovery/handler_test.go's fakeCaller. The
// route-table test never invokes it; buildRouter just needs a value.
type fakeRouteCaller struct{}

func (fakeRouteCaller) CallOnPremise(_ context.Context, _, _, _ string,
	_ http.Header, _ io.Reader) (*http.Response, error) {
	return nil, nil
}

// fakeRouteMutator is a one-method fake satisfying btp.OnPremMutator, in the
// shape of examples/adtcheckrun/handler_test.go's fakeMutator. Unused at
// call time; buildRouter just needs a value.
type fakeRouteMutator struct{}

func (fakeRouteMutator) CallOnPremiseMutating(_ context.Context, _, _, _ string,
	_ http.Header, _ io.Reader) (*http.Response, error) {
	return nil, nil
}

// Test_RouterAllowList pins the exact set of routes buildRouter mounts.
// It walks the REAL gin route table (r.Routes()), not the OpenAPI document —
// POST /api/adt-checkrun is gin-registered and appears in no OpenAPI spec,
// so an OpenAPI-only assertion would be blind to it. It covers the root
// router too: a demo mounted on the root sits outside the JWT middleware
// entirely, the worse case.
//
// A fork that consciously mounts a new route adds it to want and the gate
// passes again. A route that lands without a want entry fails the build —
// the surprise #125 is about.
func Test_RouterAllowList(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	r := buildRouter(fakeGuard{}, fakeRouteCaller{}, fakeRouteMutator{}, logger)

	got := make(map[string]bool) // "METHOD /path" -> true
	for _, ri := range r.Routes() {
		got[ri.Method+" "+ri.Path] = true
	}

	// want is pinned from the first run of r.Routes() output. The
	// huma-generated block (four OpenAPI-spec variants + /docs +
	// /schemas/:schema) is captured wholesale rather than hand-guessed,
	// because huma v2.38.0 registers two /api/openapi-3.0.* downgrade
	// routes that a hand-list is easy to miss.
	want := map[string]bool{
		"GET /healthz":           true,
		"GET /version":           true,
		"GET /api/me":            true,
		"GET /api/adt-discovery": true,
		"POST /api/adt-checkrun": true,
		// --- huma-generated block: replace with first-run output ---
		"GET /api/openapi.json":     true,
		"GET /api/openapi-3.0.json": true,
		"GET /api/openapi.yaml":     true,
		"GET /api/openapi-3.0.yaml": true,
		"GET /api/docs":             true,
		"GET /api/schemas/:schema":  true,
	}

	missing := diff(want, got)
	unexpected := diff(got, want)

	if len(missing) > 0 || len(unexpected) > 0 {
		sort.Strings(missing)
		sort.Strings(unexpected)
		t.Errorf("route table drifted from allow-list\n"+
			"  missing (in want, not in router):\n    %s\n"+
			"  unexpected (in router, not in want):\n    %s",
			strings.Join(missing, "\n    "),
			strings.Join(unexpected, "\n    "))
	}
}

// diff returns the keys in a that are not in b.
func diff(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	return out
}
