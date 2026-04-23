package btp

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"
)

// ConnTokenProvider yields a fresh connectivity-service bearer token per
// request. Accepting a ctx here means cancellation on the caller's request
// propagates into the token fetch — without it, a slow XSUAA could hang the
// proxied call after the client already gave up.
type ConnTokenProvider func(ctx *http.Request) (string, error)

// NewOnPremiseTransport builds a RoundTripper that routes every request
// through the Connectivity service's on-premise reverse proxy.
//
// Proxy-Authorization must always be `Bearer <conn-token>`. For HTTPS
// targets Go's standard library sends it on the CONNECT tunnel via
// ProxyConnectHeader; for plain HTTP targets the header travels with the
// forwarded request itself (the proxy consumes it before forwarding).
// Setting it on both paths is harmless and keeps the RoundTripper
// branch-free.
func NewOnPremiseTransport(conn *ConnCredentials, provider ConnTokenProvider) (http.RoundTripper, error) {
	if conn == nil {
		return nil, ErrNoConnectivityBinding
	}
	if conn.OnPremiseProxyHost == "" || conn.OnPremiseProxyPort == "" {
		return nil, errors.New("connectivity binding has empty onpremise_proxy_host/port")
	}
	if provider == nil {
		return nil, errors.New("ConnTokenProvider is required")
	}

	proxyURL := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(conn.OnPremiseProxyHost, conn.OnPremiseProxyPort),
	}

	base := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
	}
	return &onPremiseRoundTripper{base: base, token: provider}, nil
}

type onPremiseRoundTripper struct {
	base  *http.Transport
	token ConnTokenProvider
}

func (t *onPremiseRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := t.token(req)
	if err != nil {
		return nil, err
	}
	// Clone before mutating so a retry (by the caller) sees the original.
	r := req.Clone(req.Context())
	r.Header.Set("Proxy-Authorization", "Bearer "+tok)
	tr := t.base.Clone()
	tr.ProxyConnectHeader = http.Header{
		"Proxy-Authorization": []string{"Bearer " + tok},
	}
	return tr.RoundTrip(r)
}

// DefaultOnPremiseTimeout is the per-call timeout for proxied requests.
// On-prem R/3 systems can be slow; Service uses this and exposes no knob —
// wrap the returned http.Client if you need a different value.
const DefaultOnPremiseTimeout = 30 * time.Second
