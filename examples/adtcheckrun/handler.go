// Package adtcheckrun is the POST-side showcase for the constrained
// proxy pattern. It accepts a typed JSON request, runs an ADT syntax
// check against the configured destination (via the CSRF-aware
// CallOnPremiseMutating), parses SAP's XML response into typed
// structs, and returns a typed JSON response. The caller never sees
// XML even though the SAP-side protocol is XML end-to-end.
//
// Template discipline kept across this handler:
//
//   - btp.OnPremMutator dependency, not concrete Service type —
//     handler tests use a one-method fake. CSRF handshake is the
//     Service's concern, tested in internal/btp/service_csrf_test.go.
//   - Destination + SAP path hard-coded at Register time — no
//     path or destination injection.
//   - go-playground/validator tag on ObjectURI keeps a malformed
//     request from ever reaching SAP.
//   - Input JSON → internal XML (built via struct marshalling, not
//     string concatenation) → SAP → response XML → typed Go structs
//     → output JSON. The caller boundary is JSON in both directions.
//
// The XML-side types are adapted from
// github.com/Hochfrequenz/adtler (MIT) — specifically adtler's
// `adt/adtxml/syntaxcheck.go`. We vendor rather than import because
// the template deliberately has zero external BTP-specific deps.
package adtcheckrun

import (
	"bytes"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/hochfrequenz/go-sap-btp-cf-template/internal/btp"
)

// Request is the typed view of the JSON body. The ObjectURI tag
// keeps a malformed path from ever reaching SAP.
type Request struct {
	// ObjectURI is the ADT URI of the object to check, e.g.
	// `/sap/bc/adt/oo/classes/cl_abap_syntax/source/main`.
	// Required and must be an ADT-shaped path.
	ObjectURI string `json:"object_uri" binding:"required,startswith=/sap/bc/adt/"`
}

// Response is the typed JSON view of SAP's checkrun reply. The
// internal XML shape is decoded first, then translated to this flat
// JSON-friendly structure. Adtler's original struct has a `URI`
// on CheckMessage that carries encoded `#start=line,col`; the
// translation step below decodes that into line/column integers so
// callers don't have to parse fragment-strings.
type Response struct {
	Reports []Report `json:"reports"`
}

// Report is one per object being checked. Status is typically
// "notProcessed" (object not found), "processed" (ran to completion),
// or "processedWithErrors".
type Report struct {
	Reporter      string    `json:"reporter"`
	TriggeringURI string    `json:"triggering_uri"`
	Status        string    `json:"status"`
	StatusText    string    `json:"status_text,omitempty"`
	Messages      []Message `json:"messages"`
}

// Message is one issue the checker produced. Type is "E" (error),
// "W" (warning), "I" (informational), "S" (status). Line + Column
// come from decoding the SAP URI's `#start=line,col` fragment.
type Message struct {
	Type      string `json:"type"`
	ShortText string `json:"short_text"`
	URI       string `json:"uri"`
	Line      int    `json:"line,omitempty"`
	Column    int    `json:"column,omitempty"`
}

// Register attaches POST /adt-checkrun to the JWT-guarded api group.
// The destination name and SAP path are closed over in the handler
// — this is a constrained-proxy route, not a transparent one.
func Register(api *gin.RouterGroup, svc btp.OnPremMutator) {
	api.POST("/adt-checkrun", Handler(svc))
}

// Handler runs the POST demo. Flow:
//
//  1. Validate JSON Request (struct tags).
//  2. Build the internal XML payload SAP's checkrun expects.
//  3. Call svc.CallOnPremiseMutating — which runs the CSRF dance.
//  4. Read the SAP XML response and decode into CheckRunReports.
//  5. Translate to the JSON-shaped Response and return.
func Handler(svc btp.OnPremMutator) gin.HandlerFunc {
	// FORK: "HF_S4" is the name of Hochfrequenz's on-prem destination.
	// apply-config rewrites this literal across examples/**/*.go via
	// `examples.destination_name` in config.yml — change config.yml,
	// re-run apply-config, this constant updates with everything else.
	// The SAP path /sap/bc/adt/checkruns is standard across any
	// ADT-enabled S/4 system and rarely needs changing.
	const (
		destinationName = "HF_S4"
		sapPath         = "/sap/bc/adt/checkruns"
	)

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

		body := buildCheckRunXML(req.ObjectURI)
		headers := http.Header{
			"Content-Type": []string{"application/vnd.sap.adt.checkobjects+xml"},
			"Accept":       []string{"application/vnd.sap.adt.checkmessages+xml"},
		}

		resp, err := svc.CallOnPremiseMutating(
			c.Request.Context(),
			destinationName,
			http.MethodPost,
			sapPath,
			headers,
			bytes.NewReader(body),
		)
		if err != nil {
			btp.AbortError(c, http.StatusBadGateway, btp.CodeUpstreamUnreachable,
				"on-premise call failed", err)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			btp.AbortError(c, http.StatusBadGateway, btp.CodeUpstreamUnreachable,
				btp.OnPremNon2xxDetail(resp.StatusCode), nil)
			return
		}

		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			btp.AbortError(c, http.StatusBadGateway, btp.CodeUpstreamUnreachable,
				"reading on-premise response body failed", err)
			return
		}

		var reports checkRunReports
		if err := xml.Unmarshal(raw, &reports); err != nil {
			btp.AbortError(c, http.StatusBadGateway, btp.CodeUpstreamUnreachable,
				"parsing on-premise check-run response failed", err)
			return
		}

		c.JSON(http.StatusOK, toResponse(reports))
	}
}

// buildCheckRunXML returns the check-run request body naming a
// single object. The exact shape is documented in SAP Note 1922353
// and in adtler's source.
func buildCheckRunXML(objectURI string) []byte {
	// Single-object checkrun payload. maximumVerdicts caps how many
	// issues SAP returns per object.
	return []byte(
		`<?xml version="1.0" encoding="UTF-8"?>` +
			`<chkrun:checkObjectList xmlns:chkrun="http://www.sap.com/adt/checkrun" chkrun:maximumVerdicts="100">` +
			`<chkrun:checkObject adtcore:uri="` + xmlEscape(objectURI) + `" chkrun:version="active" xmlns:adtcore="http://www.sap.com/adt/core"/>` +
			`</chkrun:checkObjectList>`)
}

// xmlEscape escapes a string for use as an XML attribute value.
// objectURI is validated by the struct tag to start with
// /sap/bc/adt/ but may still contain path segments that need
// escaping (e.g. angle brackets in method names). Using Go's
// encoding/xml for a single attribute is overkill; this covers
// the five XML-significant characters.
func xmlEscape(s string) string {
	r := strings.NewReplacer(
		`&`, `&amp;`,
		`<`, `&lt;`,
		`>`, `&gt;`,
		`"`, `&quot;`,
		`'`, `&apos;`,
	)
	return r.Replace(s)
}

// --- SAP-side XML types (adapted from github.com/Hochfrequenz/adtler, MIT) ---

// checkRunReports is the XML response from POST /sap/bc/adt/checkruns.
// Verified shape (adtler, 2026-03-24, S/4 on-prem).
type checkRunReports struct {
	XMLName xml.Name         `xml:"checkRunReports"`
	Reports []checkRunReport `xml:"checkReport"`
}

type checkRunReport struct {
	Reporter   string         `xml:"reporter,attr"`
	TriggerURI string         `xml:"triggeringUri,attr"`
	Status     string         `xml:"status,attr"`
	StatusText string         `xml:"statusText,attr"`
	Messages   []checkMessage `xml:"checkMessageList>checkMessage"`
}

type checkMessage struct {
	URI       string `xml:"uri,attr"`
	Type      string `xml:"type,attr"`
	ShortText string `xml:"shortText,attr"`
}

// parseMessagePosition extracts (line, column) from a checkMessage URI
// fragment of the form `.../source/main#start=42,5`. Returns zeros
// when the fragment is absent or malformed.
func parseMessagePosition(uri string) (int, int) {
	idx := strings.Index(uri, "#start=")
	if idx < 0 {
		return 0, 0
	}
	parts := strings.SplitN(uri[idx+len("#start="):], ",", 2)
	line, _ := strconv.Atoi(parts[0])
	col := 0
	if len(parts) == 2 {
		col, _ = strconv.Atoi(parts[1])
	}
	return line, col
}

func toResponse(r checkRunReports) Response {
	out := Response{Reports: make([]Report, 0, len(r.Reports))}
	for _, rep := range r.Reports {
		msgs := make([]Message, 0, len(rep.Messages))
		for _, m := range rep.Messages {
			line, col := parseMessagePosition(m.URI)
			msgs = append(msgs, Message{
				Type:      m.Type,
				ShortText: m.ShortText,
				URI:       m.URI,
				Line:      line,
				Column:    col,
			})
		}
		out.Reports = append(out.Reports, Report{
			Reporter:      rep.Reporter,
			TriggeringURI: rep.TriggerURI,
			Status:        rep.Status,
			StatusText:    rep.StatusText,
			Messages:      msgs,
		})
	}
	return out
}
