package btp_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
