package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Build-time variables injected via -ldflags. Defaults are safe fallbacks
// for local `go run` and test builds that don't pass ldflags.
var (
	version   = "dev"
	commit    = "unknown"
	branch    = "unknown"
	buildDate = "unknown"
)

func versionHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"version":    version,
			"commit":     commit,
			"branch":     branch,
			"build_date": buildDate,
		})
	}
}
