// Package btp wires the SAP BTP-specific plumbing this template
// ships with: XSUAA JWT validation, Destination-service lookup, the
// Cloud Connector reverse-proxy dance, pluggable destination-level
// authenticators, a typed API error envelope with request-ID
// correlation, and transparent CSRF handshaking for mutating calls.
//
// # Public API surface vs. template-internal
//
// This file freezes the contract every fork is allowed to lean on.
// Handlers, examples, and cmd/* wiring may depend ONLY on the
// identifiers listed below — anything not named here is template-
// internal and may move, rename, or disappear in any PR.
//
// The list exists so a future extract into an independent
// go.mod-module (`hochfrequenz/btp-go` or similar) is cheap: the
// hard part — "who depends on what that we did not mean to expose" —
// is answered up front. CI gates in .github/workflows/template-
// guards.yml enforce the "depend only through these" rule at PR
// time, not after the fact.
//
// # Library-intent surface
//
// ## Service & on-prem plumbing
//
//   - [Service], [NewService]
//   - [OnPremCaller]            — interface handlers should depend on for reads
//   - [OnPremMutator]           — interface handlers should depend on for writes (CSRF)
//   - [*Service.ProxyHandler]   — Gin handler exposed for the demo /api/sap/ route
//   - [NewOnPremiseTransport], [ConnTokenProvider]
//   - [ServiceOption], [WithUserAgent], [WithMgmtTimeout], [WithOnPremiseTimeout], [WithCSRFFetchPath]
//   - [DefaultOnPremiseTimeout], [DefaultMgmtTimeout], [DefaultUserAgent], [DefaultCSRFFetchPath]
//
// ## JWT validation & middleware
//
//   - [JWTValidator], [NewJWTValidator]
//   - [*JWTValidator.Middleware], [*JWTValidator.Parse]
//   - [ForwardedUserTokenKey]
//   - [RequestID], [RequestIDFromContext], [RequestIDHeader], [RequestIDContextKey]
//   - [RequireScope]
//
// ## Error envelope
//
//   - [AbortError]              — the single blessed writer for error responses
//   - [ErrorEnvelope], [ErrorDetail], [ErrorCode]
//   - [CodeInvalidRequest], [CodeUnauthorized], [CodeForbidden],
//     [CodeNotFound], [CodeUpstreamUnreachable], [CodeInternal]
//
// ## Environment / VCAP bindings
//
//   - [Env], [LoadEnv]
//   - [XSUAACredentials], [DestCredentials], [ConnCredentials]
//   - [Destination], [LookupDestination]
//
// ## Authenticator registry
//
//   - [AuthenticatorRegistry], [*AuthenticatorRegistry.Register]
//   - [DestinationAuthenticator] interface
//   - [DefaultAuthenticators], [NoAuthenticator], [BasicAuthenticator]
//   - [AuthType], [ProxyType]
//
// ## Tokens
//
//   - [TokenFetcher], [NewTokenFetcher]
//   - [TokenRefreshLeeway]
//
// ## Sentinel errors
//
//   - [ErrNoDestinationBinding], [ErrNoConnectivityBinding], [ErrNoXSUAABinding]
//   - [ErrDestinationNotFound], [ErrNotInCloudFoundry]
//
// # Template-internal (do NOT depend on from outside this package)
//
// Everything else, including (but not limited to): unexported types
// and helpers (csrfState, serviceOptions, onPremiseRoundTripper,
// callOnce, callMutatingOnce, fetchCSRF, csrfStateFor, invalidateCSRF,
// skipForwardedHeader, filterForwardedCookies, isMutatingMethod,
// requestIDCtxKey, newRequestID, extractScopes, trimSlash), test
// helpers, and any identifier added in future without a corresponding
// line above.
//
// # Trigger to revisit the extract decision
//
// Open a follow-up "extract btp into public module" issue when ANY
// of:
//   - A third HF service is forking this repo for its wiring.
//   - One of today's consumers wants to pin a btp version
//     independently of the template's other churn.
//   - A non-HF fork asks to consume the btp code (today blocked by
//     the internal/ path; the ask itself is the signal).
package btp
