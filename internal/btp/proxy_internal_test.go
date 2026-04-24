package btp

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"
)

// This is an internal test so it can reach into the unexported
// onPremiseRoundTripper and its base Transport to exercise the
// GetProxyConnectHeader closure directly. The closure fires on the
// HTTPS CONNECT-tunnel path, which is hard to observe via httptest
// without standing up a full CONNECT-aware proxy plus a trusted
// TLS cert chain; direct invocation is cheaper and tighter.

// Test_NewOnPremiseTransport_GetProxyConnectHeader_ReturnsBearer
// pins the happy path: the closure calls the provider with the
// ambient request context and returns a Proxy-Authorization header
// carrying the fetched token.
func Test_NewOnPremiseTransport_GetProxyConnectHeader_ReturnsBearer(t *testing.T) {
	var gotCtx context.Context
	rt, err := NewOnPremiseTransport(
		&ConnCredentials{OnPremiseProxyHost: "proxy.invalid", OnPremiseProxyPort: "8081"},
		func(req *http.Request) (string, error) {
			gotCtx = req.Context()
			return "connect-token", nil
		},
	)
	then.AssertThat(t, err, is.Nil())

	inner := rt.(*onPremiseRoundTripper)
	then.AssertThat(t, inner.base.GetProxyConnectHeader != nil, is.True())

	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "sentinel")
	hdr, err := inner.base.GetProxyConnectHeader(ctx, nil, "sap.internal:443")
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, hdr.Get("Proxy-Authorization"), is.EqualTo("Bearer connect-token"))

	// Provider was invoked with a request that carries our context —
	// that means ctx.Done() from the outer call propagates into the
	// token fetch, which is the whole reason we plumb ctx here.
	sentinel, _ := gotCtx.Value(ctxKey{}).(string)
	then.AssertThat(t, sentinel, is.EqualTo("sentinel"))
}

// Test_NewOnPremiseTransport_GetProxyConnectHeader_PropagatesErr
// covers the failure branch: if the token provider errors, the
// closure must surface it so net/http aborts the CONNECT dial
// instead of attempting an un-authenticated tunnel.
func Test_NewOnPremiseTransport_GetProxyConnectHeader_PropagatesErr(t *testing.T) {
	rt, err := NewOnPremiseTransport(
		&ConnCredentials{OnPremiseProxyHost: "proxy.invalid", OnPremiseProxyPort: "8081"},
		func(*http.Request) (string, error) { return "", errors.New("xsuaa down") },
	)
	then.AssertThat(t, err, is.Nil())

	inner := rt.(*onPremiseRoundTripper)
	_, err = inner.base.GetProxyConnectHeader(context.Background(), nil, "sap.internal:443")
	then.AssertThat(t, err, is.Not(is.Nil()))
}
