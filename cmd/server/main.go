// Command server is the HTTP entry point for the BTP MWE.
// It boots a Gin router with XSUAA JWT middleware and exposes one demo
// endpoint that proxies calls through the Cloud Connector to a named
// destination.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humagin"
	"github.com/gin-gonic/gin"

	"github.com/hochfrequenz/go-sap-btp-cf-template/examples/adtcheckrun"
	"github.com/hochfrequenz/go-sap-btp-cf-template/examples/adtdiscovery"
	"github.com/hochfrequenz/go-sap-btp-cf-template/internal/btp"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevelFromEnv()}))
	slog.SetDefault(logger)

	env, err := btp.LoadEnv()
	if err != nil {
		logger.Error("cloud foundry environment not available; refusing to start", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	validator, err := btp.NewJWTValidator(ctx, env.XSUAA)
	if err != nil {
		logger.Error("xsuaa jwt validator init failed", "err", err)
		os.Exit(1)
	}

	svc, err := btp.NewService(env, btp.WithUserAgent(buildUserAgent()))
	if err != nil {
		logger.Error("btp service init failed", "err", err)
		os.Exit(1)
	}

	r := buildRouter(validator, svc, svc, logger)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	// The four timeouts cover four distinct slow-client failure modes;
	// any one missing leaves a goroutine leak vector against a backend
	// that is directly internet-reachable on its .cfapps.* route (not
	// only behind the approuter):
	//
	//   - ReadHeaderTimeout (10s):  Slowloris on request headers.
	//   - ReadTimeout       (60s):  Slow-body POSTs (client → server).
	//   - WriteTimeout     (900s):  Bounds total handler runtime, since
	//                               Go starts WriteTimeout at header-read,
	//                               not at first write.
	//   - IdleTimeout      (120s):  Keep-alive sockets parked indefinitely.
	//
	// WriteTimeout is sized for an on-prem SAP system that is usually
	// slow under load. An ADT call routed through the Cloud Connector +
	// CSRF handshake regularly takes minutes, and observed worst case is
	// ~5 minutes per leg.
	//
	// 900s (15 minutes) is intentionally HIGHER than the 10-minute
	// btp.DefaultOnPremiseTimeout. WriteTimeout is one budget covering
	// the *whole* handler run (CSRF handshake leg + main on-prem POST +
	// response write); the on-prem client timeout is a budget per call.
	// Setting WriteTimeout = on-prem-timeout would let WriteTimeout race
	// the on-prem timeout under CSRF — sometimes producing a clean
	// upstream-unreachable envelope, sometimes a server-side timeout.
	// 900s gives 5 minutes of headroom over a single full-budget on-prem
	// call so the on-prem timeout reliably fires first and the client
	// sees one stable failure mode. The 900s also lines up with most CF
	// Gorouter request-timeout defaults, so values above this are moot
	// without platform-side changes.
	//
	// WriteTimeout is the only inner cap; if it's reached, that means
	// the handler genuinely stalled or made multiple consecutive
	// long-running on-prem calls — both pathological enough that
	// timing out is the right answer.
	//
	// ReadTimeout stays at 60s because it bounds *client → server* body
	// transfer; the request bodies the template ships are small JSON, and
	// 60s on a slow body upload is already deep into Slowloris territory.
	//
	// A handler that legitimately needs an even longer write window
	// (large-file streaming, long-poll) should override per-request via
	// http.NewResponseController(w).SetWriteDeadline(...) rather than
	// raising this default — looser global timeouts re-open the
	// slow-client surface for every other route.
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      900 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Runtime server failures land on serverErr; signal shutdowns land
	// on ctx.Done(). Select over both so we have ONE exit path with one
	// log line — not two (goroutine-local + main) that could produce
	// different log shapes for the same class of event.
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		logger.Error("server terminated unexpectedly", "err", err)
	case <-ctx.Done():
		logger.Info("shutdown signal received; draining")
	}
	// CF sends SIGTERM and waits ~10s before SIGKILL. Give handlers
	// most of that budget to drain in-flight work. Reached from both
	// the signal path AND the server-error path, so a crash and a
	// clean shutdown go through the same drain logic.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
	}
}

// logLevelFromEnv reads LOG_LEVEL and maps it to slog.Level. INFO by
// default; DEBUG in local dev via `LOG_LEVEL=debug go run ./cmd/server`.
// Deliberately the only knob — no YAML, no flags, no framework config.
// ERROR is supported for forks that want to suppress INFO in low-noise
// deployments, but WARN is intentionally NOT mapped: this template does
// not use that level (see README §"Logging — two levels, no warnings").
func logLevelFromEnv() slog.Level {
	raw := os.Getenv("LOG_LEVEL")
	switch strings.ToLower(raw) {
	case "":
		return slog.LevelInfo
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "error":
		return slog.LevelError
	default:
		// Unknown value (including "warn", which is intentionally not
		// mapped per README §"Logging"). Fall back to INFO and tell
		// the operator on stderr so a typo or a misremembered setting
		// does not silently pick a level the operator did not intend.
		fmt.Fprintf(os.Stderr,
			"LOG_LEVEL=%q ignored; use debug|info|error. See README §Logging.\n",
			raw)
		return slog.LevelInfo
	}
}

// buildRouter wires the Gin router from its abstract dependencies —
// NOT from *btp.Service directly. Handlers added here are testable
// with a one-method fake (see examples/*_test.go) and decoupled from
// any internal btp refactor.
//
// The set of routes below is the *constrained-proxy* pattern: every
// endpoint has a fixed method, a fixed destination name, and a fixed
// SAP path baked in at the registration site. That is deliberate —
// strict typing at the Gin boundary requires a finite endpoint set.
// A transparent `api.Any("/sap/:destination/*path", …)` route is
// convenient but untyped by definition, and it turns the service
// into a tunnel that carries the destination's technical-user
// authority to any authenticated BTP caller. The template ships
// without such a route; forks that genuinely need one should wire
// `svc.ProxyHandler` themselves, gated behind `btp.RequireScope`.
func buildRouter(validator *btp.JWTValidator, caller btp.OnPremCaller, mutator btp.OnPremMutator, logger *slog.Logger) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	// The backend app is directly reachable on its .cfapps.* route, not
	// only via the approuter. Trusting every upstream for X-Forwarded-For
	// would let a direct caller forge c.ClientIP(). nil trusts nobody; the
	// logged IP is always the real TCP peer.
	_ = r.SetTrustedProxies(nil)
	// Middleware order matters. Outermost is recoverPanic — its deferred
	// recover() must wrap every other handler so a panic anywhere in the
	// chain lands in the typed btp.ErrorEnvelope path. RequestID next so
	// the access log and any AbortError envelope share the ID. MaxBodySize
	// sits before any handler that reads the body — an oversized payload
	// fails fast with a typed 413 rather than reaching the Gin binder
	// where it would buffer into the app's 128 MiB CF memory quota.
	// requestLog stays innermost so its deferred access-log line captures
	// the final response status; securityHeaders sits immediately before
	// it.
	r.Use(
		recoverPanic(),
		btp.RequestID(),
		btp.MaxBodySize(btp.DefaultMaxBodyBytes),
		securityHeaders(),
		requestLog(logger),
	)

	r.GET("/healthz", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	r.GET("/version", versionHandler())

	api := r.Group("/api")
	api.Use(validator.Middleware())
	api.GET("/me", func(c *gin.Context) {
		claims, _ := c.Get("jwtClaims")
		c.JSON(http.StatusOK, gin.H{"claims": claims})
	})

	// Mount a huma.API on top of the same Gin group. huma generates a
	// real OpenAPI 3.1 spec from the handler signatures and serves it +
	// a Swagger UI for free:
	//
	//   GET /api/openapi.json   — the spec (OpenAPI 3.1)
	//   GET /api/openapi.yaml   — same, YAML
	//   GET /api/docs           — Swagger UI rendered from the spec
	//   GET /api/schemas/*      — referenced schemas
	//
	// They sit under /api so the JWT middleware applies — the spec
	// describes a JWT-gated API; reading it requires the same auth.
	// Forks that want public docs can move the huma mount to the
	// engine root and drop validator from the operations directly,
	// but the typical case (HF-internal API) is happier with gated
	// docs.
	hapi := humagin.NewWithGroup(r, api,
		huma.DefaultConfig("Go SAP BTP CF Template", "0.1"))

	// Two constrained-proxy demos. Both are fully typed (JSON in,
	// JSON out) — SAP's XML is consumed + parsed inside the handler,
	// never emitted at the client boundary:
	//   GET  /api/adt-discovery  → huma-style: typed Input/Output,
	//                               appears in the OpenAPI spec.
	//   POST /api/adt-checkrun   → gin-style: raw c.ShouldBindJSON,
	//                               does NOT appear in the OpenAPI
	//                               spec. Migrating it (and
	//                               invoicesync) to huma is tracked
	//                               as follow-up work — they read
	//                               jwtClaims, which currently lives
	//                               in the gin context map and needs
	//                               a small adapter to surface in
	//                               huma's context.Context.
	adtdiscovery.Register(hapi, caller)
	adtcheckrun.Register(api, mutator)

	return r
}

// buildUserAgent derives a traceable User-Agent from the compiled binary's
// module path and version. Passing this through to btp.NewService means
// SAP-side access logs and oncall traces see "my-service/v1.2.3" rather
// than the template's literal name — exactly the move each fork should
// make. debug.ReadBuildInfo can fail for unusual build setups (test
// binaries, `go run`); the fallback keeps the service bootable.
func buildUserAgent() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if p := info.Main.Path; p != "" && p != "command-line-arguments" {
			ver := info.Main.Version
			if ver == "" || ver == "(devel)" {
				ver = "dev"
			}
			return p + "/" + ver
		}
	}
	return btp.DefaultUserAgent
}

// recoverPanic replaces gin.Recovery() so panic responses honour the
// typed btp.ErrorEnvelope contract. Default gin.Recovery in ReleaseMode
// writes "Internal Server Error" as text/plain — a client that switches
// on `error.code` gets undefined behaviour for panics. This emits the
// same envelope shape every other error path uses.
//
// io.Discard suppresses Gin's built-in stack-to-stderr line: AbortError
// already does its own slog.ErrorContext with the request ID, and the
// panic value + stack are pushed into that line so the operator gets
// one structured record per panic instead of two — one plain-text from
// Gin and one structured from us.
//
// Note: by the time this fires, btp.RequestID() has already run (it is
// installed inside this Recovery's deferred wrap) so the envelope and
// the operator log line share the same request_id. A panic *before*
// RequestID runs would surface with an empty request_id; that case is
// pre-request and acceptable.
func recoverPanic() gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, recovered any) {
		// Build an err that carries the panic value and the stack so
		// AbortError's slog line preserves both. The client never sees
		// it — userMsg below is what reaches the wire.
		err := fmt.Errorf("panic: %v\n%s", recovered, debug.Stack())
		btp.AbortError(c, http.StatusInternalServerError, btp.CodeInternal,
			"internal error", err)
	})
}

// securityHeaders attaches a small, JSON-API-appropriate set of response
// headers on every response. The backend serves JSON (not HTML), so CSP
// and frame-ancestors policies are low-value and intentionally omitted —
// the headers below are the cheap wins that any public-facing service is
// expected to emit, regardless of whether browsers or other services are
// the consumer.
//
//   - Strict-Transport-Security: a year, includeSubDomains. The backend
//     is reachable on its .cfapps.* route directly, not just behind the
//     approuter, so every response that crosses the public internet
//     should pin TLS for future requests.
//   - X-Content-Type-Options: nosniff. Suppresses MIME-sniffing on
//     anything we serve, in case a buggy proxy ever rewrites a
//     Content-Type.
//   - Referrer-Policy: no-referrer. We never serve HTML, so a Referer
//     header originating from this service makes no sense; keep clients
//     from leaking their own URLs onward in case of an unintended
//     redirect.
//   - Cache-Control: no-store on /api/*. API responses are user-scoped
//     by definition (everything under /api is JWT-gated) — no browser,
//     intermediate proxy, or CDN should retain a copy that another
//     authenticated user might later receive. no-store is the strictest
//     variant: no-cache would still permit revalidation, no-store
//     forbids retention entirely. /healthz and any future static asset
//     on the root path are unaffected — they're not user-scoped.
func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		c.Next()
	}
}

// requestLog is a minimal structured access logger. gin.Logger() is fine
// in dev but emits plain text that doesn't play nicely with BTP's
// application logging service.
//
// Deliberate omissions:
//   - Query string is NOT logged. Downstream services that put an ID
//     or email in ?owner=… shouldn't have it land in the access log
//     as a side effect. If a route needs that context, it belongs in
//     the handler's slog line (audit trail), not in this generic one.
//   - JWT claims are NOT logged. This is the access log — one line per
//     request, no user identity. Handler-level slog.InfoContext with
//     the claim is the right place.
func requestLog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		rid, _ := c.Get(btp.RequestIDContextKey)
		ridStr, _ := rid.(string)
		logger.Info("http",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"client_ip", c.ClientIP(),
			"request_id", ridStr,
		)
	}
}
