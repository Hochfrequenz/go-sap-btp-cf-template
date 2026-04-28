package btp_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"

	"github.com/hochfrequenz/go-sap-btp-cf-template/internal/btp"
)

// The literal-string assertions in this file are intentional: the
// (kind, detail) pairs are part of the public API surface and clients
// may switch on them. A typo-fix that "improves" wording would silently
// break consumers; these tests catch that.

func Test_ClassifyOnPremError_NilReturnsEmpty(t *testing.T) {
	kind, detail := btp.ClassifyOnPremError(nil)
	then.AssertThat(t, string(kind), is.EqualTo(""))
	then.AssertThat(t, detail, is.EqualTo(""))
}

func Test_ClassifyOnPremError_DestinationNotFound(t *testing.T) {
	kind, detail := btp.ClassifyOnPremError(btp.ErrDestinationNotFound)
	then.AssertThat(t, kind, is.EqualTo(btp.OnPremFailureDestinationNotFound))
	then.AssertThat(t, string(kind), is.EqualTo("destination_not_found"))
	then.AssertThat(t, detail, is.EqualTo("destination not found"))
}

func Test_ClassifyOnPremError_ResponseTooLarge(t *testing.T) {
	kind, detail := btp.ClassifyOnPremError(btp.ErrOnPremResponseTooLarge)
	then.AssertThat(t, kind, is.EqualTo(btp.OnPremFailureResponseTooLarge))
	then.AssertThat(t, string(kind), is.EqualTo("response_too_large"))
	then.AssertThat(t, detail, is.EqualTo("on-premise response exceeded configured size limit"))
}

func Test_ClassifyOnPremError_Timeout(t *testing.T) {
	kind, detail := btp.ClassifyOnPremError(context.DeadlineExceeded)
	then.AssertThat(t, kind, is.EqualTo(btp.OnPremFailureTimeout))
	then.AssertThat(t, string(kind), is.EqualTo("timeout"))
	then.AssertThat(t, detail, is.EqualTo("on-premise call timed out"))
}

func Test_ClassifyOnPremError_Canceled(t *testing.T) {
	kind, detail := btp.ClassifyOnPremError(context.Canceled)
	then.AssertThat(t, kind, is.EqualTo(btp.OnPremFailureCanceled))
	then.AssertThat(t, string(kind), is.EqualTo("canceled"))
	then.AssertThat(t, detail, is.EqualTo("on-premise call canceled"))
}

func Test_ClassifyOnPremError_TransportFallback(t *testing.T) {
	kind, detail := btp.ClassifyOnPremError(errors.New("dns lookup failed: no such host"))
	then.AssertThat(t, kind, is.EqualTo(btp.OnPremFailureTransport))
	then.AssertThat(t, string(kind), is.EqualTo("transport_error"))
	then.AssertThat(t, detail, is.EqualTo("on-premise transport error"))
}

// Service.CallOnPremise wraps errors with fmt.Errorf("...: %w", err);
// the classifier must walk that chain via errors.Is to remain accurate
// across wraps that the production code already does today.
func Test_ClassifyOnPremError_WrappedSentinelStillClassified(t *testing.T) {
	wrapped := fmt.Errorf("destination lookup: %w", btp.ErrDestinationNotFound)
	kind, detail := btp.ClassifyOnPremError(wrapped)
	then.AssertThat(t, kind, is.EqualTo(btp.OnPremFailureDestinationNotFound))
	then.AssertThat(t, detail, is.EqualTo("destination not found"))
}

func ExampleClassifyOnPremError() {
	kind, detail := btp.ClassifyOnPremError(context.DeadlineExceeded)
	fmt.Println(kind, "/", detail)
	// Output: timeout / on-premise call timed out
}
