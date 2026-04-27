package btp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/singleflight"
)

// OnPremCaller is the single-method contract Gin handlers should depend
// on for read-only on-premise calls. *Service satisfies it in production;
// unit tests substitute a fake with one method to exercise handler logic
// without standing up the XSUAA / Destination / Cloud Connector stack.
//
// Signature mirrors Service.CallOnPremise exactly, so the interface is
// a no-op refactor for existing callers: swap the handler's parameter
// type from *Service to OnPremCaller and every call site keeps compiling.
type OnPremCaller interface {
	CallOnPremise(ctx context.Context, destName, method, pathSuffix string,
		headers http.Header, body io.Reader) (*http.Response, error)
}

// OnPremMutator is the mutating-call contract handlers should depend
// on for writes (POST / PUT / DELETE / PATCH against SAP endpoints
// that enforce CSRF). Implementations perform the X-CSRF-Token
// handshake transparently — callers just invoke
// CallOnPremiseMutating and get a response that already went through
// fetch-token → attach-token-and-cookies → retry-once-on-403.
//
// Handlers that mix reads and writes declare a composite interface
// at the usage site:
//
//	type InvoiceClient interface {
//	    btp.OnPremCaller
//	    btp.OnPremMutator
//	}
//
// *Service satisfies both so wiring stays unchanged.
type OnPremMutator interface {
	CallOnPremiseMutating(ctx context.Context, destName, method, pathSuffix string,
		headers http.Header, body io.Reader) (*http.Response, error)
}

// Service orchestrates a call to an on-premise SAP system behind the Cloud
// Connector: destination lookup → connectivity token → proxied call with the
// right auth headers. Safe for concurrent use.
//
// Service satisfies the OnPremCaller interface — handlers should accept
// that interface rather than *Service so unit tests can substitute a fake.
type Service struct {
	env                     *Env
	tokens                  *TokenFetcher
	authenticators          *AuthenticatorRegistry
	mgmtClient              *http.Client
	onPremClient            *http.Client
	userAgent               string
	onPremResponseSizeLimit int64

	// CSRF handshake state — see CallOnPremiseMutating for the dance.
	// csrfFetchPath is the GET path used to retrieve a fresh token.
	// csrfStates caches token + SAP session cookies keyed by
	// destination name; csrfMu guards only the cache read/write.
	// csrfGroup dedupes concurrent fetches for the same destination
	// so N goroutines racing into a cold cache produce ONE SAP
	// round-trip, not N.
	csrfFetchPath string
	csrfMu        sync.Mutex
	csrfStates    map[string]*csrfState
	csrfGroup     singleflight.Group
}

// csrfState holds one destination's cached X-CSRF-Token and the SAP
// session cookies that came back with it. The SAP side pins the
// token to the session; both travel together or not at all.
type csrfState struct {
	token   string
	cookies []*http.Cookie
}

// DefaultMgmtTimeout is the per-call timeout for management calls (XSUAA
// token exchange, Destination service lookup). Override with
// WithMgmtTimeout when a specific service needs a different value.
const DefaultMgmtTimeout = 10 * time.Second

// DefaultUserAgent is the User-Agent header sent on outbound on-prem
// requests when no WithUserAgent option is supplied. Forked services
// should set an explicit UA so traces on the SAP side can distinguish
// the caller from the template — see cmd/server/main.go for the
// debug.ReadBuildInfo pattern.
const DefaultUserAgent = "go-sap-btp-cf-template/0.1"

// DefaultCSRFFetchPath is the SAP path CallOnPremiseMutating calls
// with `X-CSRF-Token: Fetch` to retrieve a token. Defaults to ADT's
// bootstrap endpoint because that's the one `adtler` uses and the
// one most forks will hit first. Override with WithCSRFFetchPath if
// the destination doesn't route /sap/bc/adt/* — a bare GET against
// a REST endpoint on the same ICF node works too.
const DefaultCSRFFetchPath = "/sap/bc/adt/discovery"

// DefaultOnPremResponseSizeLimit caps how much of an on-prem response
// body Service will let a caller read. The Cloud Connector tunnel
// itself is trusted, but a misbehaving SAP backend, a misrouted
// hostname, or — for customer-managed CC topologies — a MITM between
// CC and SAP could otherwise stream gigabytes into the app's 128 MiB
// CF memory quota. 10 MiB is well above any legitimate typed-API
// response (ADT XML, a few hundred KiB of FI line items, etc.).
//
// On overshoot, the wrapped resp.Body returns ErrOnPremResponseTooLarge
// from Read; the caller's io.ReadAll surfaces it and the handler
// translates to btp.CodeUpstreamUnreachable (502).
//
// Override with WithOnPremResponseSizeLimit when a specific fork has
// a route that legitimately returns more.
const DefaultOnPremResponseSizeLimit int64 = 10 << 20

// ErrOnPremResponseTooLarge is returned by reads on an on-prem response
// body once the per-call cap configured via WithOnPremResponseSizeLimit
// (default DefaultOnPremResponseSizeLimit) is exceeded. Callers using
// io.ReadAll surface this error from their ReadAll call; the typical
// translation in handlers is btp.CodeUpstreamUnreachable (502).
var ErrOnPremResponseTooLarge = errors.New("on-prem response exceeds configured size limit")

// ServiceOption configures NewService. Use WithUserAgent, WithMgmtTimeout,
// and WithOnPremiseTimeout to tune the defaults; zero options keeps the
// built-in values.
type ServiceOption func(*serviceOptions)

type serviceOptions struct {
	userAgent               string
	mgmtTimeout             time.Duration
	onPremiseTimeout        time.Duration
	csrfFetchPath           string
	onPremResponseSizeLimit int64
}

// WithUserAgent sets the User-Agent header for outbound on-prem requests.
// An empty string resets to DefaultUserAgent.
func WithUserAgent(ua string) ServiceOption {
	return func(o *serviceOptions) { o.userAgent = ua }
}

// WithMgmtTimeout overrides the management-call timeout (XSUAA, Destination
// service lookup). Zero resets to DefaultMgmtTimeout.
func WithMgmtTimeout(d time.Duration) ServiceOption {
	return func(o *serviceOptions) { o.mgmtTimeout = d }
}

// WithOnPremiseTimeout overrides the per-call timeout for on-prem requests.
// Zero resets to DefaultOnPremiseTimeout.
func WithOnPremiseTimeout(d time.Duration) ServiceOption {
	return func(o *serviceOptions) { o.onPremiseTimeout = d }
}

// WithCSRFFetchPath overrides the SAP path CallOnPremiseMutating calls
// to pick up a fresh X-CSRF-Token. Empty string resets to
// DefaultCSRFFetchPath. Change this if your destination doesn't host
// /sap/bc/adt/* — pass the path of any GET-capable SAP endpoint that
// responds to `X-CSRF-Token: Fetch` with a token and a session cookie.
func WithCSRFFetchPath(path string) ServiceOption {
	return func(o *serviceOptions) { o.csrfFetchPath = path }
}

// WithOnPremResponseSizeLimit caps the per-call on-prem response body.
// Reads that would push a single response past `bytes` return
// ErrOnPremResponseTooLarge from the wrapped resp.Body. Zero resets to
// DefaultOnPremResponseSizeLimit. Pass a larger value if a single fork
// route legitimately returns more.
func WithOnPremResponseSizeLimit(bytes int64) ServiceOption {
	return func(o *serviceOptions) { o.onPremResponseSizeLimit = bytes }
}

// NewService wires a Service using the defaults listed in DefaultMgmtTimeout,
// DefaultOnPremiseTimeout, and DefaultUserAgent, plus DefaultAuthenticators()
// and a TokenFetcher with its own http.Client. Pass ServiceOptions to tune
// timeouts or the outbound User-Agent.
func NewService(env *Env, opts ...ServiceOption) (*Service, error) {
	if env == nil {
		return nil, errors.New("nil env")
	}
	if env.Dest == nil {
		return nil, ErrNoDestinationBinding
	}
	if env.Conn == nil {
		return nil, ErrNoConnectivityBinding
	}

	o := serviceOptions{
		userAgent:               DefaultUserAgent,
		mgmtTimeout:             DefaultMgmtTimeout,
		onPremiseTimeout:        DefaultOnPremiseTimeout,
		csrfFetchPath:           DefaultCSRFFetchPath,
		onPremResponseSizeLimit: DefaultOnPremResponseSizeLimit,
	}
	for _, opt := range opts {
		opt(&o)
	}
	// Treat zero/empty field values as "use the default" so callers can
	// compose a struct of options without knowing which fields they care
	// about — e.g. WithMgmtTimeout(0) falls back to the constant.
	if o.userAgent == "" {
		o.userAgent = DefaultUserAgent
	}
	if o.mgmtTimeout == 0 {
		o.mgmtTimeout = DefaultMgmtTimeout
	}
	if o.onPremiseTimeout == 0 {
		o.onPremiseTimeout = DefaultOnPremiseTimeout
	}
	if o.csrfFetchPath == "" {
		o.csrfFetchPath = DefaultCSRFFetchPath
	}
	if o.onPremResponseSizeLimit == 0 {
		o.onPremResponseSizeLimit = DefaultOnPremResponseSizeLimit
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
		env:                     env,
		tokens:                  tokens,
		authenticators:          DefaultAuthenticators(),
		mgmtClient:              &http.Client{Timeout: o.mgmtTimeout},
		onPremClient:            &http.Client{Transport: transport, Timeout: o.onPremiseTimeout},
		userAgent:               o.userAgent,
		onPremResponseSizeLimit: o.onPremResponseSizeLimit,
		csrfFetchPath:           o.csrfFetchPath,
		csrfStates:              map[string]*csrfState{},
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
//
// The returned resp.Body is capped at DefaultOnPremResponseSizeLimit
// (override via WithOnPremResponseSizeLimit). A read past the cap
// returns ErrOnPremResponseTooLarge; an io.ReadAll caller sees that
// error from the call and should surface it as 502
// (CodeUpstreamUnreachable). The cap protects against an SAP/CC
// misroute streaming gigabytes into the CF memory quota.
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

// CallOnPremiseMutating is the write counterpart of CallOnPremise.
// SAP ICF endpoints that accept POST / PUT / DELETE / PATCH typically
// require a CSRF token in `X-CSRF-Token`, together with the SAP
// session cookies set by the server when the token was first minted.
// This method runs that handshake for you:
//
//  1. Look up the token + cookies for `destName` in the Service's
//     CSRF cache; on a miss, GET the configured CSRF fetch path
//     (WithCSRFFetchPath, default DefaultCSRFFetchPath) with
//     `X-CSRF-Token: Fetch` and cache what comes back.
//  2. Attach the cached token and cookies to the mutating call and
//     send it.
//  3. On a 403 carrying `X-CSRF-Token: Required` (SAP's signal that
//     the server-side session was recycled), invalidate the cache,
//     re-fetch once, and retry the mutating call a single time.
//
// The request body is buffered up-front so the retry can re-read it.
// If your body is too large to buffer, write your own handshake on
// top of CallOnPremise rather than using this helper.
//
// Returns the final response (body must be closed by the caller) or
// the underlying error. A 403 that is NOT a CSRF-required signal
// (i.e. a real authorization failure) surfaces to the caller as-is,
// no retry.
//
// As with CallOnPremise, resp.Body is capped at
// DefaultOnPremResponseSizeLimit (override via WithOnPremResponseSizeLimit);
// a read past the cap returns ErrOnPremResponseTooLarge.
func (s *Service) CallOnPremiseMutating(ctx context.Context, destName, method, pathSuffix string, headers http.Header, body io.Reader) (*http.Response, error) {
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("read mutating body: %w", err)
		}
	}

	state, err := s.csrfStateFor(ctx, destName)
	if err != nil {
		return nil, err
	}
	resp, err := s.callMutatingOnce(ctx, destName, method, pathSuffix, headers, bodyBytes, state)
	if err != nil {
		return nil, err
	}
	// SAP signals a CSRF-specific rejection via 403 + X-CSRF-Token: Required.
	// Other 403s are real authz failures and must NOT be retried — they
	// indicate the technical user lacks the object-level authorization,
	// which fresh tokens cannot fix.
	if resp.StatusCode == http.StatusForbidden &&
		strings.EqualFold(resp.Header.Get("X-CSRF-Token"), "Required") {
		_ = resp.Body.Close()
		s.invalidateCSRF(destName)
		state, err = s.csrfStateFor(ctx, destName)
		if err != nil {
			return nil, err
		}
		resp, err = s.callMutatingOnce(ctx, destName, method, pathSuffix, headers, bodyBytes, state)
	}
	return resp, err
}

// callMutatingOnce attaches the given CSRF token + cookies to a copy
// of headers and delegates to CallOnPremise. Sharing the read path
// means all the three-leg plumbing (destination lookup, 401 retry,
// authenticator application) stays in one place.
func (s *Service) callMutatingOnce(ctx context.Context, destName, method, pathSuffix string,
	headers http.Header, body []byte, state *csrfState) (*http.Response, error) {
	out := http.Header{}
	for k, vs := range headers {
		for _, v := range vs {
			out.Add(k, v)
		}
	}
	out.Set("X-CSRF-Token", state.token)
	// RFC 6265 says clients send one Cookie header with cookies joined
	// by "; ". Go lets you Add multiple Cookie headers and the stdlib
	// will send them all, but servers reading via r.Header.Get("Cookie")
	// only see the first value — combine them upfront to avoid that
	// foot-gun on the SAP side.
	if len(state.cookies) > 0 {
		parts := make([]string, 0, len(state.cookies))
		for _, c := range state.cookies {
			parts = append(parts, c.Name+"="+c.Value)
		}
		out.Set("Cookie", strings.Join(parts, "; "))
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	return s.CallOnPremise(ctx, destName, method, pathSuffix, out, r)
}

// csrfStateFor returns the cached CSRF state for destName, fetching
// it if absent. Concurrent first-misses for the same destination are
// deduped through singleflight — N goroutines racing into a cold
// cache trigger exactly ONE SAP round-trip, all N see the same state.
// The short mutex sections only guard the in-memory map; we never
// hold them across the network call.
func (s *Service) csrfStateFor(ctx context.Context, destName string) (*csrfState, error) {
	// Fast path: cached.
	s.csrfMu.Lock()
	state, ok := s.csrfStates[destName]
	s.csrfMu.Unlock()
	if ok {
		return state, nil
	}
	// Slow path: singleflight-dedupe the fetch.
	v, err, _ := s.csrfGroup.Do(destName, func() (any, error) {
		// Another goroutine's Do() may have populated the cache by
		// the time this callback runs — re-check.
		s.csrfMu.Lock()
		if existing, ok := s.csrfStates[destName]; ok {
			s.csrfMu.Unlock()
			return existing, nil
		}
		s.csrfMu.Unlock()

		fetched, err := s.fetchCSRF(ctx, destName)
		if err != nil {
			return nil, err
		}
		s.csrfMu.Lock()
		s.csrfStates[destName] = fetched
		s.csrfMu.Unlock()
		return fetched, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*csrfState), nil
}

// fetchCSRF performs the X-CSRF-Token: Fetch handshake against the
// configured fetch path. Separated out so csrfStateFor's singleflight
// callback can run it without holding the state mutex — if the mutex
// wrapped the network call, a slow SAP would block cache reads for
// OTHER destinations too.
func (s *Service) fetchCSRF(ctx context.Context, destName string) (*csrfState, error) {
	// Accept: */* matters — some SAP ICF nodes return 400 without it
	// on the Fetch handshake. We do not read the body anyway (the
	// token travels in the response header), so widest-possible Accept
	// is correct.
	resp, err := s.CallOnPremise(ctx, destName, http.MethodGet, s.csrfFetchPath,
		http.Header{
			"X-CSRF-Token": []string{"Fetch"},
			"Accept":       []string{"*/*"},
		}, nil)
	if err != nil {
		return nil, fmt.Errorf("csrf fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodySnippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("csrf fetch %s returned status %d: %s",
			s.csrfFetchPath, resp.StatusCode, strings.TrimSpace(string(bodySnippet)))
	}
	token := resp.Header.Get("X-CSRF-Token")
	if token == "" {
		return nil, fmt.Errorf("csrf fetch %s returned empty X-CSRF-Token header", s.csrfFetchPath)
	}
	return &csrfState{token: token, cookies: resp.Cookies()}, nil
}

// invalidateCSRF drops the cached token + cookies for destName. Called
// when SAP signals a CSRF-specific rejection (403 + X-CSRF-Token:
// Required) so the next CallOnPremiseMutating refreshes.
func (s *Service) invalidateCSRF(destName string) {
	s.csrfMu.Lock()
	delete(s.csrfStates, destName)
	s.csrfMu.Unlock()
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
		if strings.EqualFold(k, "Cookie") {
			// Cookies get per-cookie filtering — approuter session
			// cookies (JSESSIONID etc.) are dropped, SAP session
			// cookies (SAP_SESSIONID_* / sap-usercontext) pass through
			// so CallOnPremiseMutating can inject them for the CSRF
			// handshake. See filterForwardedCookies below.
			for _, v := range vs {
				if kept := filterForwardedCookies(v); kept != "" {
					req.Header.Add("Cookie", kept)
				}
			}
			continue
		}
		if skipForwardedHeader(k) {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// A neutral, self-identifying UA. The PHP/Python reference impersonates
	// a browser as a HF-SAP-specific filter workaround; that is not a BTP
	// requirement and should not be copied into new services. The concrete
	// value comes from WithUserAgent (or DefaultUserAgent) — forked services
	// should pass WithUserAgent so SAP-side traces distinguish the caller
	// from the template.
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", s.userAgent)
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
	resp, err := s.onPremClient.Do(req)
	if err != nil {
		return nil, err
	}
	// Cap the readable body so a misbehaving SAP / misrouted endpoint /
	// MITM in a customer-managed CC topology cannot stream gigabytes
	// into the app's 128 MiB CF memory quota. The wrap is transparent
	// to honest callers — io.ReadAll on a small response works as
	// before; an attempt to read past the cap returns
	// ErrOnPremResponseTooLarge from Read, which the caller's io.ReadAll
	// surfaces as a typed error.
	resp.Body = newLimitedOnPremBody(resp.Body, s.onPremResponseSizeLimit)
	return resp, nil
}

// limitedOnPremBody wraps an http.Response.Body to enforce a maximum
// total read size. Reads up to limit bytes pass through unchanged;
// the (limit+1)-th byte triggers ErrOnPremResponseTooLarge from Read.
// The wrapper caps the per-Read slice so callers buffering the body
// (io.ReadAll) cannot grow the buffer past limit before hitting the
// error — the memory invariant the cap exists for.
type limitedOnPremBody struct {
	rc    io.ReadCloser
	limit int64
	read  int64
}

func newLimitedOnPremBody(rc io.ReadCloser, limit int64) io.ReadCloser {
	return &limitedOnPremBody{rc: rc, limit: limit}
}

func (b *limitedOnPremBody) Read(p []byte) (int, error) {
	if b.read > b.limit {
		return 0, ErrOnPremResponseTooLarge
	}
	// Cap the slice so we read at most one byte past the limit — that's
	// what proves the body has more than `limit` content. Without this
	// cap a single Read of e.g. 32 KiB into a 1-byte-remaining budget
	// would buffer 32 KiB before the error fires.
	remaining := b.limit - b.read + 1
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := b.rc.Read(p)
	b.read += int64(n)
	if b.read > b.limit {
		return n, ErrOnPremResponseTooLarge
	}
	return n, err
}

func (b *limitedOnPremBody) Close() error { return b.rc.Close() }

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
//
// Cookie is deliberately NOT in the drop list: callOnce does per-cookie
// filtering via filterForwardedCookies so SAP session cookies
// (SAP_SESSIONID_* / sap-usercontext) can flow through as part of the
// CSRF handshake in CallOnPremiseMutating, while approuter cookies
// (JSESSIONID etc.) are still dropped.
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
		"host":
		return true
	}
	return false
}

// filterForwardedCookies reads one inbound `Cookie:` header value and
// returns a `Cookie:` value containing only cookies that are safe to
// forward to the SAP side:
//
//   - SAP_SESSIONID_<sid>_<client> — the SAP session cookie set by the
//     server after a CSRF-fetch; mutating calls must echo it back or
//     the ICF rejects them.
//   - sap-usercontext — SAP's per-user context handle.
//
// Everything else (approuter's JSESSIONID, tracking cookies, etc.) is
// dropped. Empty return value means no `Cookie:` header should travel
// at all — callers must skip the Add in that case.
func filterForwardedCookies(cookieHeader string) string {
	var kept []string
	for _, part := range strings.Split(cookieHeader, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name := part
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if strings.HasPrefix(name, "SAP_SESSIONID_") || name == "sap-usercontext" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "; ")
}

// isMutatingMethod returns true for HTTP methods that SAP's ICF
// typically gates with CSRF. GET / HEAD / OPTIONS are read-only and
// pass through unaltered; everything else triggers the handshake.
func isMutatingMethod(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
		return true
	}
	return false
}

// ProxyHandler is a Gin handler exposing /:destination/*path as a transparent
// pass-through. Useful for an MWE demo; production handlers should wrap
// CallOnPremise / CallOnPremiseMutating with endpoint-specific logic.
//
// The method gate below routes mutating requests through
// CallOnPremiseMutating so that SAP endpoints enforcing CSRF work
// out of the box — a bare POST / PUT / DELETE / PATCH against
// /api/sap/<destination>/sap/bc/adt/... would otherwise fail with
// 403 X-CSRF-Token: Required on every call.
func (s *Service) ProxyHandler(c *gin.Context) {
	destName := c.Param("destination")
	suffix := c.Param("path")

	var (
		resp *http.Response
		err  error
	)
	if isMutatingMethod(c.Request.Method) {
		resp, err = s.CallOnPremiseMutating(c.Request.Context(), destName, c.Request.Method, suffix, c.Request.Header, c.Request.Body)
	} else {
		resp, err = s.CallOnPremise(c.Request.Context(), destName, c.Request.Method, suffix, c.Request.Header, c.Request.Body)
	}
	if err != nil {
		AbortError(c, http.StatusBadGateway, CodeUpstreamUnreachable,
			"on-premise call failed", err)
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
		// Almost always a client disconnect mid-stream; nothing for
		// operators to act on. Emit at DEBUG so a developer chasing a
		// specific cut-off case can raise the level locally, but the
		// production INFO stream stays quiet on normal disconnects.
		// Deliberately not WARN — see README §"Logging — two levels,
		// no warnings" for why this template does not use that level.
		slog.DebugContext(c.Request.Context(),
			"copying on-prem response to client failed",
			"destination", destName,
			"err", err,
		)
	}
}
