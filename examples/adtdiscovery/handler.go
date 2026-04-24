// Package adtdiscovery is the GET-side showcase for the constrained
// proxy pattern. It wraps SAP's `/sap/bc/adt/discovery` endpoint —
// the cheapest non-mutating ADT call — and returns a typed JSON
// view of the ATOM service document the SAP side emits.
//
// Why this handler ships wired into cmd/server/main.go's router
// (unlike invoicesync / adtcheckrun which are pure-reference
// examples): the showcase deploy needs a safe read-path endpoint
// that demonstrates the three-leg dance end-to-end. Discovery is
// ideal because it:
//
//   - Requires only ADT-developer authority on the SAP side
//     (anything broader would be a privilege surface).
//   - Does not mutate state.
//   - Has a well-known XML shape that maps cleanly to a small
//     JSON schema — no caller-visible XML.
//
// Template discipline kept across this handler:
//
//   - Output is typed JSON. SAP's XML is consumed internally;
//     callers never see it.
//   - The destination name and SAP path are hard-coded at the
//     route registration site (Register below), not taken from
//     the request — no path or destination injection surface.
//   - Depends on the narrow btp.OnPremCaller interface so the
//     unit test uses a one-method fake.
package adtdiscovery

import (
	"encoding/xml"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/hochfrequenz/go-sap-btp-cf-template/internal/btp"
)

// Response is the JSON shape the handler returns to the caller.
// A flat workspace → collection map of the ATOM service document,
// with every other ADT-specific thing (categories, templateLinks,
// accept-types) deliberately dropped — they're noise at the API
// boundary and should stay on the SAP side.
type Response struct {
	Workspaces []Workspace `json:"workspaces"`
}

// Workspace groups a set of collections under a human title.
// Matches ATOM's `app:workspace` element.
type Workspace struct {
	Title       string       `json:"title"`
	Collections []Collection `json:"collections"`
}

// Collection is one endpoint the ADT service advertises — href is
// the relative SAP path, title is the human label.
type Collection struct {
	Title string `json:"title"`
	Href  string `json:"href"`
}

// Register attaches GET /adt-discovery to the JWT-guarded api
// group. The destination name and SAP path are closed over in the
// handler so the route is not path-parameterised — that's the
// whole point of the constrained-proxy pattern.
func Register(api *gin.RouterGroup, svc btp.OnPremCaller) {
	api.GET("/adt-discovery", Handler(svc))
}

// Handler calls the configured destination's /sap/bc/adt/discovery,
// parses the ATOM service XML, and returns a compact typed JSON
// view. Fatal on XML parse failure — a well-formed SAP response
// is a prerequisite for the three-leg call to be considered
// working at all.
func Handler(svc btp.OnPremCaller) gin.HandlerFunc {
	const (
		destinationName = "HF_S4"
		sapPath         = "/sap/bc/adt/discovery"
	)

	return func(c *gin.Context) {
		resp, err := svc.CallOnPremise(c.Request.Context(),
			destinationName, http.MethodGet, sapPath, nil, nil)
		if err != nil {
			btp.AbortError(c, http.StatusBadGateway, btp.CodeUpstreamUnreachable,
				"on-premise call failed", err)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			btp.AbortError(c, http.StatusBadGateway, btp.CodeUpstreamUnreachable,
				"on-premise call returned non-2xx", nil)
			return
		}

		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			btp.AbortError(c, http.StatusBadGateway, btp.CodeUpstreamUnreachable,
				"reading on-premise response body failed", err)
			return
		}

		var svcDoc atomService
		if err := xml.Unmarshal(raw, &svcDoc); err != nil {
			btp.AbortError(c, http.StatusBadGateway, btp.CodeUpstreamUnreachable,
				"parsing on-premise ATOM service document failed", err)
			return
		}

		c.JSON(http.StatusOK, toResponse(svcDoc))
	}
}

// atomService mirrors the bits of ATOM/app we care about. SAP's
// response has many more fields (category scheme, accept-types,
// templateLinks, …) which are intentionally not decoded — we only
// need the workspace titles and their collections' title + href.
//
// Local-name XML matching (no namespace URI in the tags) follows
// adtler's (github.com/Hochfrequenz/adtler, MIT) approach: SAP
// sends namespace-prefixed elements (app:workspace, atom:title,
// …) and Go's encoding/xml matches on local name when no
// namespace URI is given, which works across SAP's actual shape.
type atomService struct {
	XMLName    xml.Name        `xml:"service"`
	Workspaces []atomWorkspace `xml:"workspace"`
}

type atomWorkspace struct {
	Title       string           `xml:"title"`
	Collections []atomCollection `xml:"collection"`
}

type atomCollection struct {
	Href  string `xml:"href,attr"`
	Title string `xml:"title"`
}

func toResponse(s atomService) Response {
	out := Response{Workspaces: make([]Workspace, 0, len(s.Workspaces))}
	for _, w := range s.Workspaces {
		cols := make([]Collection, 0, len(w.Collections))
		for _, c := range w.Collections {
			cols = append(cols, Collection{Title: c.Title, Href: c.Href})
		}
		out.Workspaces = append(out.Workspaces, Workspace{
			Title:       w.Title,
			Collections: cols,
		})
	}
	return out
}
