package btp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"
)

// ConnTokenProvider yields a fresh connectivity-service bearer token per
// request. Accepting a request here means cancellation on the caller's
// request propagates into the token fetch — without it, a slow XSUAA
// could hang the proxied call after the client already gave up.
type ConnTokenProvider func(ctx *http.Request) (string, error)

// NewOnPremiseTransport builds a RoundTripper that routes every request
// through the Connectivity service's on-premise reverse proxy.
//
// Proxy-Authorization must always be `Bearer <conn-token>`. For HTTPS
// targets Go's standard library sends it on the CONNECT tunnel via
// Transport.GetProxyConnectHeader (wired here, per-request); for plain
// HTTP targets the header travels with the forwarded request itself
// (the proxy consumes it before forwarding). Wiring the CONNECT header
// per-request rather than per-Transport is what lets the RoundTripper
// stop cloning the Transport on every call — the idle-connection pool
// stays shared across calls.
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
		GetProxyConnectHeader: func(ctx context.Context, _ *url.URL, _ string) (http.Header, error) {
			// net/http calls this when opening a CONNECT tunnel for an
			// HTTPS target. We expose only ctx to the provider; the
			// existing ConnTokenProvider signature takes *http.Request,
			// so we hand it a bare request carrying the right context.
			// The provider reads ctx.Done / request cancellation only.
			//
			// For HTTPS targets this provider is called twice per
			// request (once here for the CONNECT header, once in
			// RoundTrip for the body-leg Proxy-Authorization). The
			// production TokenFetcher caches, so both calls are
			// cache hits on the hot path and the second call is a
			// sync.Map lookup. First call after cache expiry is the
			// only case that re-fetches twice.
			tok, err := provider((&http.Request{}).WithContext(ctx))
			if err != nil {
				return nil, err
			}
			return http.Header{"Proxy-Authorization": []string{"Bearer " + tok}}, nil
		},
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
	// Plain-HTTP targets carry Proxy-Authorization as a request header
	// (the proxy consumes it before forwarding). HTTPS targets use the
	// Transport's GetProxyConnectHeader instead, wired in
	// NewOnPremiseTransport. We do NOT clone the base Transport here:
	// cloning would create a fresh idle-connection pool on every call.
	r := req.Clone(req.Context())
	r.Header.Set("Proxy-Authorization", "Bearer "+tok)
	return t.base.RoundTrip(r)
}

// DefaultOnPremiseTimeout is the per-call timeout for proxied requests.
// On-prem R/3 systems can be slow; Service uses this and exposes no knob —
// wrap the returned http.Client if you need a different value.
const DefaultOnPremiseTimeout = 30 * time.Second
