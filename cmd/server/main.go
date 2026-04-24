// Command server is the HTTP entry point for the BTP MWE.
// It boots a Gin router with XSUAA JWT middleware and exposes one demo
// endpoint that proxies calls through the Cloud Connector to a named
// destination.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hochfrequenz/go-sap-btp-cloud-foundry-mwe/internal/btp"
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

	r := buildRouter(validator, svc, logger)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
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
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		return slog.LevelDebug
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func buildRouter(validator *btp.JWTValidator, svc *btp.Service, logger *slog.Logger) *gin.Engine {
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
	// Transparent pass-through to a BTP destination.
	// GET /api/sap/<destinationName>/<path...>
	api.Any("/sap/:destination/*path", svc.ProxyHandler)

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
