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
func Register(api *gin.RouterGroup, svc *btp.Service) {
	api.POST("/invoice-sync", Handler(svc))
}

// Handler is the actual request handler. Split from Register so it's
// easy to unit-test with a fixture-built btp.Service (see
// internal/btp/service_test.go for the stub patterns).
func Handler(svc *btp.Service) gin.HandlerFunc {
	// destinationName would usually come from configuration or a route
	// parameter; hard-coded here so the example stays self-contained.
	const destinationName = "HF_S4"

	return func(c *gin.Context) {
		// 2. Bind + validate the request body.
		var req Request
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// 3. (Optional) who is the caller? Claims are already
		//    signature- and audience-validated by the middleware.
		//    user_name is the canonical XSUAA user claim.
		claims := c.MustGet("jwtClaims").(jwt.MapClaims)
		_ = claims["user_name"]

		// Marshal the validated request into whatever shape the ABAP
		// endpoint expects. A real service keeps this lossy conversion
		// in one place, not inline in the handler.
		body, err := json.Marshal(toABAPPayload(req))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "marshal: " + err.Error()})
			return
		}

		// 4. One call runs XSUAA client_credentials → Destination
		//    lookup → Connectivity client_credentials → Cloud
		//    Connector tunnel → Basic Auth against SAP.
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
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		defer func() { _ = resp.Body.Close() }()

		// 5. Stream the on-prem response through verbatim.
		//    resp.ContentLength can be -1 for chunked responses;
		//    DataFromReader falls back to streaming without a
		//    Content-Length header in that case.
		c.DataFromReader(resp.StatusCode, resp.ContentLength,
			resp.Header.Get("Content-Type"), resp.Body, nil)
	}
}

// toABAPPayload converts the validated Gin-side request into the shape
// the ABAP endpoint expects. Field-name mapping (BUKRS for company
// code, WAERS for currency, etc.) lives here so handler code stays
// readable and the translation has exactly one home.
func toABAPPayload(r Request) any {
	return map[string]any{
		"BUKRS":  r.CompanyCode,
		"BUDAT":  r.PostingDate.Format("2006-01-02"),
		"AMOUNT": r.AmountCents,
		"WAERS":  r.Currency,
		"XBLNR":  r.Reference,
	}
}
