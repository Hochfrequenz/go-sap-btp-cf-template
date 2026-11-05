package adtdiscovery_test

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
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humagin"
	"github.com/gin-gonic/gin"

	"github.com/hochfrequenz/btpingo"
	"github.com/hochfrequenz/btpingo/btpingotest"
	"github.com/hochfrequenz/btpingo/ginpingo"

	"github.com/hochfrequenz/go-sap-btp-cf-template/examples/adtdiscovery"
)

// fakeCaller mirrors the OnPremCaller pattern used elsewhere in
// examples/: one-method fake, no XSUAA / Destination / CC stubbing.
type fakeCaller struct {
	gotDest, gotMethod, gotPath string
	resp                        *http.Response
	err                         error
}

func (f *fakeCaller) CallOnPremise(_ context.Context, dest, method, path string,
	_ http.Header, _ io.Reader) (*http.Response, error) {
	f.gotDest, f.gotMethod, f.gotPath = dest, method, path
	return f.resp, f.err
}

// newRouter builds a minimal gin engine + huma.API mirroring the
// production wiring (humagin.NewWithGroup on a router group). Tests
// then httptest as before; the path matches whatever the group's
// prefix is. Empty prefix in tests so /adt-discovery is the path.
func newRouter(fake *fakeCaller) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("")
	hapi := humagin.NewWithGroup(r, api,
		huma.DefaultConfig("test", "0.0.0"))
	adtdiscovery.Register(hapi, fake)
	return r
}

// Minimal valid ATOM service doc — two workspaces, three collections.
// Namespace prefixes are the real SAP ones but local-name matching
// means the test doesn't need to declare them in every attr.
const sapDiscoveryXML = `<?xml version="1.0" encoding="utf-8"?>
<app:service xmlns:app="http://www.w3.org/2007/app" xmlns:atom="http://www.w3.org/2005/Atom">
  <app:workspace>
    <atom:title>Core</atom:title>
    <app:collection href="/sap/bc/adt/core/unit/runs">
      <atom:title>Unit Test Runs</atom:title>
    </app:collection>
    <app:collection href="/sap/bc/adt/checkruns">
      <atom:title>Check Runs</atom:title>
    </app:collection>
  </app:workspace>
  <app:workspace>
    <atom:title>Repository</atom:title>
    <app:collection href="/sap/bc/adt/repository/informationsystem/search">
      <atom:title>Search</atom:title>
    </app:collection>
  </app:workspace>
</app:service>`

func Test_Handler_ReturnsTypedJSONFromSAPXML(t *testing.T) {
	fake := &fakeCaller{
		resp: &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/atomsvc+xml"}},
			Body:          io.NopCloser(strings.NewReader(sapDiscoveryXML)),
			ContentLength: -1,
		},
	}
	r := newRouter(fake)

	req := httptest.NewRequest(http.MethodGet, "/adt-discovery", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))
	// huma serves successful responses as application/json.
	then.AssertThat(t,
		strings.Contains(w.Header().Get("Content-Type"), "application/json"),
		is.True())
	// No XML leak at the boundary.
	then.AssertThat(t, strings.Contains(w.Body.String(), "<?xml"), is.False())
	then.AssertThat(t, strings.Contains(w.Body.String(), "<app:"), is.False())

	var resp adtdiscovery.Response
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &resp), is.Nil())
	then.AssertThat(t, len(resp.Workspaces), is.EqualTo(2))

	// Workspace 0: Core with two collections.
	then.AssertThat(t, resp.Workspaces[0].Title, is.EqualTo("Core"))
	then.AssertThat(t, len(resp.Workspaces[0].Collections), is.EqualTo(2))
	then.AssertThat(t, resp.Workspaces[0].Collections[0].Title, is.EqualTo("Unit Test Runs"))
	then.AssertThat(t, resp.Workspaces[0].Collections[0].Href,
		is.EqualTo("/sap/bc/adt/core/unit/runs"))
	then.AssertThat(t, resp.Workspaces[0].Collections[1].Href,
		is.EqualTo("/sap/bc/adt/checkruns"))

	// Workspace 1: Repository with one collection.
	then.AssertThat(t, resp.Workspaces[1].Title, is.EqualTo("Repository"))
	then.AssertThat(t, len(resp.Workspaces[1].Collections), is.EqualTo(1))
}

func Test_Handler_CallsCorrectSAPPath(t *testing.T) {
	fake := &fakeCaller{
		resp: &http.Response{
			StatusCode:    http.StatusOK,
			Body:          io.NopCloser(strings.NewReader(sapDiscoveryXML)),
			ContentLength: -1,
		},
	}
	r := newRouter(fake)

	req := httptest.NewRequest(http.MethodGet, "/adt-discovery", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))
	then.AssertThat(t, fake.gotDest, is.EqualTo("HF_S4"))
	then.AssertThat(t, fake.gotMethod, is.EqualTo(http.MethodGet))
	then.AssertThat(t, fake.gotPath, is.EqualTo("/sap/bc/adt/discovery"))
}

// Test_Handler_SurfacesUpstreamErrorAs502 pins the upstream-failure
// path. huma renders errors as the RFC 7807 problem-details model
// (Title, Status, Detail) — different shape from the gin-style
// btpingo.ErrorEnvelope used elsewhere in the template. The status is
// what callers switch on; the detail carries the user-safe message
// classified by btpingo.ClassifyOnPremError.
func Test_Handler_SurfacesUpstreamErrorAs502(t *testing.T) {
	fake := &fakeCaller{err: errors.New("on-prem system unreachable")}
	r := newRouter(fake)

	req := httptest.NewRequest(http.MethodGet, "/adt-discovery", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusBadGateway))
	var env huma.ErrorModel
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Status, is.EqualTo(http.StatusBadGateway))
	// errors.New(...) has no recognised sentinel → transport-error branch.
	then.AssertThat(t, env.Detail, is.EqualTo("on-premise transport error"))
	// No leakage of Go error text — the underlying err is captured
	// for operator side, not surfaced in the envelope.
	then.AssertThat(t,
		strings.Contains(w.Body.String(), "on-prem system unreachable"), is.False())
}

// Test_Handler_ClassifiesDestinationNotFoundAs502 pins that the
// classifier's destination-not-found branch reaches the wire — proves
// the handler delegates to btpingo.ClassifyOnPremError rather than emitting
// a single constant detail for every error.
func Test_Handler_ClassifiesDestinationNotFoundAs502(t *testing.T) {
	fake := &fakeCaller{err: btpingo.ErrDestinationNotFound}
	r := newRouter(fake)

	req := httptest.NewRequest(http.MethodGet, "/adt-discovery", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusBadGateway))
	var env huma.ErrorModel
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Detail, is.EqualTo("destination not found"))
}

func Test_Handler_SurfacesMalformedXMLAs502(t *testing.T) {
	fake := &fakeCaller{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`<html>maintenance</html>`)),
		},
	}
	r := newRouter(fake)

	req := httptest.NewRequest(http.MethodGet, "/adt-discovery", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusBadGateway))
	var env huma.ErrorModel
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Status, is.EqualTo(http.StatusBadGateway))
}

func Test_Handler_SurfacesNon2xxAs502(t *testing.T) {
	fake := &fakeCaller{
		resp: &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       io.NopCloser(strings.NewReader("")),
		},
	}
	r := newRouter(fake)

	req := httptest.NewRequest(http.MethodGet, "/adt-discovery", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusBadGateway))
	var env huma.ErrorModel
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Status, is.EqualTo(http.StatusBadGateway))
	// SAP HTTP status surfaces in the detail — the diagnosability
	// fix from issue #68 motivated by PR #67's 400-from-SAP debug pain.
	then.AssertThat(t, env.Detail, is.EqualTo("on-premise system returned HTTP 500"))
}

// errReader returns a controlled error from Read so the io.ReadAll
// failure branch in the handler is exercised.
type errReader struct{ err error }

func (e errReader) Read(_ []byte) (int, error) { return 0, e.err }

func Test_Handler_SurfacesBodyReadErrorAs502(t *testing.T) {
	fake := &fakeCaller{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(errReader{err: errors.New("read failed")}),
		},
	}
	r := newRouter(fake)

	req := httptest.NewRequest(http.MethodGet, "/adt-discovery", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusBadGateway))
	var env huma.ErrorModel
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Status, is.EqualTo(http.StatusBadGateway))
}

// Test_Register_AppearsInOpenAPISpec pins that registering the handler
// produces an OpenAPI operation in the auto-generated /openapi.json.
// A future regression that drops huma.Register or breaks the input/
// output type discovery would silently disappear from the spec — this
// test catches that.
func Test_Register_AppearsInOpenAPISpec(t *testing.T) {
	fake := &fakeCaller{}
	r := newRouter(fake)

	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))
	// Crude shape-check: the spec must mention our path AND the
	// derived operation ID. Full schema validation is huma's
	// concern, not ours.
	body := w.Body.String()
	then.AssertThat(t, strings.Contains(body, "/adt-discovery"), is.True())
	then.AssertThat(t, strings.Contains(body, "adt-discovery"), is.True())
}

// newRouterFor is newRouter for any OnPremCaller (here a HandlerCaller).
func newRouterFor(caller btpingo.OnPremCaller) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	adtdiscovery.Register(humagin.NewWithGroup(r, r.Group(""), huma.DefaultConfig("test", "0.0.0")), caller)
	return r
}

// sapDiscoveryServer is a fake SAP: it answers 401 on the first request (as
// an ICF does when a session has expired) and serves the discovery document
// from then on. It also records what reached it.
func sapDiscoveryServer(hits *int, gotAccept *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		*gotAccept = r.Header.Get("Accept")
		if *hits == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/atomsvc+xml")
		_, _ = io.WriteString(w, sapDiscoveryXML)
	})
}

// Test_Handler_ViaHandlerCaller drives the handler through
// btpingotest.HandlerCaller instead of a one-method fake. HandlerCaller
// serves a fake SAP (an http.Handler) over a loopback port and shares
// btpingo's real Service code paths: header filtering, the one 401 retry,
// the response size limit, the redirect policy and the CSRF handshake. Use it
// when the handler's behaviour depends on those; the one-method fakes above
// stay the default for plain handler logic.
func Test_Handler_ViaHandlerCaller(t *testing.T) {
	t.Run("401 is retried once and the typed JSON comes out", func(t *testing.T) {
		var hits int
		var gotAccept string
		// "HF_S4" is rewritten by apply-config like the handler's literal.
		caller, err := btpingotest.NewHandlerCaller(sapDiscoveryServer(&hits, &gotAccept),
			btpingotest.WithDestinationName("HF_S4"))
		then.AssertThat(t, err, is.Nil())
		t.Cleanup(caller.Close)

		rec := httptest.NewRecorder()
		newRouterFor(caller).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/adt-discovery", nil))

		then.AssertThat(t, rec.Code, is.EqualTo(http.StatusOK))
		then.AssertThat(t, hits, is.EqualTo(2))
		then.AssertThat(t, gotAccept, is.EqualTo("application/atomsvc+xml"))
		var got adtdiscovery.Response
		then.AssertThat(t, json.Unmarshal(rec.Body.Bytes(), &got), is.Nil())
		then.AssertThat(t, len(got.Workspaces), is.EqualTo(2))
	})

	t.Run("a response over the size limit is a 502", func(t *testing.T) {
		hits, gotAccept := 1, "" // hits=1 skips the helper's 401
		caller, err := btpingotest.NewHandlerCaller(sapDiscoveryServer(&hits, &gotAccept),
			btpingotest.WithDestinationName("HF_S4"),
			btpingotest.WithResponseSizeLimit(64))
		then.AssertThat(t, err, is.Nil())
		t.Cleanup(caller.Close)

		rec := httptest.NewRecorder()
		newRouterFor(caller).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/adt-discovery", nil))

		then.AssertThat(t, rec.Code, is.EqualTo(http.StatusBadGateway))
	})
}

// Test_Router_WithMockXSUAA shows the mock XSUAA next to HandlerCaller: the
// production auth middleware (ginpingo.JWT with a real btpingo.JWTValidator)
// validates a token the mock issued, in front of the handler.
func Test_Router_WithMockXSUAA(t *testing.T) {
	const clientID = "sb-demo!t1"
	xsuaa, err := btpingotest.NewXSUAA(btpingotest.XSUAAConfig{
		XSAppName:   "demo!t1",
		AppClientID: clientID,
		Clients: []btpingotest.XSUAAClient{
			{ClientID: clientID, Secret: "secret", Scopes: []string{"demo!t1.User"}},
		},
	})
	then.AssertThat(t, err, is.Nil())
	srv := httptest.NewServer(xsuaa)
	t.Cleanup(srv.Close)
	validator, err := btpingo.NewJWTValidator(context.Background(), xsuaa.Credentials(srv.URL))
	then.AssertThat(t, err, is.Nil())

	hits, gotAccept := 1, "" // hits=1 skips the helper's 401
	caller, err := btpingotest.NewHandlerCaller(sapDiscoveryServer(&hits, &gotAccept),
		btpingotest.WithDestinationName("HF_S4"))
	then.AssertThat(t, err, is.Nil())
	t.Cleanup(caller.Close)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	api.Use(ginpingo.JWT(validator))
	adtdiscovery.Register(humagin.NewWithGroup(r, api, huma.DefaultConfig("test", "0.0.0")), caller)

	noToken := httptest.NewRecorder()
	r.ServeHTTP(noToken, httptest.NewRequest(http.MethodGet, "/api/adt-discovery", nil))
	then.AssertThat(t, noToken.Code, is.EqualTo(http.StatusUnauthorized))

	token, err := xsuaa.MintToken(clientID)
	then.AssertThat(t, err, is.Nil())
	req := httptest.NewRequest(http.MethodGet, "/api/adt-discovery", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	withToken := httptest.NewRecorder()
	r.ServeHTTP(withToken, req)
	then.AssertThat(t, withToken.Code, is.EqualTo(http.StatusOK))
}
