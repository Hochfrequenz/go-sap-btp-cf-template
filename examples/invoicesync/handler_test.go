package invoicesync_test

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

	"github.com/hochfrequenz/go-sap-btp-cloud-foundry-mwe/examples/invoicesync"
)

// fakeOnPrem is the canonical handler-test double: a one-method fake
// satisfying btp.OnPremCaller. It records the arguments the handler
// passed (so the test can assert request shape) and returns a canned
// response (so the test can assert response translation). Nothing
// about XSUAA, Destination, or the Cloud Connector is exercised —
// those layers are Service's concern and already tested in
// internal/btp/service_test.go.
type fakeOnPrem struct {
	// captured inputs
	gotDest   string
	gotMethod string
	gotPath   string
	gotBody   []byte

	// canned outputs
	resp *http.Response
	err  error
}

func (f *fakeOnPrem) CallOnPremise(_ context.Context, dest, method, path string,
	_ http.Header, body io.Reader) (*http.Response, error) {
	f.gotDest, f.gotMethod, f.gotPath = dest, method, path
	if body != nil {
		f.gotBody, _ = io.ReadAll(body)
	}
	return f.resp, f.err
}

// stubJWTClaims is a tiny middleware that drops a pretend-validated
// jwt.MapClaims into the Gin context under the same key the real
// validator.Middleware() uses. The handler's `c.MustGet("jwtClaims")`
// cannot panic without it.
func stubJWTClaims(claims jwt.MapClaims) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("jwtClaims", claims)
		c.Next()
	}
}

func newRouter(fake *fakeOnPrem) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(stubJWTClaims(jwt.MapClaims{"user_name": "test-user@example"}))
	r.POST("/invoice-sync", invoicesync.Handler(fake))
	return r
}

func Test_Handler_MarshalsRequestIntoABAPShape(t *testing.T) {
	fake := &fakeOnPrem{
		resp: &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			Body:          io.NopCloser(strings.NewReader(`{"ok":true}`)),
			ContentLength: -1,
		},
	}
	r := newRouter(fake)

	// Drive a request that passes struct-tag validation.
	body := `{
      "company_code": "1000",
      "posting_date": "2026-04-23T00:00:00Z",
      "amount_cents": 12345,
      "currency":     "EUR",
      "reference":    "INV-42"
    }`
	req := httptest.NewRequest(http.MethodPost, "/invoice-sync", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// The handler should have answered 200 and forwarded the canned body.
	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))
	then.AssertThat(t, w.Body.String(), is.EqualTo(`{"ok":true}`))

	// And it should have asked the OnPremCaller for the expected shape.
	then.AssertThat(t, fake.gotDest, is.EqualTo("HF_S4"))
	then.AssertThat(t, fake.gotMethod, is.EqualTo(http.MethodPost))
	then.AssertThat(t, fake.gotPath, is.EqualTo("/sap/bc/rest/zmy_invoice_sync"))

	// Decode into a typed struct so the matcher inference is clean.
	// This also mirrors the README's "type everything" discipline —
	// tests inspect the ABAP-side shape through a struct, not a map.
	var abap struct {
		BUKRS string `json:"BUKRS"`
		WAERS string `json:"WAERS"`
		XBLNR string `json:"XBLNR"`
	}
	then.AssertThat(t, json.Unmarshal(fake.gotBody, &abap), is.Nil())
	then.AssertThat(t, abap.BUKRS, is.EqualTo("1000"))
	then.AssertThat(t, abap.WAERS, is.EqualTo("EUR"))
	then.AssertThat(t, abap.XBLNR, is.EqualTo("INV-42"))
}

func Test_Handler_RejectsInvalidPayloadBeforeTouchingSAP(t *testing.T) {
	// A fake whose CallOnPremise MUST NOT be invoked — the handler
	// should fail at struct-tag validation and never reach it.
	fake := &fakeOnPrem{}
	r := newRouter(fake)

	// company_code must be 4 uppercase chars; this sends 3 lowercase.
	body := `{"company_code":"abc","posting_date":"2026-04-23T00:00:00Z","amount_cents":1,"currency":"EUR"}`
	req := httptest.NewRequest(http.MethodPost, "/invoice-sync", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusBadRequest))
	// Crucial: the fake was not called. Validation short-circuited
	// before SAP could see a malformed payload.
	then.AssertThat(t, fake.gotDest, is.EqualTo(""))
}

func Test_Handler_SurfacesSAPErrorAs502(t *testing.T) {
	// When svc.CallOnPremise returns an error (e.g. the on-prem system
	// is unreachable), the handler should surface a 502 with the error
	// as message — not a 500.
	fake := &fakeOnPrem{err: errors.New("on-prem system unreachable")}
	r := newRouter(fake)

	body := `{"company_code":"1000","posting_date":"2026-04-23T00:00:00Z","amount_cents":1,"currency":"EUR"}`
	req := httptest.NewRequest(http.MethodPost, "/invoice-sync", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	then.AssertThat(t, w.Code, is.EqualTo(http.StatusBadGateway))
	then.AssertThat(t, strings.Contains(w.Body.String(), "unreachable"), is.True())
}
