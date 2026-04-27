// Command server is the HTTP entry point for the BTP MWE.
// It boots a Gin router with XSUAA JWT middleware and exposes one demo
// endpoint that proxies calls through the Cloud Connector to a named
// destination.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

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
	//   - WriteTimeout     (600s):  Bounds total handler runtime, since
	//                               Go starts WriteTimeout at header-read,
	//                               not at first write.
	//   - IdleTimeout      (120s):  Keep-alive sockets parked indefinitely.
	//
	// WriteTimeout is sized for an on-prem SAP system that is usually
	// slow under load. An ADT call routed through the Cloud Connector +
	// CSRF handshake regularly takes minutes, and observed worst case is
	// ~5 minutes. 600s (10 minutes) leaves comfortable margin for the
	// "usual" slow-but-completing call without normalising calls that are
	// genuinely hung — values much higher than this should be a per-route
	// per-request override (see below), not a global default.
	//
	// Paired with btp.DefaultOnPremiseTimeout (also 10m). The two close
	// the slow-SAP cliff together: the on-prem client surfaces a clean
	// timeout error to the handler if SAP is hung, and WriteTimeout
	// catches the (hopefully unreachable) case where the handler itself
	// stalls without making the on-prem call.
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
		WriteTimeout:      600 * time.Second,
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
	// Middleware order matters: RequestID first so both the access log
	// and any btp.AbortError envelope can pick up the same ID.
	r.Use(gin.Recovery(), btp.RequestID(), requestLog(logger))

	r.GET("/healthz", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	api := r.Group("/api")
	api.Use(validator.Middleware())
	api.GET("/me", func(c *gin.Context) {
		claims, _ := c.Get("jwtClaims")
		c.JSON(http.StatusOK, gin.H{"claims": claims})
	})
	// Two constrained-proxy demos. Both are fully typed (JSON in,
	// JSON out) — SAP's XML is consumed + parsed inside the handler,
	// never emitted at the client boundary:
	//   GET  /api/adt-discovery  → list ADT workspaces + collections.
	//   POST /api/adt-checkrun   → run an ATC / syntax check for one
	//                              ABAP object; goes through the
	//                              CSRF handshake transparently.
	// Replace or add your own handlers here. Both follow the same
	// shape: destination name + SAP path hard-coded at Register
	// time, validator tags on the request struct.
	adtdiscovery.Register(api, caller)
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
