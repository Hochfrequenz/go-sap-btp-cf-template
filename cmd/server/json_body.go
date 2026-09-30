package main

import (
	"mime"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/hochfrequenz/btpingo"
	"github.com/hochfrequenz/btpingo/ginpingo"
)

// requireJSONBody rejects any /api request whose method is not GET, HEAD,
// or OPTIONS unless its Content-Type is exactly "application/json" (a
// charset parameter is allowed; anything else, including a MISSING
// Content-Type, is rejected). GET/HEAD/OPTIONS never carry a body under
// this template's routes, so they pass through untouched.
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

		mediaType, _, err := mime.ParseMediaType(c.Request.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			ginpingo.AbortError(c, http.StatusUnsupportedMediaType, btpingo.CodeInvalidRequest,
				"request body must be application/json", nil)
			return
		}
		c.Next()
	}
}
