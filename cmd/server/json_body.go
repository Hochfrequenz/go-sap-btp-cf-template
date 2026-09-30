package main

import (
	"mime"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/hochfrequenz/btpingo"
	"github.com/hochfrequenz/btpingo/ginpingo"
)

// requireJSONBody rejects any /api request that carries a body unless its
// Content-Type is "application/json" or a structured-syntax "application/*
// +json" type (e.g. "application/merge-patch+json"); a parameter such as
// "; charset=utf-8" is ignored, and both the type and its parameter names
// are matched case-insensitively. POST always counts as carrying a body,
// even with Content-Length 0 or unknown (-1, e.g. chunked), so every POST
// must declare its format; PUT/PATCH/DELETE with no body (Content-Length
// 0) pass through untouched. A missing or malformed Content-Type is
// rejected. GET, HEAD, and OPTIONS are exempted by this middleware
// outright, regardless of any body they carry.
//
// This complements — it does not replace — the approuter's own CSRF check
// on /api/*: that check governs the session credentials a request carries,
// while this middleware governs the shape of the payload itself. Every
// write handler behind it parses the body as JSON (c.ShouldBindJSON /
// huma's own decoder) and nothing else, so making that requirement
// explicit here, and rejecting anything else up front, keeps the two
// checks aligned instead of relying only on each handler's own binder to
// fail closed.
//
// mime.ParseMediaType (not a raw string compare) is used so parameters
// (e.g. "application/json; charset=utf-8", case differences, or extra
// whitespace) do not cause a false rejection, while a malformed
// Content-Type header still fails closed.
func requireJSONBody() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}

		// No body to decode: Content-Length 0 (Go also reports 0 for a
		// request with neither Content-Length nor Transfer-Encoding). A
		// bodyless DELETE/PUT/PATCH passes; POST does not, so every POST
		// declares its format. An unknown length (-1: chunked, or HTTP/2
		// without Content-Length) counts as a body.
		if c.Request.ContentLength == 0 && c.Request.Method != http.MethodPost {
			c.Next()
			return
		}

		mediaType, _, err := mime.ParseMediaType(c.Request.Header.Get("Content-Type"))
		if err != nil || !isJSONMediaType(mediaType) {
			ginpingo.AbortError(c, http.StatusUnsupportedMediaType, btpingo.CodeInvalidRequest,
				"request body must be application/json", nil)
			return
		}
		c.Next()
	}
}

// isJSONMediaType reports whether mt (already lower-cased by
// mime.ParseMediaType) is application/json or a structured-syntax
// "+json" type such as application/merge-patch+json.
func isJSONMediaType(mt string) bool {
	return mt == "application/json" ||
		(strings.HasPrefix(mt, "application/") && strings.HasSuffix(mt, "+json"))
}
