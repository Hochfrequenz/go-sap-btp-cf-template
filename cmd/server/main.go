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
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hochfrequenz/go-sap-btp-cloud-foundry-mwe/internal/btp"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
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

	svc, err := btp.NewService(env)
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

	go func() {
		logger.Info("server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server terminated unexpectedly", "err", err)
			cancel()
		}
	}()

	<-ctx.Done()
	// CF sends SIGTERM and waits ~10s before SIGKILL. Give handlers most of
	// that budget to drain in-flight work.
	logger.Info("shutdown signal received; draining")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
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
