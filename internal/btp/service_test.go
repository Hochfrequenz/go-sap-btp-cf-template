package btp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"
	"github.com/gin-gonic/gin"

	"github.com/hochfrequenz/go-sap-btp-cloud-foundry-mwe/internal/btp"
)

// btpStack spins up three httptest servers standing in for XSUAA, the
// Destination service, and the on-premise proxy. It is deliberately
// coarse-grained: the MWE's value is the wiring between these three, so the
// tests verify the wiring end-to-end rather than mock individual helpers.
type btpStack struct {
	xsuaa   *httptest.Server
	dest    *httptest.Server
	proxy   *httptest.Server
	onPrem  *httptest.Server
	env     *btp.Env
	tokens  atomic.Int32 // exchange count on the XSUAA server
	lookups atomic.Int32 // destination lookups
	calls   atomic.Int32 // on-prem calls
}

func newBTPStack(t *testing.T, destBody string) *btpStack {
	t.Helper()
	s := &btpStack{}

	// On-prem "SAP". The test proxy below forwards to here.
	s.onPrem = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		// Echo back headers that the test wants to inspect.
		w.Header().Set("X-Received-Auth", r.Header.Get("Authorization"))
		w.Header().Set("X-Received-UA", r.Header.Get("User-Agent"))
		w.Header().Set("X-Received-Location", r.Header.Get("SAP-Connectivity-SCC-Location_ID"))
		w.Header().Set("X-Received-Cookie", r.Header.Get("Cookie"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"path":"` + r.URL.Path + `"}`))
	}))
	t.Cleanup(s.onPrem.Close)

	// Fake CC HTTP proxy: forward the incoming request to the onPrem URL,
	// asserting Proxy-Authorization along the way.
	s.proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Proxy-Authorization"), "Bearer ") {
			http.Error(w, "missing proxy auth", http.StatusProxyAuthRequired)
			return
		}
		// The client sends an absolute URL for HTTP-through-HTTP-proxy.
		u, err := url.Parse(r.RequestURI)
		if err != nil || u.Host == "" {
			http.Error(w, "bad request-uri", http.StatusBadRequest)
			return
		}
		onPremURL, _ := url.Parse(s.onPrem.URL)
		outReq, _ := http.NewRequestWithContext(r.Context(), r.Method, onPremURL.String()+u.Path, r.Body)
		for k, vs := range r.Header {
			if strings.EqualFold(k, "Proxy-Authorization") {
				continue
			}
			for _, v := range vs {
				outReq.Header.Add(k, v)
			}
		}
		resp, err := http.DefaultClient.Do(outReq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(s.proxy.Close)

	// Destination service — returns the caller-supplied body.
	s.dest = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.lookups.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(destBody))
	}))
	t.Cleanup(s.dest.Close)

	// XSUAA token endpoint — returns a one-hour token per call.
	s.xsuaa = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := s.tokens.Add(1)
		fmt.Fprintf(w, `{"access_token":"tok-%d","token_type":"bearer","expires_in":3600}`, n)
	}))
	t.Cleanup(s.xsuaa.Close)

	proxyURL, _ := url.Parse(s.proxy.URL)
	s.env = &btp.Env{
		XSUAA: &btp.XSUAACredentials{URL: s.xsuaa.URL, ClientID: "x", ClientSecret: "y", XSAppName: "GoApp", UAADomain: "uaa"},
		Dest:  &btp.DestCredentials{URI: s.dest.URL, ClientID: "d", ClientSecret: "ds", URL: s.xsuaa.URL},
		Conn:  &btp.ConnCredentials{ClientID: "c", ClientSecret: "cs", URL: s.xsuaa.URL, OnPremiseProxyHost: proxyURL.Hostname(), OnPremiseProxyPort: proxyURL.Port()},
	}
	return s
}

func Test_Service_CallOnPremise_EndToEnd(t *testing.T) {
	s := newBTPStack(t, `{
		"destinationConfiguration":{
			"Name":"HfSap","Type":"HTTP","URL":"`+"http://sap.internal:8000"+`",
			"Authentication":"BasicAuthentication","ProxyType":"OnPremise",
			"User":"u","Password":"p",
			"CloudConnectorLocationId":"loc-42"
		}
	}`)
	// Point the destination URL at our fake on-prem so the request actually
	// goes somewhere the proxy can reach. We rebuild the stack JSON with a
	// working URL; the Destination's proxy routing goes via our fake proxy.
	s = newBTPStack(t, fmt.Sprintf(`{
		"destinationConfiguration":{
			"Name":"HfSap","Type":"HTTP","URL":%q,
			"Authentication":"BasicAuthentication","ProxyType":"OnPremise",
			"User":"u","Password":"p",
			"CloudConnectorLocationId":"loc-42"
		}
	}`, s.onPrem.URL))

	svc, err := btp.NewService(s.env)
	then.AssertThat(t, err, is.Nil())

	// Feed the inbound request with headers the service should filter.
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer forwarded-user-jwt")
	hdr.Set("Cookie", "approuter-sess=secret")
	hdr.Set("X-Trace-ID", "abc")

	resp, err := svc.CallOnPremise(context.Background(), "HfSap", http.MethodGet, "/sap/opu/odata/ping", hdr, nil)
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, resp.StatusCode, is.EqualTo(http.StatusOK))
	_ = resp.Body.Close()

	// Basic-auth replaced the forwarded user JWT.
	then.AssertThat(t, strings.HasPrefix(resp.Header.Get("X-Received-Auth"), "Basic "), is.True())
	// Cookie was filtered out.
	then.AssertThat(t, resp.Header.Get("X-Received-Cookie"), is.EqualTo(""))
	// Neutral UA set when caller didn't supply one.
	then.AssertThat(t, strings.Contains(resp.Header.Get("X-Received-UA"), "go-sap-btp-cloud-foundry-mwe"), is.True())
	// Location ID forwarded from the destination.
	then.AssertThat(t, resp.Header.Get("X-Received-Location"), is.EqualTo("loc-42"))
	// Exactly two XSUAA exchanges: one for dest-service, one for connectivity.
	then.AssertThat(t, int(s.tokens.Load()), is.EqualTo(2))
}

func Test_Service_CallOnPremise_NoLocationIDHeaderWhenAbsent(t *testing.T) {
	s := newBTPStack(t, fmt.Sprintf(`{
		"destinationConfiguration":{
			"Name":"D","Type":"HTTP","URL":%q,
			"Authentication":"NoAuthentication","ProxyType":"OnPremise"
		}
	}`, "placeholder"))
	s = newBTPStack(t, fmt.Sprintf(`{
		"destinationConfiguration":{
			"Name":"D","Type":"HTTP","URL":%q,
			"Authentication":"NoAuthentication","ProxyType":"OnPremise"
		}
	}`, s.onPrem.URL))

	svc, err := btp.NewService(s.env)
	then.AssertThat(t, err, is.Nil())

	resp, err := svc.CallOnPremise(context.Background(), "D", http.MethodGet, "/x", nil, nil)
	then.AssertThat(t, err, is.Nil())
	defer resp.Body.Close()
	then.AssertThat(t, resp.Header.Get("X-Received-Location"), is.EqualTo(""))
}

func Test_Service_CallOnPremise_DestinationNotFound(t *testing.T) {
	s := newBTPStack(t, `ignored`)
	s.dest.Close()
	s.dest = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(s.dest.Close)
	s.env.Dest.URI = s.dest.URL

	svc, err := btp.NewService(s.env)
	then.AssertThat(t, err, is.Nil())

	_, err = svc.CallOnPremise(context.Background(), "Missing", http.MethodGet, "/", nil, nil)
	then.AssertThat(t, err, is.Not(is.Nil()))
	then.AssertThat(t, errors.Is(err, btp.ErrDestinationNotFound), is.True())
}

func Test_NewService_RequiresBindings(t *testing.T) {
	_, err := btp.NewService(nil)
	then.AssertThat(t, err, is.Not(is.Nil()))

	_, err = btp.NewService(&btp.Env{XSUAA: &btp.XSUAACredentials{URL: "https://u", XSAppName: "a"}})
	then.AssertThat(t, errors.Is(err, btp.ErrNoDestinationBinding), is.True())

	_, err = btp.NewService(&btp.Env{
		XSUAA: &btp.XSUAACredentials{URL: "https://u", XSAppName: "a"},
		Dest:  &btp.DestCredentials{URI: "https://d", ClientID: "c", ClientSecret: "s", URL: "https://u"},
	})
	then.AssertThat(t, errors.Is(err, btp.ErrNoConnectivityBinding), is.True())
}

func Test_Service_AuthenticatorsExposesRegistry(t *testing.T) {
	s := newBTPStack(t, `{"destinationConfiguration":{"URL":"x"}}`)
	svc, err := btp.NewService(s.env)
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, svc.Authenticators() != nil, is.True())
}

func Test_Service_CallOnPremise_RetriesOn401(t *testing.T) {
	// The stack helper hard-codes its proxy to forward to its own onPrem,
	// so for this test we install a single onPrem (401-then-200) and then
	// build the stack with that URL as the destination target — the
	// proxy's forward URL is what matters since the stack overrides path.
	var calls atomic.Int32
	flipServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer flipServer.Close()

	s := newBTPStack(t, "placeholder")
	// Swap the stack's onPrem for our flip-server by reusing its address.
	// Simpler: reach into the helper — close its default onPrem and make
	// the proxy forward to flipServer instead. We can accomplish the same
	// by discarding the stack's auto-URL and rebuilding destBody to point
	// at flipServer, since the proxy handler uses s.onPrem.URL directly
	// (hard-coded in the helper). Override it by replacing the proxy.
	s.proxy.Close()
	s.proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Proxy-Authorization"), "Bearer ") {
			http.Error(w, "missing proxy auth", http.StatusProxyAuthRequired)
			return
		}
		u, err := url.Parse(r.RequestURI)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		outReq, _ := http.NewRequestWithContext(r.Context(), r.Method, flipServer.URL+u.Path, r.Body)
		for k, vs := range r.Header {
			if strings.EqualFold(k, "Proxy-Authorization") {
				continue
			}
			for _, v := range vs {
				outReq.Header.Add(k, v)
			}
		}
		resp, err := http.DefaultClient.Do(outReq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(s.proxy.Close)
	pu, _ := url.Parse(s.proxy.URL)
	s.env.Conn.OnPremiseProxyHost = pu.Hostname()
	s.env.Conn.OnPremiseProxyPort = pu.Port()

	// Destination URL can be anything HTTP; the proxy overrides it.
	s.dest.Close()
	s.dest = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"destinationConfiguration":{"Name":"D","Type":"HTTP","URL":%q,"Authentication":"NoAuthentication","ProxyType":"OnPremise"}}`, flipServer.URL)
	}))
	t.Cleanup(s.dest.Close)
	s.env.Dest.URI = s.dest.URL

	svc, err := btp.NewService(s.env)
	then.AssertThat(t, err, is.Nil())

	resp, err := svc.CallOnPremise(context.Background(), "D", http.MethodGet, "/", nil, nil)
	then.AssertThat(t, err, is.Nil())
	defer resp.Body.Close()
	then.AssertThat(t, resp.StatusCode, is.EqualTo(http.StatusOK))
	then.AssertThat(t, int(calls.Load()), is.EqualTo(2))
}

func Test_Service_ProxyHandler_EndToEnd(t *testing.T) {
	s := newBTPStack(t, "placeholder")
	s = newBTPStack(t, fmt.Sprintf(`{
		"destinationConfiguration":{"Name":"D","Type":"HTTP","URL":%q,"Authentication":"NoAuthentication","ProxyType":"OnPremise"}
	}`, s.onPrem.URL))
	svc, err := btp.NewService(s.env)
	then.AssertThat(t, err, is.Nil())

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Any("/api/sap/:destination/*path", svc.ProxyHandler)

	req := httptest.NewRequest(http.MethodGet, "/api/sap/D/whatever", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	then.AssertThat(t, w.Code, is.EqualTo(http.StatusOK))
	then.AssertThat(t, strings.Contains(w.Body.String(), `"ok":true`), is.True())
}

func Test_Service_CallOnPremise_RejectsPathTraversal(t *testing.T) {
	s := newBTPStack(t, `{"destinationConfiguration":{"URL":"http://x"}}`)
	svc, err := btp.NewService(s.env)
	then.AssertThat(t, err, is.Nil())

	_, err = svc.CallOnPremise(context.Background(), "D", http.MethodGet, "/foo/../../admin", nil, nil)
	then.AssertThat(t, err, is.Not(is.Nil()))
	then.AssertThat(t, strings.Contains(err.Error(), "traversal"), is.True())
}

func Test_Service_ProxyHandler_Returns502OnLookupFail(t *testing.T) {
	s := newBTPStack(t, "placeholder")
	s.dest.Close()
	s.dest = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(s.dest.Close)
	s.env.Dest.URI = s.dest.URL

	svc, err := btp.NewService(s.env)
	then.AssertThat(t, err, is.Nil())

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Any("/api/sap/:destination/*path", svc.ProxyHandler)

	req := httptest.NewRequest(http.MethodGet, "/api/sap/Missing/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	then.AssertThat(t, w.Code, is.EqualTo(http.StatusBadGateway))
}
