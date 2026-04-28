package btp

import (
	"context"
	"errors"
)

// OnPremFailureKind is the typed classifier on the result of an on-prem
// call returned by [Service.CallOnPremise] / [Service.CallOnPremiseMutating].
//
// The kind values below are the **stable wire format** — clients may
// switch on them, tests will assert against them, alert rules might
// match on them. Adding a kind is non-breaking; renaming or removing
// a kind requires a CHANGELOG entry and a migration note.
type OnPremFailureKind string

// Classifier kinds returned by [ClassifyOnPremError]. Stable wire
// format — see [OnPremFailureKind] docstring.
const (
	OnPremFailureDestinationNotFound OnPremFailureKind = "destination_not_found"
	OnPremFailureResponseTooLarge    OnPremFailureKind = "response_too_large"
	OnPremFailureTimeout             OnPremFailureKind = "timeout"
	OnPremFailureCanceled            OnPremFailureKind = "canceled"
	OnPremFailureTransport           OnPremFailureKind = "transport_error"
)

// ClassifyOnPremError maps an error from [Service.CallOnPremise] or
// [Service.CallOnPremiseMutating] to a typed kind plus a stable,
// client-safe detail string suitable for the huma 502 envelope's
// `detail` field. Returns ("", "") when err is nil — callers should
// guard the err themselves; this is a classifier, not a status check.
//
// Recognised branches (in priority order):
//
//   - errors.Is(err, [ErrDestinationNotFound])     → ([OnPremFailureDestinationNotFound], "destination not found")
//   - errors.Is(err, [ErrOnPremResponseTooLarge])  → ([OnPremFailureResponseTooLarge],    "on-premise response exceeded configured size limit")
//   - errors.Is(err, context.DeadlineExceeded)     → ([OnPremFailureTimeout],             "on-premise call timed out")
//   - errors.Is(err, context.Canceled)             → ([OnPremFailureCanceled],            "on-premise call canceled")
//   - default                                      → ([OnPremFailureTransport],           "on-premise transport error")
//
// The default branch is intentionally coarse: the on-prem stack
// today does not tag connectivity-proxy failures, token-fetch failures,
// TLS handshake errors, or DNS failures with sentinel errors, so a
// finer split would be guessing. Tightening the default is follow-up
// work that requires adding sentinels in btp first.
//
// The wire format (kind values + detail strings) is part of the
// public API surface; see [OnPremFailureKind].
func ClassifyOnPremError(err error) (OnPremFailureKind, string) {
	if err == nil {
		return "", ""
	}
	switch {
	case errors.Is(err, ErrDestinationNotFound):
		return OnPremFailureDestinationNotFound, "destination not found"
	case errors.Is(err, ErrOnPremResponseTooLarge):
		return OnPremFailureResponseTooLarge, "on-premise response exceeded configured size limit"
	case errors.Is(err, context.DeadlineExceeded):
		return OnPremFailureTimeout, "on-premise call timed out"
	case errors.Is(err, context.Canceled):
		return OnPremFailureCanceled, "on-premise call canceled"
	default:
		return OnPremFailureTransport, "on-premise transport error"
	}
}
