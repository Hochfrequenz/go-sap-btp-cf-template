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
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/hochfrequenz/go-sap-btp-cf-template/examples/adtdiscovery"
	"github.com/hochfrequenz/go-sap-btp-cf-template/internal/btp"
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

func stubJWTClaims(claims jwt.MapClaims) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("jwtClaims", claims)
		c.Next()
	}
}

func newRouter(fake *fakeCaller) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(stubJWTClaims(jwt.MapClaims{"user_name": "test-user@example"}))
	r.GET("/adt-discovery", adtdiscovery.Handler(fake))
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
	then.AssertThat(t, w.Header().Get("Content-Type"),
		is.EqualTo("application/json; charset=utf-8"))
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

func Test_Handler_SurfacesUpstreamErrorAs502(t *testing.T) {
	fake := &fakeCaller{err: errors.New("on-prem system unreachable")}
	r := newRouter(fake)

	req := httptest.NewRequest(http.MethodGet, "/adt-discovery", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusBadGateway))
	var env btp.ErrorEnvelope
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Error.Code, is.EqualTo(btp.CodeUpstreamUnreachable))
	// No leakage of Go error text.
	then.AssertThat(t,
		strings.Contains(w.Body.String(), "on-prem system unreachable"), is.False())
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
	var env btp.ErrorEnvelope
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Error.Code, is.EqualTo(btp.CodeUpstreamUnreachable))
}
