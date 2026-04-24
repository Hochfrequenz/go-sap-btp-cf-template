package btp_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"

	"github.com/hochfrequenz/go-sap-btp-cloud-foundry-mwe/internal/btp"
)

func Test_NewOnPremiseTransport_RejectsNilConn(t *testing.T) {
	_, err := btp.NewOnPremiseTransport(nil, func(*http.Request) (string, error) { return "t", nil })
	then.AssertThat(t, errors.Is(err, btp.ErrNoConnectivityBinding), is.True())
}

func Test_NewOnPremiseTransport_RejectsEmptyProxyHost(t *testing.T) {
	_, err := btp.NewOnPremiseTransport(&btp.ConnCredentials{OnPremiseProxyPort: "1"}, func(*http.Request) (string, error) { return "t", nil })
	then.AssertThat(t, err, is.Not(is.Nil()))
}

func Test_NewOnPremiseTransport_RejectsNilProvider(t *testing.T) {
	_, err := btp.NewOnPremiseTransport(&btp.ConnCredentials{OnPremiseProxyHost: "h", OnPremiseProxyPort: "1"}, nil)
	then.AssertThat(t, err, is.Not(is.Nil()))
}

func Test_OnPremiseTransport_SetsProxyAuthOnHTTPTarget(t *testing.T) {
	var got string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Proxy-Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	pu, _ := url.Parse(proxy.URL)
	rt, err := btp.NewOnPremiseTransport(
		&btp.ConnCredentials{OnPremiseProxyHost: pu.Hostname(), OnPremiseProxyPort: pu.Port()},
		func(*http.Request) (string, error) { return "the-token", nil },
	)
	then.AssertThat(t, err, is.Nil())

	client := &http.Client{Transport: rt}
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/x", nil)
	resp, err := client.Do(req)
	then.AssertThat(t, err, is.Nil())
	_ = resp.Body.Close()
	then.AssertThat(t, got, is.EqualTo("Bearer the-token"))
}

func Test_OnPremiseTransport_PropagatesProviderError(t *testing.T) {
	rt, err := btp.NewOnPremiseTransport(
		&btp.ConnCredentials{OnPremiseProxyHost: "127.0.0.1", OnPremiseProxyPort: "1"},
		func(*http.Request) (string, error) { return "", errors.New("xsuaa down") },
	)
	then.AssertThat(t, err, is.Nil())

	client := &http.Client{Transport: rt}
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/x", nil)
	_, err = client.Do(req)
	then.AssertThat(t, err, is.Not(is.Nil()))
	then.AssertThat(t, strings.Contains(err.Error(), "xsuaa down"), is.True())
}

// Test_OnPremiseTransport_ReusesConnectionPool pins the core of the
// transport-hygiene fix (issue #21): RoundTrip must NOT clone the base
// Transport on every call, because a cloned Transport has its own
// idle-connection pool and every call then pays a fresh TCP handshake
// with the proxy. We observe this behaviourally: a proxy that records
// each incoming r.RemoteAddr will see keep-alive reuse as repeated
// occurrences of the same "ip:port" client tuple. A TCP connection
// returned to the idle pool reuses the same local port; a fresh connect
// picks a new one. If we observed N distinct RemoteAddrs for N requests,
// the pool would be defeated.
func Test_OnPremiseTransport_ReusesConnectionPool(t *testing.T) {
	var (
		mu      sync.Mutex
		remotes = make(map[string]int)
	)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		remotes[r.RemoteAddr]++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	pu, _ := url.Parse(proxy.URL)
	rt, err := btp.NewOnPremiseTransport(
		&btp.ConnCredentials{OnPremiseProxyHost: pu.Hostname(), OnPremiseProxyPort: pu.Port()},
		func(*http.Request) (string, error) { return "the-token", nil },
	)
	then.AssertThat(t, err, is.Nil())

	client := &http.Client{Transport: rt}
	const N = 8
	for i := 0; i < N; i++ {
		req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/x", nil)
		resp, err := client.Do(req)
		then.AssertThat(t, err, is.Nil())
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	mu.Lock()
	distinct := len(remotes)
	mu.Unlock()
	// With a shared pool, all N requests land on 1 TCP connection.
	// Allow a tiny amount of slack for test-runner scheduling quirks,
	// but anything above 2 means the pool is defeated.
	then.AssertThat(t, distinct <= 2, is.True())
}
