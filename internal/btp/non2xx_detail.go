package btp

import "fmt"

// OnPremNon2xxDetail returns a stable, client-safe detail string for
// the case where [Service.CallOnPremise] / [Service.CallOnPremiseMutating]
// returned without error but the on-prem system replied with a non-2xx
// status. The handler is responsible for the 502 envelope status; this
// helper only supplies the `detail` text.
//
// Format: "on-premise system returned HTTP <status>".
//
// **Stable wire format** — clients may switch on it, tests will assert
// against it, alert rules might match on it. Changes (rewording, added
// reason phrase, etc.) require a CHANGELOG entry and a migration note.
//
// Why no SAP response body or reason phrase: bodies can leak internal
// SAP detail (Short Dump trace IDs, ABAP field names) into the public
// envelope; reason phrases (`"Bad Request"`) are derivable client-side
// from the integer. Operators get the full body via slog server-side.
//
// See [ClassifyOnPremError] for the parallel classifier on the err path.
func OnPremNon2xxDetail(status int) string {
	return fmt.Sprintf("on-premise system returned HTTP %d", status)
}
