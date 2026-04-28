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

// canned SAP check-run XML — what /sap/bc/adt/checkruns actually
// returns for a real class with syntax issues. Verified shape against
// srvhfuhana (see adtler's fixtures).
const sapCheckrunXMLWithIssues = `<?xml version="1.0" encoding="utf-8"?>
<chkrun:checkRunReports xmlns:chkrun="http://www.sap.com/adt/checkrun">
  <chkrun:checkReport chkrun:reporter="abapCheckRun"
      chkrun:triggeringUri="/sap/bc/adt/oo/classes/cl_broken/source/main"
      chkrun:status="processed"
      chkrun:statusText="">
    <chkrun:checkMessageList>
      <chkrun:checkMessage uri="/sap/bc/adt/oo/classes/cl_broken/source/main#start=42,5"
          type="E" shortText="Unknown class CL_DOES_NOT_EXIST"/>
      <chkrun:checkMessage uri="/sap/bc/adt/oo/classes/cl_broken/source/main#start=50,10"
          type="W" shortText="Obsolete statement"/>
    </chkrun:checkMessageList>
  </chkrun:checkReport>
</chkrun:checkRunReports>`

// Canned response for the "class does not exist" case — what we got
// in the live deploy verification. No checkMessageList.
const sapCheckrunXMLNotProcessed = `<?xml version="1.0" encoding="utf-8"?>
<chkrun:checkRunReports xmlns:chkrun="http://www.sap.com/adt/checkrun">
  <chkrun:checkReport chkrun:reporter="abapCheckRun"
      chkrun:triggeringUri="/sap/bc/adt/oo/classes/cl_abap_syntax"
      chkrun:status="notProcessed"
      chkrun:statusText="Resource CLASS CL_ABAP_SYNTAX does not exist."/>
</chkrun:checkRunReports>`

const reqBodyAbapSyntaxClass = `{"object_uri":"/sap/bc/adt/oo/classes/cl_abap_syntax"}`

func Test_Handler_ParsesXMLIntoTypedJSON(t *testing.T) {
	fake := &fakeMutator{
		resp: &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/vnd.sap.adt.checkmessages+xml"}},
			Body:          io.NopCloser(strings.NewReader(sapCheckrunXMLWithIssues)),
			ContentLength: -1,
		},
	}
	r := newRouter(fake)

	body := `{"object_uri":"/sap/bc/adt/oo/classes/cl_broken/source/main"}`
	req := httptest.NewRequest(http.MethodPost, "/adt-checkrun", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))
	// Output MUST be JSON — no XML leak at the API boundary.
	then.AssertThat(t, w.Header().Get("Content-Type"),
		is.EqualTo("application/json; charset=utf-8"))
	then.AssertThat(t, strings.Contains(w.Body.String(), "<?xml"), is.False())
	then.AssertThat(t, strings.Contains(w.Body.String(), "<chkrun:"), is.False())

	// Typed decode.
	var resp adtcheckrun.Response
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &resp), is.Nil())
	then.AssertThat(t, len(resp.Reports), is.EqualTo(1))
	then.AssertThat(t, resp.Reports[0].Reporter, is.EqualTo("abapCheckRun"))
	then.AssertThat(t, resp.Reports[0].Status, is.EqualTo("processed"))
	then.AssertThat(t, len(resp.Reports[0].Messages), is.EqualTo(2))

	// First message: error at line 42, column 5.
	m0 := resp.Reports[0].Messages[0]
	then.AssertThat(t, m0.Type, is.EqualTo("E"))
	then.AssertThat(t, m0.ShortText, is.EqualTo("Unknown class CL_DOES_NOT_EXIST"))
	then.AssertThat(t, m0.Line, is.EqualTo(42))
	then.AssertThat(t, m0.Column, is.EqualTo(5))

	// Second message: warning at 50,10.
	m1 := resp.Reports[0].Messages[1]
	then.AssertThat(t, m1.Type, is.EqualTo("W"))
	then.AssertThat(t, m1.Line, is.EqualTo(50))
}

// Test_Handler_HandlesStatusOnlyReport pins the "object not found"
// shape SAP actually returned in live deploy verification —
// checkRunReports with status=notProcessed and no message list.
func Test_Handler_HandlesStatusOnlyReport(t *testing.T) {
	fake := &fakeMutator{
		resp: &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/vnd.sap.adt.checkmessages+xml"}},
			Body:          io.NopCloser(strings.NewReader(sapCheckrunXMLNotProcessed)),
			ContentLength: -1,
		},
	}
	r := newRouter(fake)

	req := httptest.NewRequest(http.MethodPost, "/adt-checkrun", strings.NewReader(reqBodyAbapSyntaxClass))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))
	var resp adtcheckrun.Response
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &resp), is.Nil())
	then.AssertThat(t, len(resp.Reports), is.EqualTo(1))
	then.AssertThat(t, resp.Reports[0].Status, is.EqualTo("notProcessed"))
	then.AssertThat(t, resp.Reports[0].StatusText, is.EqualTo("Resource CLASS CL_ABAP_SYNTAX does not exist."))
	then.AssertThat(t, len(resp.Reports[0].Messages), is.EqualTo(0))
}

func Test_Handler_SendsCorrectADTRequest(t *testing.T) {
	fake := &fakeMutator{
		resp: &http.Response{
			StatusCode:    http.StatusOK,
			Body:          io.NopCloser(strings.NewReader(sapCheckrunXMLNotProcessed)),
			ContentLength: -1,
		},
	}
	r := newRouter(fake)

	req := httptest.NewRequest(http.MethodPost, "/adt-checkrun", strings.NewReader(reqBodyAbapSyntaxClass))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))

	// Request shape that went to SAP.
	then.AssertThat(t, fake.gotDest, is.EqualTo("HF_S4"))
	then.AssertThat(t, fake.gotMethod, is.EqualTo(http.MethodPost))
	then.AssertThat(t, fake.gotPath, is.EqualTo("/sap/bc/adt/checkruns"))
	then.AssertThat(t, fake.gotHeaders.Get("Content-Type"),
		is.EqualTo("application/vnd.sap.adt.checkobjects+xml"))
	// Request body is the internal XML wrapping the user's ObjectURI.
	then.AssertThat(t, strings.Contains(string(fake.gotBody),
		"/sap/bc/adt/oo/classes/cl_abap_syntax"), is.True())
}

func Test_Handler_RejectsNonADTObjectURI(t *testing.T) {
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
	then.AssertThat(t, fake.gotDest, is.EqualTo(""))
}

func Test_Handler_SurfacesUpstreamErrorAs502(t *testing.T) {
	fake := &fakeMutator{err: errors.New("on-prem system unreachable")}
	r := newRouter(fake)

	req := httptest.NewRequest(http.MethodPost, "/adt-checkrun", strings.NewReader(reqBodyAbapSyntaxClass))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusBadGateway))
	var env btp.ErrorEnvelope
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Error.Code, is.EqualTo(btp.CodeUpstreamUnreachable))
	then.AssertThat(t,
		strings.Contains(w.Body.String(), "on-prem system unreachable"), is.False())
}

func Test_Handler_SurfacesBadSAPResponseAs502(t *testing.T) {
	// SAP returns a 200 but the body isn't the expected XML shape —
	// maybe a maintenance page, maybe a garbled response. Handler
	// must surface a typed 502 rather than panic or echo junk.
	fake := &fakeMutator{
		resp: &http.Response{
			StatusCode:    http.StatusOK,
			Body:          io.NopCloser(strings.NewReader(`<html>SAP is down for maintenance</html>`)),
			ContentLength: -1,
		},
	}
	r := newRouter(fake)

	body := `{"object_uri":"/sap/bc/adt/oo/classes/cl_x"}`
	req := httptest.NewRequest(http.MethodPost, "/adt-checkrun", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusBadGateway))
	var env btp.ErrorEnvelope
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Error.Code, is.EqualTo(btp.CodeUpstreamUnreachable))
}

func Test_Handler_SurfacesNon2xxAs502(t *testing.T) {
	fake := &fakeMutator{
		resp: &http.Response{
			StatusCode:    http.StatusInternalServerError,
			Body:          io.NopCloser(strings.NewReader("")),
			ContentLength: -1,
		},
	}
	r := newRouter(fake)

	req := httptest.NewRequest(http.MethodPost, "/adt-checkrun", strings.NewReader(reqBodyAbapSyntaxClass))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusBadGateway))
	var env btp.ErrorEnvelope
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Error.Code, is.EqualTo(btp.CodeUpstreamUnreachable))
	// SAP HTTP status surfaces in the detail — the diagnosability
	// fix from issue #68 motivated by PR #67's 400-from-SAP debug pain.
	then.AssertThat(t, env.Error.Message, is.EqualTo("on-premise system returned HTTP 500"))
}

// errReader returns a controlled error from Read so we can exercise
// the io.ReadAll failure branch in the handler.
type errReader struct{ err error }

func (e errReader) Read(_ []byte) (int, error) { return 0, e.err }

func Test_Handler_SurfacesBodyReadErrorAs502(t *testing.T) {
	fake := &fakeMutator{
		resp: &http.Response{
			StatusCode:    http.StatusOK,
			Body:          io.NopCloser(errReader{err: errors.New("read failed")}),
			ContentLength: -1,
		},
	}
	r := newRouter(fake)

	req := httptest.NewRequest(http.MethodPost, "/adt-checkrun", strings.NewReader(reqBodyAbapSyntaxClass))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusBadGateway))
	var env btp.ErrorEnvelope
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &env), is.Nil())
	then.AssertThat(t, env.Error.Code, is.EqualTo(btp.CodeUpstreamUnreachable))
}

// Test_Register_AttachesPOSTRoute proves Register wires the route
// onto a JWT-guarded api group. We use the constructed router rather
// than calling Handler directly so the Register code path is covered.
func Test_Register_AttachesPOSTRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(stubJWTClaims(jwt.MapClaims{"user_name": "u@example"}))
	api := r.Group("/api")
	fake := &fakeMutator{
		resp: &http.Response{
			StatusCode:    http.StatusOK,
			Body:          io.NopCloser(strings.NewReader(sapCheckrunXMLNotProcessed)),
			ContentLength: -1,
		},
	}
	adtcheckrun.Register(api, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/adt-checkrun", strings.NewReader(reqBodyAbapSyntaxClass))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))
}

// Test_Handler_HandlesMessageWithoutPositionFragment exercises the
// parseMessagePosition no-fragment branch (URI without `#start=`).
func Test_Handler_HandlesMessageWithoutPositionFragment(t *testing.T) {
	const xmlNoFragment = `<?xml version="1.0" encoding="utf-8"?>
<chkrun:checkRunReports xmlns:chkrun="http://www.sap.com/adt/checkrun">
  <chkrun:checkReport chkrun:reporter="abapCheckRun"
      chkrun:triggeringUri="/sap/bc/adt/oo/classes/cl_x/source/main"
      chkrun:status="processed">
    <chkrun:checkMessageList>
      <chkrun:checkMessage uri="/sap/bc/adt/oo/classes/cl_x/source/main"
          type="I" shortText="No issues found"/>
    </chkrun:checkMessageList>
  </chkrun:checkReport>
</chkrun:checkRunReports>`
	fake := &fakeMutator{
		resp: &http.Response{
			StatusCode:    http.StatusOK,
			Body:          io.NopCloser(strings.NewReader(xmlNoFragment)),
			ContentLength: -1,
		},
	}
	r := newRouter(fake)

	req := httptest.NewRequest(http.MethodPost, "/adt-checkrun", strings.NewReader(reqBodyAbapSyntaxClass))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))
	var resp adtcheckrun.Response
	then.AssertThat(t, json.Unmarshal(w.Body.Bytes(), &resp), is.Nil())
	then.AssertThat(t, len(resp.Reports[0].Messages), is.EqualTo(1))
	then.AssertThat(t, resp.Reports[0].Messages[0].Line, is.EqualTo(0))
	then.AssertThat(t, resp.Reports[0].Messages[0].Column, is.EqualTo(0))
}
