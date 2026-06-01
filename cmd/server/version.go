package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Build-time variables injected via -ldflags. Defaults are safe fallbacks
// for local `go run` and test builds that don't pass ldflags.
const (
	defaultVersion   = "dev"
	defaultCommit    = "unknown"
	defaultBranch    = "unknown"
	defaultBuildDate = "unknown"
)

var (
	version   = defaultVersion
	commit    = defaultCommit
	branch    = defaultBranch
	buildDate = defaultBuildDate
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
