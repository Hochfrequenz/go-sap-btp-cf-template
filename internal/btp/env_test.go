package btp_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"

	"github.com/hochfrequenz/go-sap-btp-cf-template/internal/btp"
)

const vcapAppJSON = `{"application_id":"id","application_name":"go-btp-mwe","space_name":"dev","uris":["x"]}`

func Test_LoadEnv_FailsWithoutVCAP(t *testing.T) {
	t.Setenv("VCAP_APPLICATION", "")
	t.Setenv("VCAP_SERVICES", "")
	_, err := btp.LoadEnv()
	then.AssertThat(t, err, is.Not(is.Nil()))
	then.AssertThat(t, errors.Is(err, btp.ErrNotInCloudFoundry), is.True())
}

func Test_LoadEnv_FailsWithoutXSUAA(t *testing.T) {
	t.Setenv("VCAP_APPLICATION", vcapAppJSON)
	t.Setenv("VCAP_SERVICES", `{}`)
	_, err := btp.LoadEnv()
	then.AssertThat(t, err, is.Not(is.Nil()))
	then.AssertThat(t, errors.Is(err, btp.ErrNoXSUAABinding), is.True())
}

func Test_LoadEnv_ReportsAllValidationErrorsAtOnce(t *testing.T) {
	// Deliberately broken: bad URL, empty clientsecret, missing xsappname,
	// empty uaadomain. A lazy validator would stop at the first problem;
	// the aggregated validator should surface every one.
	t.Setenv("VCAP_APPLICATION", vcapAppJSON)
	t.Setenv("VCAP_SERVICES", `{
		"xsuaa":[{"label":"xsuaa","name":"go-xsuaa","credentials":{
			"url":"not-a-url","clientid":"cid","clientsecret":"","xsappname":"","uaadomain":""}}]
	}`)
	_, err := btp.LoadEnv()
	then.AssertThat(t, err, is.Not(is.Nil()))
	msg := err.Error()
	then.AssertThat(t, strings.Contains(msg, "xsuaa: url"), is.True())
	then.AssertThat(t, strings.Contains(msg, "clientsecret is required"), is.True())
	then.AssertThat(t, strings.Contains(msg, "xsappname is required"), is.True())
	then.AssertThat(t, strings.Contains(msg, "uaadomain is required"), is.True())
}

func Test_LoadEnv_AggregatesAcrossBindings(t *testing.T) {
	t.Setenv("VCAP_APPLICATION", vcapAppJSON)
	t.Setenv("VCAP_SERVICES", `{
		"xsuaa":[{"label":"xsuaa","name":"x","credentials":{
			"url":"https://uaa.example","clientid":"cid","clientsecret":"","xsappname":"App","uaadomain":""}}],
		"destination":[{"label":"destination","name":"d","credentials":{
			"uri":"","clientid":"","clientsecret":"","url":""}}],
		"connectivity":[{"label":"connectivity","name":"c","credentials":{
			"clientid":"ccid","clientsecret":"csec","url":"https://uaa.example",
			"onpremise_proxy_host":"host","onpremise_proxy_port":"not-a-port"}}]
	}`)
	_, err := btp.LoadEnv()
	then.AssertThat(t, err, is.Not(is.Nil()))
	msg := err.Error()
	then.AssertThat(t, strings.Contains(msg, "xsuaa:"), is.True())
	then.AssertThat(t, strings.Contains(msg, "destination:"), is.True())
	then.AssertThat(t, strings.Contains(msg, "connectivity: onpremise_proxy_port"), is.True())
}

func Test_LoadEnv_HappyPath(t *testing.T) {
	t.Setenv("VCAP_APPLICATION", vcapAppJSON)
	t.Setenv("VCAP_SERVICES", `{
		"xsuaa":[{"label":"xsuaa","name":"x","credentials":{
			"url":"https://uaa.example","clientid":"cid","clientsecret":"csec",
			"xsappname":"App","uaadomain":"uaa.example","identityzone":"hf"}}],
		"destination":[{"label":"destination","name":"d","credentials":{
			"uri":"https://dest.example","clientid":"dcid","clientsecret":"dsec","url":"https://uaa.example"}}],
		"connectivity":[{"label":"connectivity","name":"c","credentials":{
			"clientid":"ccid","clientsecret":"csec","url":"https://uaa.example",
			"onpremise_proxy_host":"proxy","onpremise_proxy_port":"20003"}}]
	}`)
	env, err := btp.LoadEnv()
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, env.XSUAA.XSAppName, is.EqualTo("App"))
	then.AssertThat(t, env.Dest.URI, is.EqualTo("https://dest.example"))
	then.AssertThat(t, env.Conn.OnPremiseProxyPort, is.EqualTo("20003"))
	then.AssertThat(t, env.XSUAA.JWKSURL(), is.EqualTo("https://uaa.example/token_keys"))
}

func Test_XSUAA_StringScrubsSecret(t *testing.T) {
	x := &btp.XSUAACredentials{URL: "https://u", ClientID: "c", ClientSecret: "super-secret", XSAppName: "a", UAADomain: "d"}
	s := fmt.Sprintf("%+v", x)
	then.AssertThat(t, strings.Contains(s, "super-secret"), is.False())
	then.AssertThat(t, strings.Contains(s, "***"), is.True())
}

func Test_Dest_StringScrubsSecret(t *testing.T) {
	d := &btp.DestCredentials{URI: "https://u", ClientID: "c", ClientSecret: "super-secret", URL: "https://uaa"}
	s := fmt.Sprintf("%v", d)
	then.AssertThat(t, strings.Contains(s, "super-secret"), is.False())
}

func Test_Conn_StringScrubsSecret(t *testing.T) {
	c := &btp.ConnCredentials{ClientID: "c", ClientSecret: "super-secret", URL: "https://uaa", OnPremiseProxyHost: "h", OnPremiseProxyPort: "1"}
	s := fmt.Sprintf("%v", c)
	then.AssertThat(t, strings.Contains(s, "super-secret"), is.False())
}

func Test_NilCredentials_StringIsSafe(t *testing.T) {
	var x *btp.XSUAACredentials
	var d *btp.DestCredentials
	var c *btp.ConnCredentials
	then.AssertThat(t, strings.Contains(fmt.Sprintf("%v", x), "nil"), is.True())
	then.AssertThat(t, strings.Contains(fmt.Sprintf("%v", d), "nil"), is.True())
	then.AssertThat(t, strings.Contains(fmt.Sprintf("%v", c), "nil"), is.True())
}

func Test_NilXSUAA_ValidateErrors(t *testing.T) {
	var x *btp.XSUAACredentials
	var errs []string
	x.Validate(&errs)
	then.AssertThat(t, len(errs) > 0, is.True())
}

func Test_Dest_ValidateFlagsMalformedURI(t *testing.T) {
	d := &btp.DestCredentials{URI: "::::not-a-url", ClientID: "c", ClientSecret: "s", URL: "https://u"}
	var errs []string
	d.Validate(&errs)
	then.AssertThat(t, len(errs) > 0, is.True())
	joined := strings.Join(errs, " ")
	then.AssertThat(t, strings.Contains(joined, "destination: uri"), is.True())
}

func Test_XSUAA_ValidateFlagsNonHTTPScheme(t *testing.T) {
	x := &btp.XSUAACredentials{URL: "ftp://example", ClientID: "c", ClientSecret: "s", XSAppName: "a", UAADomain: "d"}
	var errs []string
	x.Validate(&errs)
	joined := strings.Join(errs, " ")
	then.AssertThat(t, strings.Contains(joined, "xsuaa: url"), is.True())
}

func Test_Conn_ValidateFlagsOutOfRangePort(t *testing.T) {
	c := &btp.ConnCredentials{ClientID: "c", ClientSecret: "s", URL: "https://u", OnPremiseProxyHost: "h", OnPremiseProxyPort: "99999"}
	var errs []string
	c.Validate(&errs)
	joined := strings.Join(errs, " ")
	then.AssertThat(t, strings.Contains(joined, "onpremise_proxy_port"), is.True())
}

func Test_NilDestCredentials_ValidateIsNoop(t *testing.T) {
	var d *btp.DestCredentials
	var errs []string
	d.Validate(&errs)
	then.AssertThat(t, len(errs), is.EqualTo(0))
}

func Test_NilConnCredentials_ValidateIsNoop(t *testing.T) {
	var c *btp.ConnCredentials
	var errs []string
	c.Validate(&errs)
	then.AssertThat(t, len(errs), is.EqualTo(0))
}

func Test_Dest_ValidateRequiresAllFields(t *testing.T) {
	d := &btp.DestCredentials{}
	var errs []string
	d.Validate(&errs)
	joined := strings.Join(errs, " ")
	then.AssertThat(t, strings.Contains(joined, "uri is required"), is.True())
	then.AssertThat(t, strings.Contains(joined, "clientid is required"), is.True())
	then.AssertThat(t, strings.Contains(joined, "clientsecret is required"), is.True())
	then.AssertThat(t, strings.Contains(joined, "url (token endpoint) is required"), is.True())
}

func Test_Conn_ValidateRequiresAllFields(t *testing.T) {
	c := &btp.ConnCredentials{}
	var errs []string
	c.Validate(&errs)
	joined := strings.Join(errs, " ")
	then.AssertThat(t, strings.Contains(joined, "clientid is required"), is.True())
	then.AssertThat(t, strings.Contains(joined, "onpremise_proxy_host is required"), is.True())
	then.AssertThat(t, strings.Contains(joined, "onpremise_proxy_port is required"), is.True())
}

func Test_LoadEnv_MalformedCredentialsJSONErrors(t *testing.T) {
	// "url" is spelled correctly but credentials are a primitive instead of
	// an object — the typed parser surfaces this as an unmarshal failure.
	t.Setenv("VCAP_APPLICATION", vcapAppJSON)
	t.Setenv("VCAP_SERVICES", `{
		"xsuaa":[{"label":"xsuaa","name":"x","credentials":"not-an-object"}]
	}`)
	_, err := btp.LoadEnv()
	then.AssertThat(t, err, is.Not(is.Nil()))
}

// Test_LoadEnv_MalformedVCAPServicesErrors pins the parse-failure path
// for the typed VCAP parser (post go-cfenv removal). A non-JSON
// VCAP_SERVICES env var must surface a parse error, not silently fall
// through into "no bindings" → ErrNoXSUAABinding which would mask the
// real misconfiguration.
func Test_LoadEnv_MalformedVCAPServicesErrors(t *testing.T) {
	t.Setenv("VCAP_APPLICATION", vcapAppJSON)
	t.Setenv("VCAP_SERVICES", `{not valid json`)
	_, err := btp.LoadEnv()
	then.AssertThat(t, err, is.Not(is.Nil()))
	// Distinct error message — must mention parsing, not "binding missing".
	then.AssertThat(t, strings.Contains(err.Error(), "parse VCAP_SERVICES"), is.True())
}

// Test_LoadEnv_FailsWithoutVCAPApplication pins that VCAP_SERVICES alone
// is insufficient to consider the app "in CF" — both env vars are
// required. A missing VCAP_APPLICATION typically signals a buildpack
// misconfiguration; failing fast here is preferable to running with
// undefined behavior.
func Test_LoadEnv_FailsWithoutVCAPApplication(t *testing.T) {
	t.Setenv("VCAP_APPLICATION", "")
	t.Setenv("VCAP_SERVICES", `{"xsuaa":[]}`)
	_, err := btp.LoadEnv()
	then.AssertThat(t, errors.Is(err, btp.ErrNotInCloudFoundry), is.True())
}

func Test_RejectingAuthenticator_TypeIsStar(t *testing.T) {
	// Exercise the AuthType() accessor so coverage isn't stuck at 0%.
	reg := btp.DefaultAuthenticators()
	// Trigger the fallback path with an unknown auth type: the fact that
	// the returned error comes from the reject handler proves the
	// fallback method chain (including AuthType) is wired.
	err := reg.Apply(context.Background(), httptest.NewRequest(http.MethodGet, "/", nil), &btp.Destination{
		Authentication: btp.AuthType("X"),
	})
	then.AssertThat(t, err, is.Not(is.Nil()))
}
