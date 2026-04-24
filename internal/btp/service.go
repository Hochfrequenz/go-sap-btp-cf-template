package btp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// OnPremCaller is the single-method contract Gin handlers should depend
// on for on-premise calls. *Service satisfies it in production; unit
// tests substitute a fake with one method to exercise handler logic
// without standing up the XSUAA / Destination / Cloud Connector stack.
//
// Signature mirrors Service.CallOnPremise exactly, so the interface is
// a no-op refactor for existing callers: swap the handler's parameter
// type from *Service to OnPremCaller and every call site keeps compiling.
type OnPremCaller interface {
	CallOnPremise(ctx context.Context, destName, method, pathSuffix string,
		headers http.Header, body io.Reader) (*http.Response, error)
}

// Service orchestrates a call to an on-premise SAP system behind the Cloud
// Connector: destination lookup → connectivity token → proxied call with the
// right auth headers. Safe for concurrent use.
//
// Service satisfies the OnPremCaller interface — handlers should accept
// that interface rather than *Service so unit tests can substitute a fake.
type Service struct {
	env            *Env
	tokens         *TokenFetcher
	authenticators *AuthenticatorRegistry
	mgmtClient     *http.Client
	onPremClient   *http.Client
}

// NewService wires a Service using the defaults: 10s management-call timeout,
// DefaultAuthenticators(), and a TokenFetcher with its own http.Client. Swap
// fields after construction if you need different timeouts or authenticators.
func NewService(env *Env) (*Service, error) {
	if env == nil {
		return nil, errors.New("nil env")
	}
	if env.Dest == nil {
		return nil, ErrNoDestinationBinding
	}
	if env.Conn == nil {
		return nil, ErrNoConnectivityBinding
	}

	tokens := NewTokenFetcher(nil)

	transport, err := NewOnPremiseTransport(env.Conn, func(req *http.Request) (string, error) {
		return tokens.Fetch(req.Context(), env.Conn.URL, env.Conn.ClientID, env.Conn.ClientSecret)
	})
	if err != nil {
		return nil, fmt.Errorf("on-premise transport: %w", err)
	}

	// The management client (XSUAA, Destination service) and the on-prem
	// client must be distinct: the on-prem one has a RoundTripper that
	// routes through the Connectivity proxy. Re-using it for management
	// calls would tunnel XSUAA token requests through the reverse proxy.
	return &Service{
		env:            env,
		tokens:         tokens,
		authenticators: DefaultAuthenticators(),
		mgmtClient:     &http.Client{Timeout: 10 * time.Second},
		onPremClient:   &http.Client{Transport: transport, Timeout: DefaultOnPremiseTimeout},
	}, nil
}

// Authenticators exposes the registry so callers can plug in Auth0 / SSO /
// PrincipalPropagation handlers at startup.
func (s *Service) Authenticators() *AuthenticatorRegistry { return s.authenticators }

// CallOnPremise runs the three-leg sequence for destination `destName` and
// forwards `method path` (path is appended to the destination's URL). On a
// 401 the connectivity token is invalidated and the call retried once,
// because bearer tokens can expire between cache check and on-prem receipt.
// 403 is NOT retried: it means "authenticated but not authorized", which a
// fresh token cannot fix and re-trying would mask real auth-policy bugs.
// The returned response body must be closed by the caller.
func (s *Service) CallOnPremise(ctx context.Context, destName, method, pathSuffix string, headers http.Header, body io.Reader) (*http.Response, error) {
	// Reject traversal attempts at the edge: a user-supplied `..` in
	// pathSuffix would otherwise travel unchanged into the on-prem URL,
	// where some SAP HTTP frontends resolve it against the destination
	// root and serve a resource the destination's author never meant to
	// expose. The same risk applies to percent-encoded variants
	// (`%2e%2e`, mixed case, `%2e.`, …) because some frontends decode
	// before applying path-resolution rules. Rather than chase decoder
	// quirks, reject any `%` in pathSuffix outright — the current API
	// surface only hands legitimate ASCII path segments to the Service;
	// revisit if a real UTF-8-in-path use case ever turns up.
	if strings.Contains(pathSuffix, "..") {
		return nil, fmt.Errorf("path suffix %q contains '..'; traversal is not allowed", pathSuffix)
	}
	if strings.Contains(pathSuffix, "%") {
		return nil, fmt.Errorf("path suffix %q contains '%%'; percent-encoded path segments are not allowed", pathSuffix)
	}

	destToken, err := s.tokens.Fetch(ctx, s.env.Dest.URL, s.env.Dest.ClientID, s.env.Dest.ClientSecret)
	if err != nil {
		return nil, fmt.Errorf("destination token: %w", err)
	}
	dest, err := LookupDestination(ctx, s.mgmtClient, s.env.Dest, destToken, destName)
	if err != nil {
		return nil, fmt.Errorf("destination lookup: %w", err)
	}

	resp, err := s.callOnce(ctx, dest, method, pathSuffix, headers, body)
	if err != nil {
		return nil, err
	}
	// Body may be read-once (io.Reader), so we only retry when the caller
	// handed us nil — safer than silently draining and re-seeking.
	if resp.StatusCode == http.StatusUnauthorized && body == nil {
		_ = resp.Body.Close()
		s.tokens.Invalidate(s.env.Conn.URL, s.env.Conn.ClientID)
		resp, err = s.callOnce(ctx, dest, method, pathSuffix, headers, nil)
	}
	return resp, err
}

func (s *Service) callOnce(ctx context.Context, dest *Destination, method, pathSuffix string, headers http.Header, body io.Reader) (*http.Response, error) {
	target := trimSlash(dest.URL)
	if pathSuffix != "" {
		if !strings.HasPrefix(pathSuffix, "/") {
			pathSuffix = "/" + pathSuffix
		}
		target += pathSuffix
	}

	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("build on-prem request: %w", err)
	}
	for k, vs := range headers {
		if skipForwardedHeader(k) {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// A neutral, self-identifying UA. The PHP/Python reference impersonates
	// a browser as a HF-SAP-specific filter workaround; that is not a BTP
	// requirement and should not be copied into new services.
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "go-sap-btp-cloud-foundry-mwe/0.1")
	}
	// Multi-CC routing header: only set when the destination carries a
	// LocationId. Setting it otherwise can make the Connectivity service
	// reject the call with "unknown location".
	if dest.CloudConnectorLocationID != "" {
		req.Header.Set("SAP-Connectivity-SCC-Location_ID", dest.CloudConnectorLocationID)
	}

	if err := s.authenticators.Apply(ctx, req, dest); err != nil {
		return nil, fmt.Errorf("apply destination auth: %w", err)
	}
	return s.onPremClient.Do(req)
}

// skipForwardedHeader filters headers that must not be forwarded from the
// inbound approuter request to the on-prem call:
//
//   - Authorization: the destination's authenticator sets the right value;
//     the inbound JWT is not a credential the on-prem system understands.
//   - Proxy-Authorization: the on-prem transport always sets a fresh token.
//   - hop-by-hop (Connection, Keep-Alive, TE, Trailer, Transfer-Encoding,
//     Upgrade, Proxy-Connect) per RFC 7230; forwarding them would confuse
//     the Connectivity proxy.
//   - Host: the net/http library derives the right value from the target
//     URL; forwarding the approuter's Host breaks virtual-host routing on
//     the SAP side.
//   - Cookie: approuter session cookies are scoped to the approuter domain;
//     leaking them to the on-prem SAP system is at best useless and at
//     worst a PII concern.
func skipForwardedHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization",
		"connection",
		"keep-alive",
		"proxy-authorization",
		"proxy-connect",
		"te",
		"trailer",
		"transfer-encoding",
		"upgrade",
		"host",
		"cookie":
		return true
	}
	return false
}

// ProxyHandler is a Gin handler exposing /:destination/*path as a transparent
// pass-through. Useful for an MWE demo; production handlers should wrap
// CallOnPremise with endpoint-specific logic.
func (s *Service) ProxyHandler(c *gin.Context) {
	destName := c.Param("destination")
	suffix := c.Param("path")

	resp, err := s.CallOnPremise(c.Request.Context(), destName, c.Request.Method, suffix, c.Request.Header, c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for k, vs := range resp.Header {
		if skipForwardedHeader(k) {
			continue
		}
		for _, v := range vs {
			c.Writer.Header().Add(k, v)
		}
	}
	c.Writer.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(c.Writer, resp.Body); err != nil {
		// Failure here usually means the downstream client disconnected;
		// the response status is already written so we cannot recover — log
		// it so operators can spot patterns of SAP responses being cut off.
		slog.Default().Warn("copying on-prem response to client failed",
			"destination", destName,
			"err", err,
		)
	}
}
