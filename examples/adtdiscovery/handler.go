// Package adtdiscovery is the GET-side showcase for the constrained
// proxy pattern. It wraps SAP's `/sap/bc/adt/discovery` endpoint —
// the cheapest non-mutating ADT call — and returns a typed JSON
// view of the ATOM service document the SAP side emits.
//
// Why this handler ships wired into cmd/server/main.go's router
// (together with adtcheckrun for the POST side; invoicesync stays
// a pure-reference example because it targets a Z-endpoint that
// isn't guaranteed to exist on any given S/4): the showcase deploy
// needs a safe read-path endpoint that demonstrates the three-leg
// dance end-to-end. Discovery is ideal because it:
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
	"context"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

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

// DiscoveryInput is the (empty) input shape huma binds to. GET
// /adt-discovery takes no body, no query, and no path parameters; the
// destination name and SAP path are baked into the handler. Keeping
// the type makes the OpenAPI spec generation trivial and matches
// huma's handler signature.
type DiscoveryInput struct{}

// DiscoveryOutput wraps the JSON body huma writes to the client. The
// embedded `Body` field is huma's convention for "this is the response
// payload, not a header". The OpenAPI schema is derived from
// Response's struct tags — no annotation comments, no manual spec.
type DiscoveryOutput struct {
	Body Response
}

// Register attaches GET /adt-discovery to the huma API. The
// destination name and SAP path are closed over in the handler so the
// route is not path-parameterised — that's the constrained-proxy
// pattern. huma generates the OpenAPI operation, request/response
// schemas, and the Swagger UI entry from the function signature
// alone.
func Register(api huma.API, svc btp.OnPremCaller) {
	huma.Register(api, huma.Operation{
		OperationID: "adt-discovery",
		Method:      http.MethodGet,
		Path:        "/adt-discovery",
		Summary:     "List ADT workspaces and collections",
		Description: "Calls SAP's /sap/bc/adt/discovery on the configured " +
			"destination, parses the ATOM service document, and returns a " +
			"compact typed JSON view of workspaces + their collections.",
		Tags: []string{"adt"},
	}, Handler(svc))
}

// Handler calls the configured destination's /sap/bc/adt/discovery,
// parses the ATOM service XML, and returns a compact typed JSON view.
// Fatal on XML parse failure — a well-formed SAP response is a
// prerequisite for the three-leg call to be considered working at
// all. Errors surface as huma's status-typed errors (502 with the
// huma error model); the SAP-side Go error is captured for
// operator-side context.
func Handler(svc btp.OnPremCaller) func(context.Context, *DiscoveryInput) (*DiscoveryOutput, error) {
	// FORK: "HF_S4" is the name of Hochfrequenz's on-prem destination.
	// Change it to the destination name you configured in your BTP
	// subaccount. The SAP path /sap/bc/adt/discovery is standard
	// across any ADT-enabled S/4 system and rarely needs changing.
	const (
		destinationName = "HF_S4"
		sapPath         = "/sap/bc/adt/discovery"
	)

	return func(ctx context.Context, _ *DiscoveryInput) (*DiscoveryOutput, error) {
		resp, err := svc.CallOnPremise(ctx,
			destinationName, http.MethodGet, sapPath,
			http.Header{"Accept": []string{"application/atomsvc+xml"}}, nil)
		if err != nil {
			// huma's default NewError attaches `errs[].Error()` to the
			// response body — which would leak the underlying Go error
			// text to the client. Log it server-side (operator context),
			// surface only the safe user-message (client contract).
			slog.ErrorContext(ctx, "adt-discovery on-premise call failed", "err", err)
			return nil, huma.Error502BadGateway("on-premise call failed")
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			slog.ErrorContext(ctx, "adt-discovery on-premise non-2xx",
				"status", resp.StatusCode)
			return nil, huma.Error502BadGateway("on-premise call returned non-2xx")
		}

		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			slog.ErrorContext(ctx, "adt-discovery body read failed", "err", err)
			return nil, huma.Error502BadGateway("reading on-premise response body failed")
		}

		var svcDoc atomService
		if err := xml.Unmarshal(raw, &svcDoc); err != nil {
			slog.ErrorContext(ctx, "adt-discovery xml parse failed", "err", err)
			return nil, huma.Error502BadGateway("parsing on-premise ATOM service document failed")
		}

		return &DiscoveryOutput{Body: toResponse(svcDoc)}, nil
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
