// Package invoicesync is a reference handler for the README's
// "Adding your service" section. It is NOT wired into cmd/server —
// it exists as a typed, compileable illustration so a reader who
// copies the pattern ends up with working Go rather than a fragmenty
// snippet.
//
// The five moves a handler makes:
//
//  1. Register the route on the JWT-guarded `api` group in
//     cmd/server/main.go's buildRouter.
//  2. Bind and validate the request body via struct tags that the
//     Gin binder (go-playground/validator) enforces.
//  3. (Optional) Inspect the authenticated user from jwtClaims.
//  4. Call svc.CallOnPremise — one function runs the full three-leg
//     XSUAA → Destination → Cloud Connector → Basic Auth dance.
//  5. Shape the on-prem response back to the caller.
package invoicesync

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/hochfrequenz/go-sap-btp-cloud-foundry-mwe/internal/btp"
)

// Request is the typed view of the JSON body. Every field is validated
// at the Gin boundary via the struct tags below — a bad payload fails
// here with a 400, not on the SAP side with a Short Dump.
type Request struct {
	CompanyCode string    `json:"company_code" binding:"required,len=4,uppercase"`
	PostingDate time.Time `json:"posting_date" binding:"required"`
	AmountCents int64     `json:"amount_cents" binding:"required,min=1"`
	Currency    string    `json:"currency"     binding:"required,oneof=EUR USD GBP"`
	Reference   string    `json:"reference"    binding:"max=16"`
}

// Register attaches the /invoice-sync endpoint to the JWT-guarded `api`
// group. Call this from cmd/server/main.go's buildRouter alongside the
// other route registrations.
func Register(api *gin.RouterGroup, svc btp.OnPremCaller) {
	api.POST("/invoice-sync", Handler(svc))
}

// Handler is the actual request handler. Depends on the narrow
// btp.OnPremCaller interface (not *btp.Service), so unit tests
// substitute a one-method fake without needing to stand up the
// XSUAA / Destination / Cloud Connector stack. See handler_test.go
// in this package for the canonical mock pattern.
func Handler(svc btp.OnPremCaller) gin.HandlerFunc {
	// destinationName would usually come from configuration or a route
	// parameter; hard-coded here so the example stays self-contained.
	const destinationName = "HF_S4"

	return func(c *gin.Context) {
		// Bind + validate the request body. Struct tags do the heavy
		// lifting; a bad payload fails here with a 400 and never
		// reaches the SAP system. Validator messages from
		// go-playground/validator are safe to surface to the client,
		// so we pass err.Error() as the user-facing message — rare
		// case where including the underlying text is the right call.
		var req Request
		if err := c.ShouldBindJSON(&req); err != nil {
			btp.AbortError(c, http.StatusBadRequest, btp.CodeInvalidRequest,
				err.Error(), nil)
			return
		}

		// Claims — read the authenticated user for the audit log.
		// The middleware has already signature- and audience-validated
		// the JWT; `user_name` is the canonical XSUAA user claim.
		claims := c.MustGet("jwtClaims").(jwt.MapClaims)
		userName, _ := claims["user_name"].(string)
		slog.InfoContext(c.Request.Context(), "invoice-sync requested",
			"user", userName, "company_code", req.CompanyCode)

		// Call — one function runs XSUAA client_credentials →
		// Destination lookup → Connectivity client_credentials →
		// Cloud Connector tunnel → Basic Auth against SAP.
		body, _ := json.Marshal(toABAPPayload(req)) // typed struct → cannot fail
		headers := http.Header{"Content-Type": []string{"application/json"}}
		resp, err := svc.CallOnPremise(
			c.Request.Context(),
			destinationName,
			http.MethodPost,
			"/sap/bc/rest/zmy_invoice_sync",
			headers,
			bytes.NewReader(body),
		)
		if err != nil {
			// Upstream (SAP / Cloud Connector) failures are logged with
			// full detail on the server side; the client just sees a
			// stable "upstream_unreachable" code and can retry on that.
			btp.AbortError(c, http.StatusBadGateway, btp.CodeUpstreamUnreachable,
				"on-premise call failed", err)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		// Shape — stream the on-prem response through verbatim.
		// resp.ContentLength can be -1 for chunked responses;
		// DataFromReader falls back to streaming without a
		// Content-Length header in that case.
		c.DataFromReader(resp.StatusCode, resp.ContentLength,
			resp.Header.Get("Content-Type"), resp.Body, nil)
	}
}

// abapPayload is the typed view of what the on-prem ABAP endpoint
// expects. Keeping this as a named struct (rather than map[string]any)
// makes the outbound shape as visible and compiler-checked as the
// inbound Request — matching the README's "type everything" rule.
//
// Field names are SAP FI conventions: BUKRS = Buchungskreis (company
// code), BUDAT = Buchungsdatum (posting date), WAERS = Währung
// (currency), XBLNR = Externe Referenz. Any ABAP REST handler on the
// SAP side typically binds against exactly these names.
type abapPayload struct {
	CompanyCode string `json:"BUKRS"`
	PostingDate string `json:"BUDAT"`
	AmountCents int64  `json:"AMOUNT"`
	Currency    string `json:"WAERS"`
	Reference   string `json:"XBLNR"`
}

// toABAPPayload converts the validated Gin-side request into the shape
// the ABAP endpoint expects. The translation (BUKRS for company code,
// WAERS for currency, etc.) has exactly one home, so handler code
// stays readable.
func toABAPPayload(r Request) abapPayload {
	return abapPayload{
		CompanyCode: r.CompanyCode,
		PostingDate: r.PostingDate.Format("2006-01-02"),
		AmountCents: r.AmountCents,
		Currency:    r.Currency,
		Reference:   r.Reference,
	}
}
