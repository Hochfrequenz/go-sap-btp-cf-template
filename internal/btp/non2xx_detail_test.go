package btp_test

import (
	"fmt"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"

	"github.com/hochfrequenz/go-sap-btp-cf-template/internal/btp"
)

// The literal-string assertions in this file are intentional: the
// format is part of the public API surface and clients may switch
// on it. A typo-fix that "improves" wording would silently break
// consumers; these tests catch that.

func Test_OnPremNon2xxDetail_400(t *testing.T) {
	then.AssertThat(t, btp.OnPremNon2xxDetail(400),
		is.EqualTo("on-premise system returned HTTP 400"))
}

func Test_OnPremNon2xxDetail_404(t *testing.T) {
	then.AssertThat(t, btp.OnPremNon2xxDetail(404),
		is.EqualTo("on-premise system returned HTTP 404"))
}

func Test_OnPremNon2xxDetail_500(t *testing.T) {
	then.AssertThat(t, btp.OnPremNon2xxDetail(500),
		is.EqualTo("on-premise system returned HTTP 500"))
}

func ExampleOnPremNon2xxDetail() {
	fmt.Println(btp.OnPremNon2xxDetail(400))
	fmt.Println(btp.OnPremNon2xxDetail(500))
	// Output:
	// on-premise system returned HTTP 400
	// on-premise system returned HTTP 500
}
