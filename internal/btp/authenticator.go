package btp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// DestinationAuthenticator attaches credentials to an outbound request,
// using the configuration of a resolved Destination. Implementations are
// per-authentication-type (AuthBasic, AuthOAuth2ClientCredentials, …).
// This is the extension point for adding Auth0, SSO-on-top-of-Basic,
// principal propagation, etc. without touching the call site.
type DestinationAuthenticator interface {
	// AuthType is the Destination.Authentication value this authenticator
	// handles.
	AuthType() AuthType
	// Apply mutates req so the on-premise / target system accepts it. It
	// must NOT set Proxy-Authorization — that is the Connectivity service's
	// concern and is handled by the on-premise transport.
	Apply(ctx context.Context, req *http.Request, dest *Destination) error
}

// AuthenticatorRegistry dispatches Destination auth handling by type.
// The zero value is ready to use.
type AuthenticatorRegistry struct {
	mu       sync.RWMutex
	byType   map[AuthType]DestinationAuthenticator
	fallback DestinationAuthenticator
}

func (r *AuthenticatorRegistry) Register(a DestinationAuthenticator) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byType == nil {
		r.byType = map[AuthType]DestinationAuthenticator{}
	}
	r.byType[a.AuthType()] = a
}

// SetFallback registers a handler invoked when no registered AuthType matches.
// Useful to reject unknown auth types loudly rather than silently sending an
// unauthenticated request.
func (r *AuthenticatorRegistry) SetFallback(a DestinationAuthenticator) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fallback = a
}

func (r *AuthenticatorRegistry) Apply(ctx context.Context, req *http.Request, dest *Destination) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if a, ok := r.byType[dest.Authentication]; ok {
		return a.Apply(ctx, req, dest)
	}
	if r.fallback != nil {
		return r.fallback.Apply(ctx, req, dest)
	}
	return fmt.Errorf("no authenticator registered for destination auth type %q", dest.Authentication)
}

// DefaultAuthenticators returns a registry seeded with the auth types this
// MWE ships. Callers register additional ones (Auth0, OAuth2ClientCredentials,
// PrincipalPropagation) alongside these at startup.
func DefaultAuthenticators() *AuthenticatorRegistry {
	r := &AuthenticatorRegistry{}
	r.Register(NoAuthenticator{})
	r.Register(BasicAuthenticator{})
	// The Destination service sometimes returns an empty string for
	// destinations created without an Authentication field; treat it the
	// same as NoAuthentication rather than routing to the reject-fallback.
	r.Register(emptyAuthenticator{})
	r.SetFallback(rejectingAuthenticator{})
	return r
}

// ForwardedUserTokenKey is the request-context key under which the JWT
// middleware stashes the raw bearer token from the approuter.
// PrincipalPropagation authenticators read it to populate
// SAP-Connectivity-Authentication; regular authenticators ignore it.
type ForwardedUserTokenKey struct{}

type NoAuthenticator struct{}

func (NoAuthenticator) AuthType() AuthType { return AuthNone }
func (NoAuthenticator) Apply(context.Context, *http.Request, *Destination) error {
	return nil
}

type emptyAuthenticator struct{}

func (emptyAuthenticator) AuthType() AuthType { return "" }
func (emptyAuthenticator) Apply(context.Context, *http.Request, *Destination) error {
	return nil
}

type BasicAuthenticator struct{}

func (BasicAuthenticator) AuthType() AuthType { return AuthBasic }
func (BasicAuthenticator) Apply(_ context.Context, req *http.Request, dest *Destination) error {
	if dest.User == "" {
		return errors.New("BasicAuthentication destination is missing User")
	}
	// HTTP basic-auth cannot encode a colon in the username half of
	// user:pass. Fail loudly rather than produce a malformed header.
	if strings.Contains(dest.User, ":") {
		return errors.New("BasicAuthentication User must not contain ':'")
	}
	req.SetBasicAuth(dest.User, dest.Password)
	return nil
}

type rejectingAuthenticator struct{}

func (rejectingAuthenticator) AuthType() AuthType { return "*" }
func (rejectingAuthenticator) Apply(_ context.Context, _ *http.Request, dest *Destination) error {
	return fmt.Errorf("destination %q uses unsupported auth type %q; register a DestinationAuthenticator for it",
		dest.Name, dest.Authentication)
}
