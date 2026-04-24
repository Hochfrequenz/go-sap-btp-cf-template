// Package adtcheckrun is a reference handler for the README's
// "Calling SAP with a POST — the CSRF case" section. It is NOT wired
// into cmd/server — it exists as a typed, compileable illustration so
// a reader who copies the pattern ends up with working Go that posts
// to an ABAP endpoint behind CSRF rather than a fragmenty snippet.
//
// What makes this example different from invoicesync:
//
//   - It depends on btp.OnPremMutator (not btp.OnPremCaller).
//     CallOnPremiseMutating runs the full X-CSRF-Token handshake
//     transparently — fetch token + SAP cookies, attach on the
//     mutating call, re-fetch on 403.
//
//   - The target is ADT's syntax-check endpoint
//     (/sap/bc/adt/checkruns), which accepts a minimal XML payload
//     and returns check-run results. Any ADT POST endpoint behind
//     CSRF follows the same wiring; switch the path + payload for
//     your own service.
//
//   - The handler-test in handler_test.go uses a one-method fake
//     that satisfies btp.OnPremMutator — no XSUAA, no Destination
//     lookup, no Cloud Connector, no CSRF fetch round-trip. The
//     service-level CSRF logic is already tested in
//     internal/btp/service_csrf_test.go; the handler test just
//     exercises handler logic.
package adtcheckrun

import (
	"bytes"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/hochfrequenz/go-sap-btp-cloud-foundry-mwe/internal/btp"
)

// Request is the typed view of the JSON body this endpoint accepts.
// Validator tags keep the ABAP side from ever seeing a malformed
// payload — same discipline as invoicesync, just aimed at an ADT
// syntax check rather than an FI posting.
type Request struct {
	// ObjectURI is the ADT URI of the object to check, e.g.
	// `/sap/bc/adt/programs/programs/zmy_program/source/main`.
	// Required and must be an ADT-shaped path.
	ObjectURI string `json:"object_uri" binding:"required,startswith=/sap/bc/adt/"`
}

// Register attaches the /adt-checkrun endpoint to the JWT-guarded
// `api` group. Call this from cmd/server/main.go's buildRouter
// alongside the other route registrations.
func Register(api *gin.RouterGroup, svc btp.OnPremMutator) {
	api.POST("/adt-checkrun", Handler(svc))
}

// Handler is the actual request handler. Depends on the narrow
// btp.OnPremMutator interface (not *btp.Service), so unit tests
// substitute a one-method fake without standing up the XSUAA /
// Destination / Cloud Connector stack. The CSRF handshake is the
// Service's concern, not the handler's — a real svc.
// CallOnPremiseMutating does the fetch/attach/retry dance; the
// fake in handler_test.go does not need to.
func Handler(svc btp.OnPremMutator) gin.HandlerFunc {
	const destinationName = "HF_S4"

	return func(c *gin.Context) {
		var req Request
		if err := c.ShouldBindJSON(&req); err != nil {
			btp.AbortError(c, http.StatusBadRequest, btp.CodeInvalidRequest,
				err.Error(), nil)
			return
		}

		claims := c.MustGet("jwtClaims").(jwt.MapClaims)
		userName, _ := claims["user_name"].(string)
		slog.InfoContext(c.Request.Context(), "adt check-run requested",
			"user", userName, "object", req.ObjectURI)

		// ADT's check-run endpoint accepts a tiny XML body that
		// declares one object to check. Keep it typed via a struct
		// and marshal — never build XML by string concatenation.
		body := buildCheckRunXML(req.ObjectURI)
		headers := http.Header{
			"Content-Type": []string{"application/vnd.sap.adt.checkobjects+xml"},
			"Accept":       []string{"application/vnd.sap.adt.checkmessages+xml"},
		}

		resp, err := svc.CallOnPremiseMutating(
			c.Request.Context(),
			destinationName,
			http.MethodPost,
			"/sap/bc/adt/checkruns",
			headers,
			bytes.NewReader(body),
		)
		if err != nil {
			btp.AbortError(c, http.StatusBadGateway, btp.CodeUpstreamUnreachable,
				"on-premise call failed", err)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		c.DataFromReader(resp.StatusCode, resp.ContentLength,
			resp.Header.Get("Content-Type"), resp.Body, nil)
	}
}

// buildCheckRunXML returns an ADT check-run request body naming a
// single object. The exact shape is documented in SAP Note 1922353
// and in adtler's source; this is the minimal form that works.
//
// Kept as a named helper (rather than inlined) so the XML shape is
// visible in one place — copy-paste-and-modify for your own ADT
// endpoint is the template's point.
func buildCheckRunXML(objectURI string) []byte {
	// Single-object checkrun payload. "reporters" names the ABAP
	// checker; ATC / syntax are the common picks.
	return []byte(
		`<?xml version="1.0" encoding="UTF-8"?>` +
			`<chkrun:checkObjectList xmlns:chkrun="http://www.sap.com/adt/checkrun" chkrun:maximumVerdicts="100">` +
			`<chkrun:checkObject adtcore:uri="` + objectURI + `" chkrun:version="active" xmlns:adtcore="http://www.sap.com/adt/core"/>` +
			`</chkrun:checkObjectList>`)
}
