package btp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// AuthType enumerates the values SAP BTP puts in a destination's
// Authentication property. A typed alias keeps the authenticator registry
// compile-safe — a typo in a constant fails to build instead of silently
// falling through to the reject-fallback at runtime.
type AuthType string

const (
	AuthNone                    AuthType = "NoAuthentication"
	AuthBasic                   AuthType = "BasicAuthentication"
	AuthOAuth2ClientCredentials AuthType = "OAuth2ClientCredentials"
	AuthOAuth2UserTokenExchange AuthType = "OAuth2UserTokenExchange"
	AuthOAuth2JWTBearer         AuthType = "OAuth2JWTBearer"
	AuthOAuth2SAMLBearer        AuthType = "OAuth2SAMLBearerAssertion"
	AuthPrincipalPropagation    AuthType = "PrincipalPropagation"
	AuthSAMLAssertion           AuthType = "SamlAssertion"
	AuthClientCertificate       AuthType = "ClientCertificateAuthentication"
)

// ProxyType enumerates SAP BTP destination proxy modes. Only OnPremise
// traffic must route through the Connectivity service's reverse proxy; for
// Internet and PrivateLink the transport is a plain outbound call. The
// Service uses this to decide whether the Connectivity binding must be
// present at all.
type ProxyType string

const (
	ProxyOnPremise   ProxyType = "OnPremise"
	ProxyInternet    ProxyType = "Internet"
	ProxyPrivateLink ProxyType = "PrivateLink"
)

// ErrDestinationNotFound wraps a 404 from the Destination service so callers
// can branch on it via errors.Is. Any other 4xx/5xx returns a plain error.
var ErrDestinationNotFound = errors.New("destination not found")

// Destination is the subset of a Destination-service record this library
// reads. Additional Destination-service properties (sap-client, WebIDEUsage,
// etc.) are intentionally dropped: callers that need them can re-fetch the
// raw JSON or extend this struct.
type Destination struct {
	Name                     string    `json:"Name"`
	Type                     string    `json:"Type"`
	URL                      string    `json:"URL"`
	Authentication           AuthType  `json:"Authentication"`
	ProxyType                ProxyType `json:"ProxyType"`
	User                     string    `json:"User,omitempty"`
	Password                 string    `json:"Password,omitempty"`
	CloudConnectorLocationID string    `json:"CloudConnectorLocationId,omitempty"`
}

// IsOnPremise reports whether the destination targets a system reachable
// only via the Connectivity service's reverse proxy.
func (d *Destination) IsOnPremise() bool {
	return d.ProxyType == ProxyOnPremise
}

// destinationEnvelope matches the /destination-configuration/v1 response.
// The service wraps the destination in `destinationConfiguration` and adds
// siblings like `authTokens` we do not use here.
type destinationEnvelope struct {
	DestinationConfiguration Destination `json:"destinationConfiguration"`
}

// LookupDestination fetches a destination by name from the Destination
// service. It calls the generic /destinations/{name} endpoint, which searches
// instance- and subaccount-scope and is recommended by SAP over the
// scope-specific variants (`/instanceDestinations/`, `/subaccountDestinations/`).
func LookupDestination(ctx context.Context, httpClient *http.Client, cred *DestCredentials, bearer, name string) (*Destination, error) {
	if cred == nil {
		return nil, ErrNoDestinationBinding
	}
	if name == "" {
		return nil, errors.New("destination name is required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	endpoint := fmt.Sprintf("%s/destination-configuration/v1/destinations/%s",
		trimSlash(cred.URI), url.PathEscape(name))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build destination request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("destination lookup: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Cap the body read: the Destination service is trusted but a misrouted
	// response (proxy, DNS, etc.) could otherwise balloon memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMgmtResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read destination response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %q", ErrDestinationNotFound, name)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("destination service returned %d for %q", resp.StatusCode, name)
	}

	// The envelope shape is documented; we also accept the direct shape
	// because older service surfaces and test fixtures sometimes return it.
	var env destinationEnvelope
	if err := json.Unmarshal(body, &env); err == nil && env.DestinationConfiguration.URL != "" {
		return &env.DestinationConfiguration, nil
	}
	var direct Destination
	if err := json.Unmarshal(body, &direct); err == nil && direct.URL != "" {
		return &direct, nil
	}
	return nil, fmt.Errorf("destination %q response did not contain a URL", name)
}

// maxMgmtResponseBytes caps the body read from XSUAA and the Destination
// service. 1 MiB is well above any legitimate response these services send.
const maxMgmtResponseBytes = 1 << 20
