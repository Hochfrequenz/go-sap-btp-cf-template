package btp_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"

	"github.com/hochfrequenz/go-sap-btp-cloud-foundry-mwe/internal/btp"
)

// csrfSAPServer stands in for an SAP ICF node that enforces the CSRF
// handshake. It answers two routes:
//
//   - GET <fetchPath> with `X-CSRF-Token: Fetch` returns a token plus
//     two Set-Cookie lines (SAP_SESSIONID_* and sap-usercontext —
//     the shape a real ICF node sends).
//   - POST / PUT / DELETE anywhere else requires the right token +
//     cookies. Missing or stale → 403 + `X-CSRF-Token: Required`.
//     Correct → 200.
//
// fakeToken lets a test flip what the fetch endpoint hands out, so
// the 403-then-re-fetch path can be driven deterministically.
type csrfSAPServer struct {
	server         *httptest.Server
	fetchPath      string
	fetchHits      atomic.Int32
	mutatingHits   atomic.Int32
	lastMutatingCk string // the cookies the mutating call carried
	lastMutatingTk string // the token the mutating call carried
	currentToken   atomic.Value // string — swapped to simulate server-side session recycle
}

func newCSRFSAPServer(fetchPath, initialToken string) *csrfSAPServer {
	s := &csrfSAPServer{fetchPath: fetchPath}
	s.currentToken.Store(initialToken)

	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == fetchPath {
			if r.Header.Get("X-CSRF-Token") != "Fetch" {
				http.Error(w, "fetch path requires X-CSRF-Token: Fetch", http.StatusBadRequest)
				return
			}
			s.fetchHits.Add(1)
			tok, _ := s.currentToken.Load().(string)
			w.Header().Set("X-CSRF-Token", tok)
			// Two cookies — one SAP session, one usercontext. Realistic
			// shape for an ICF node.
			w.Header().Add("Set-Cookie", "SAP_SESSIONID_ABC_100=abc123; path=/")
			w.Header().Add("Set-Cookie", "sap-usercontext=sapclient100; path=/")
			w.WriteHeader(http.StatusOK)
			return
		}
		// Mutating path.
		s.mutatingHits.Add(1)
		s.lastMutatingTk = r.Header.Get("X-CSRF-Token")
		s.lastMutatingCk = r.Header.Get("Cookie")
		expected, _ := s.currentToken.Load().(string)
		if s.lastMutatingTk != expected {
			// SAP's real signal: 403 + explicit header.
			w.Header().Set("X-CSRF-Token", "Required")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	return s
}

func (s *csrfSAPServer) Close() { s.server.Close() }

// newBTPStackRoutedThrough reuses newBTPStack but wires its proxy to a
// specific onPrem (not the stack's default). Saves boilerplate in each
// CSRF test.
func newBTPStackRoutedThrough(t *testing.T, onPremURL string) *btpStack {
	t.Helper()
	s := newBTPStack(t, fmt.Sprintf(`{
		"destinationConfiguration":{"Name":"D","Type":"HTTP","URL":%q,"Authentication":"NoAuthentication","ProxyType":"OnPremise"}
	}`, onPremURL))
	// Replace the stack's proxy to forward to onPremURL directly rather
	// than to the stack's auto-created onPrem.
	s.proxy.Close()
	s.proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Proxy-Authorization"), "Bearer ") {
			http.Error(w, "missing proxy auth", http.StatusProxyAuthRequired)
			return
		}
		u, err := url.Parse(r.RequestURI)
		if err != nil || u.Host == "" {
			http.Error(w, "bad request-uri", http.StatusBadRequest)
			return
		}
		outReq, _ := http.NewRequestWithContext(r.Context(), r.Method, onPremURL+u.Path, r.Body)
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
		defer func() { _ = resp.Body.Close() }()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(s.proxy.Close)
	pu, _ := url.Parse(s.proxy.URL)
	s.env.Conn.OnPremiseProxyHost = pu.Hostname()
	s.env.Conn.OnPremiseProxyPort = pu.Port()
	return s
}

// Test_CallOnPremiseMutating_RunsHandshakeAndAttaches covers the happy
// path: first call against a destination triggers a fetch (which
// returns the token + cookies), then the mutating call carries both.
// Pins that the SAP session cookies actually survive the forward —
// the whole reason filterForwardedCookies exists.
func Test_CallOnPremiseMutating_RunsHandshakeAndAttaches(t *testing.T) {
	sap := newCSRFSAPServer("/sap/bc/adt/discovery", "tok-1")
	defer sap.Close()

	s := newBTPStackRoutedThrough(t, sap.server.URL)

	svc, err := btp.NewService(s.env)
	then.AssertThat(t, err, is.Nil())

	resp, err := svc.CallOnPremiseMutating(context.Background(), "D", http.MethodPost,
		"/sap/bc/adt/activation",
		http.Header{"Content-Type": []string{"application/xml"}},
		bytes.NewReader([]byte(`<x/>`)))
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, resp.StatusCode, is.EqualTo(http.StatusOK))
	_ = resp.Body.Close()

	// One fetch, one mutating call.
	then.AssertThat(t, int(sap.fetchHits.Load()), is.EqualTo(1))
	then.AssertThat(t, int(sap.mutatingHits.Load()), is.EqualTo(1))
	// Mutating call carried the token.
	then.AssertThat(t, sap.lastMutatingTk, is.EqualTo("tok-1"))
	// And both cookies passed through the forward filter, joined in
	// one header value per RFC 6265.
	then.AssertThat(t, strings.Contains(sap.lastMutatingCk, "SAP_SESSIONID_ABC_100=abc123"), is.True())
	then.AssertThat(t, strings.Contains(sap.lastMutatingCk, "sap-usercontext=sapclient100"), is.True())
}

// Test_CallOnPremiseMutating_CachesStateAcrossCalls pins that a second
// mutating call reuses the cached token/cookies rather than fetching
// again. One destination → one fetch in the steady state.
func Test_CallOnPremiseMutating_CachesStateAcrossCalls(t *testing.T) {
	sap := newCSRFSAPServer("/sap/bc/adt/discovery", "tok-stable")
	defer sap.Close()

	s := newBTPStackRoutedThrough(t, sap.server.URL)

	svc, err := btp.NewService(s.env)
	then.AssertThat(t, err, is.Nil())

	for i := 0; i < 3; i++ {
		resp, err := svc.CallOnPremiseMutating(context.Background(), "D", http.MethodPost,
			"/sap/bc/adt/activation", nil, bytes.NewReader([]byte(`<x/>`)))
		then.AssertThat(t, err, is.Nil())
		_ = resp.Body.Close()
	}

	then.AssertThat(t, int(sap.fetchHits.Load()), is.EqualTo(1))
	then.AssertThat(t, int(sap.mutatingHits.Load()), is.EqualTo(3))
}

// Test_CallOnPremiseMutating_RefetchesOnCSRFRequired drives the retry
// path. SAP recycles its session (currentToken changes); the cached
// token now mismatches; the server returns 403 + X-CSRF-Token:
// Required; the Service must re-fetch once and retry, ending in 200.
func Test_CallOnPremiseMutating_RefetchesOnCSRFRequired(t *testing.T) {
	sap := newCSRFSAPServer("/sap/bc/adt/discovery", "tok-old")
	defer sap.Close()

	s := newBTPStackRoutedThrough(t, sap.server.URL)

	svc, err := btp.NewService(s.env)
	then.AssertThat(t, err, is.Nil())

	// First call primes the cache with tok-old.
	resp, err := svc.CallOnPremiseMutating(context.Background(), "D", http.MethodPost,
		"/sap/bc/adt/activation", nil, bytes.NewReader([]byte(`<x/>`)))
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, resp.StatusCode, is.EqualTo(http.StatusOK))
	_ = resp.Body.Close()

	// Server "recycles" its session — any request with tok-old now 403s.
	sap.currentToken.Store("tok-new")

	// Second call: cached tok-old fails, Service re-fetches (gets
	// tok-new), retries, succeeds.
	resp, err = svc.CallOnPremiseMutating(context.Background(), "D", http.MethodPost,
		"/sap/bc/adt/activation", nil, bytes.NewReader([]byte(`<x/>`)))
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, resp.StatusCode, is.EqualTo(http.StatusOK))
	_ = resp.Body.Close()

	// Two fetches (initial + re-fetch after 403).
	then.AssertThat(t, int(sap.fetchHits.Load()), is.EqualTo(2))
	// Three mutating attempts (first success + failed retry + successful retry).
	then.AssertThat(t, int(sap.mutatingHits.Load()), is.EqualTo(3))
	// Final mutating call carried the new token.
	then.AssertThat(t, sap.lastMutatingTk, is.EqualTo("tok-new"))
}

// Test_CallOnPremiseMutating_SurfacesRealForbidden pins that a 403
// WITHOUT the X-CSRF-Token: Required header does NOT trigger a
// re-fetch. That's a real authorization failure and fresh tokens
// cannot fix it — retrying would mask the real problem.
func Test_CallOnPremiseMutating_SurfacesRealForbidden(t *testing.T) {
	sap := newCSRFSAPServer("/sap/bc/adt/discovery", "tok-1")
	// Hijack the mutating handler to return a "real" 403 with NO
	// X-CSRF-Token header.
	sap.server.Close()
	sap.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/sap/bc/adt/discovery" {
			sap.fetchHits.Add(1)
			w.Header().Set("X-CSRF-Token", "tok-1")
			w.Header().Add("Set-Cookie", "SAP_SESSIONID_XYZ_100=s1")
			w.WriteHeader(http.StatusOK)
			return
		}
		sap.mutatingHits.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer sap.Close()

	s := newBTPStackRoutedThrough(t, sap.server.URL)

	svc, err := btp.NewService(s.env)
	then.AssertThat(t, err, is.Nil())

	resp, err := svc.CallOnPremiseMutating(context.Background(), "D", http.MethodPost,
		"/sap/bc/adt/activation", nil, bytes.NewReader([]byte(`<x/>`)))
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, resp.StatusCode, is.EqualTo(http.StatusForbidden))
	_ = resp.Body.Close()

	// Single fetch, single mutating attempt — NO retry, NO re-fetch.
	then.AssertThat(t, int(sap.fetchHits.Load()), is.EqualTo(1))
	then.AssertThat(t, int(sap.mutatingHits.Load()), is.EqualTo(1))
}

// Test_CallOnPremiseMutating_SingleflightDedupesConcurrentFetches
// pins the concurrency contract: N goroutines hitting a cold cache
// for the same destination trigger exactly ONE fetch, not N. This is
// the whole reason csrfStateFor uses singleflight — without it, a
// burst of incoming POSTs would hammer the SAP ICF with redundant
// discovery requests on every cold start.
func Test_CallOnPremiseMutating_SingleflightDedupesConcurrentFetches(t *testing.T) {
	sap := newCSRFSAPServer("/sap/bc/adt/discovery", "tok-1")
	// Slow down the fetch so the race window is real — 20 ms is
	// plenty for the goroutines below to all reach csrfStateFor
	// before the first one's fetch returns.
	sap.server.Close()
	sap.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/sap/bc/adt/discovery" {
			if r.Header.Get("X-CSRF-Token") != "Fetch" {
				http.Error(w, "fetch requires X-CSRF-Token: Fetch", http.StatusBadRequest)
				return
			}
			sap.fetchHits.Add(1)
			time.Sleep(20 * time.Millisecond)
			w.Header().Set("X-CSRF-Token", "tok-1")
			w.Header().Add("Set-Cookie", "SAP_SESSIONID_ABC_100=abc123; path=/")
			w.WriteHeader(http.StatusOK)
			return
		}
		sap.mutatingHits.Add(1)
		if r.Header.Get("X-CSRF-Token") == "tok-1" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("X-CSRF-Token", "Required")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer sap.Close()

	s := newBTPStackRoutedThrough(t, sap.server.URL)
	svc, err := btp.NewService(s.env)
	then.AssertThat(t, err, is.Nil())

	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			resp, err := svc.CallOnPremiseMutating(context.Background(), "D",
				http.MethodPost, "/sap/bc/adt/activation", nil,
				bytes.NewReader([]byte(`<x/>`)))
			then.AssertThat(t, err, is.Nil())
			if resp != nil {
				_ = resp.Body.Close()
			}
		}()
	}
	wg.Wait()

	// One fetch total — singleflight collapsed the N-way race.
	// N mutating calls all succeeded.
	then.AssertThat(t, int(sap.fetchHits.Load()), is.EqualTo(1))
	then.AssertThat(t, int(sap.mutatingHits.Load()), is.EqualTo(N))
}

// Test_CallOnPremiseMutating_FetchErrors covers the two error
// branches of fetchCSRF a happy-path test can't reach: a non-2xx
// status (with the body snippet included in the returned error for
// operator triage) and a 200 response missing the X-CSRF-Token
// header entirely (a misconfigured SAP or a non-CSRF endpoint).
func Test_CallOnPremiseMutating_FetchErrors(t *testing.T) {
	t.Run("non-2xx fetch status surfaces body snippet", func(t *testing.T) {
		sap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("Accept header required"))
		}))
		defer sap.Close()

		s := newBTPStackRoutedThrough(t, sap.URL)
		svc, err := btp.NewService(s.env)
		then.AssertThat(t, err, is.Nil())

		_, err = svc.CallOnPremiseMutating(context.Background(), "D",
			http.MethodPost, "/x", nil, bytes.NewReader([]byte(`{}`)))
		then.AssertThat(t, err, is.Not(is.Nil()))
		then.AssertThat(t, strings.Contains(err.Error(), "status 400"), is.True())
		// Body snippet included so operators can see WHY SAP rejected.
		then.AssertThat(t, strings.Contains(err.Error(), "Accept header required"), is.True())
	})

	t.Run("fetch returns 200 but no X-CSRF-Token header", func(t *testing.T) {
		sap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Legitimate 200 — but no token header means this path
			// is not CSRF-protected (or the fetch path is wrong).
			w.WriteHeader(http.StatusOK)
		}))
		defer sap.Close()

		s := newBTPStackRoutedThrough(t, sap.URL)
		svc, err := btp.NewService(s.env)
		then.AssertThat(t, err, is.Nil())

		_, err = svc.CallOnPremiseMutating(context.Background(), "D",
			http.MethodPost, "/x", nil, bytes.NewReader([]byte(`{}`)))
		then.AssertThat(t, err, is.Not(is.Nil()))
		then.AssertThat(t, strings.Contains(err.Error(), "empty X-CSRF-Token header"), is.True())
	})
}

// Test_CallOnPremiseMutating_NilBody pins the body==nil branch of
// CallOnPremiseMutating: a DELETE without a body should run the
// handshake and send the mutating request normally.
func Test_CallOnPremiseMutating_NilBody(t *testing.T) {
	sap := newCSRFSAPServer("/sap/bc/adt/discovery", "tok-1")
	defer sap.Close()

	s := newBTPStackRoutedThrough(t, sap.server.URL)
	svc, err := btp.NewService(s.env)
	then.AssertThat(t, err, is.Nil())

	resp, err := svc.CallOnPremiseMutating(context.Background(), "D",
		http.MethodDelete, "/sap/bc/adt/objects/zmy", nil, nil)
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, resp.StatusCode, is.EqualTo(http.StatusOK))
	_ = resp.Body.Close()
	then.AssertThat(t, int(sap.fetchHits.Load()), is.EqualTo(1))
	then.AssertThat(t, int(sap.mutatingHits.Load()), is.EqualTo(1))
}

// Test_CallOnPremiseMutating_CustomFetchPath pins WithCSRFFetchPath:
// a fork with non-ADT endpoints can configure its own path and the
// handshake runs against that.
func Test_CallOnPremiseMutating_CustomFetchPath(t *testing.T) {
	sap := newCSRFSAPServer("/sap/bc/rest/zmy_service", "tok-custom")
	defer sap.Close()

	s := newBTPStackRoutedThrough(t, sap.server.URL)

	svc, err := btp.NewService(s.env, btp.WithCSRFFetchPath("/sap/bc/rest/zmy_service"))
	then.AssertThat(t, err, is.Nil())

	resp, err := svc.CallOnPremiseMutating(context.Background(), "D", http.MethodPost,
		"/sap/bc/rest/zmy_service", nil, bytes.NewReader([]byte(`{}`)))
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, resp.StatusCode, is.EqualTo(http.StatusOK))
	_ = resp.Body.Close()

	then.AssertThat(t, int(sap.fetchHits.Load()), is.EqualTo(1))
	then.AssertThat(t, sap.lastMutatingTk, is.EqualTo("tok-custom"))
}
