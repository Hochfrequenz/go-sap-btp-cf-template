package btp

import (
	"net/http"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"
)

// Internal tests cover two helpers that are unexported but
// load-bearing for the CSRF path: filterForwardedCookies (which
// decides whether an inbound Cookie header survives the forward)
// and isMutatingMethod (which decides whether ProxyHandler takes
// the CSRF path). Both are small enough to keep behavior frozen
// with direct unit tests — external tests would have to drive
// full HTTP stacks to observe the same outcomes.

func Test_filterForwardedCookies(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"only approuter cookie — dropped", "JSESSIONID=foo", ""},
		{"only SAP session — kept", "SAP_SESSIONID_ABC_100=abc", "SAP_SESSIONID_ABC_100=abc"},
		{"only sap-usercontext — kept", "sap-usercontext=sapclient100", "sap-usercontext=sapclient100"},
		{"mixed — approuter dropped, SAP kept",
			"JSESSIONID=foo; SAP_SESSIONID_XYZ_100=s1; sap-usercontext=u1",
			"SAP_SESSIONID_XYZ_100=s1; sap-usercontext=u1"},
		{"double spaces around separator",
			"JSESSIONID=foo;   SAP_SESSIONID_ABC_100=abc",
			"SAP_SESSIONID_ABC_100=abc"},
		{"empty segment between semicolons",
			";; SAP_SESSIONID_ABC_100=abc ;;",
			"SAP_SESSIONID_ABC_100=abc"},
		{"cookie without value — still checked by name",
			"SAP_SESSIONID_NOVAL",
			"SAP_SESSIONID_NOVAL"},
		{"lowercase sap_sessionid — dropped (case-sensitive by design)",
			"sap_sessionid_abc_100=abc",
			""},
		{"partial-prefix match must NOT pass",
			"SAP_SESSION=nope",
			""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := filterForwardedCookies(c.in)
			then.AssertThat(t, got, is.EqualTo(c.want))
		})
	}
}

func Test_isMutatingMethod(t *testing.T) {
	cases := []struct {
		method string
		want   bool
	}{
		{http.MethodPost, true},
		{http.MethodPut, true},
		{http.MethodDelete, true},
		{http.MethodPatch, true},
		// Case-insensitive handling — clients sometimes lowercase.
		{"post", true},
		{"Put", true},
		// Read methods — must NOT trigger the CSRF path.
		{http.MethodGet, false},
		{http.MethodHead, false},
		{http.MethodOptions, false},
		// Unknown / exotic — default to false so an unexpected
		// method doesn't accidentally skip the read-path retry
		// logic. Forks that need LINK / LOCK / MKCOL in the
		// CSRF gate should override isMutatingMethod.
		{"LINK", false},
		{"LOCK", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.method, func(t *testing.T) {
			then.AssertThat(t, isMutatingMethod(c.method), is.EqualTo(c.want))
		})
	}
}
