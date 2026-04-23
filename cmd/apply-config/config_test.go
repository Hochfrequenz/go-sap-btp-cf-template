package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	then.AssertThat(t, os.WriteFile(path, []byte(content), 0644), is.Nil())
	return path
}

func Test_LoadConfig_ValidMinimal(t *testing.T) {
	path := writeTemp(t, `
app:
  name: my-app
  module: github.com/acme/my-app
services:
  xsuaa: my-xsuaa
  destination: my-dest
  connectivity: my-cc
cf:
  api: https://api.cf.eu10.hana.ondemand.com
  org: ACME_space
  space: dev
  domain: cfapps.eu10.hana.ondemand.com
`)
	cfg, err := LoadConfig(path)
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, cfg.App.Name, is.EqualTo("my-app"))
	then.AssertThat(t, cfg.Services.XSUAA, is.EqualTo("my-xsuaa"))
}

func Test_LoadConfig_DerivesServicesFromAppName(t *testing.T) {
	path := writeTemp(t, `
app:
  name: foo
  module: github.com/acme/foo
cf:
  api: https://api.cf.eu10.hana.ondemand.com
  org: ORG
  space: dev
  domain: cfapps.eu10.hana.ondemand.com
`)
	cfg, err := LoadConfig(path)
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, cfg.Services.XSUAA, is.EqualTo("foo-xsuaa"))
	then.AssertThat(t, cfg.Services.Destination, is.EqualTo("foo-dest"))
	then.AssertThat(t, cfg.Services.Connectivity, is.EqualTo("foo-cc"))
}

func Test_LoadConfig_AggregatesAllValidationErrors(t *testing.T) {
	// Deliberately broken: empty app.name, bad module, non-https cf.api,
	// uppercase services.xsuaa, missing cf.space.
	path := writeTemp(t, `
app:
  name: ""
  module: not-a-module
services:
  xsuaa: Bad_Name
cf:
  api: http://insecure
  org: ORG
  space: ""
  domain: d
`)
	_, err := LoadConfig(path)
	then.AssertThat(t, err, is.Not(is.Nil()))
	msg := err.Error()
	for _, want := range []string{
		"app.name is required",
		"app.module",
		"cf.api",
		"cf.space is required",
		"services.xsuaa",
		"services.destination", // derived fails because app.name empty
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q; got:\n%s", want, msg)
		}
	}
}

func Test_LoadConfig_RejectsUppercaseAppName(t *testing.T) {
	path := writeTemp(t, `
app:
  name: MyApp
  module: github.com/acme/my-app
cf:
  api: https://api.cf.eu10.hana.ondemand.com
  org: O
  space: s
  domain: d
`)
	_, err := LoadConfig(path)
	then.AssertThat(t, err, is.Not(is.Nil()))
	then.AssertThat(t, strings.Contains(err.Error(), "app.name"), is.True())
}

func Test_LoadConfig_AcceptsCFOrgWithSpacesAndUnderscore(t *testing.T) {
	// SAP BTP org names commonly look like "HF Dev Account_hf-cf" —
	// spaces and underscores are legitimate; don't reject them.
	path := writeTemp(t, `
app:
  name: ok
  module: github.com/acme/ok
cf:
  api: https://api.cf.eu10.hana.ondemand.com
  org: HF Dev Account_hf-cf
  space: dev
  domain: cfapps.eu10.hana.ondemand.com
`)
	_, err := LoadConfig(path)
	then.AssertThat(t, err, is.Nil())
}

func Test_LoadConfig_MissingFile(t *testing.T) {
	_, err := LoadConfig("/nope/does/not/exist.yml")
	then.AssertThat(t, err, is.Not(is.Nil()))
	then.AssertThat(t, strings.Contains(err.Error(), "read"), is.True())
}

func Test_LoadConfig_TrimsWhitespaceOnAllFields(t *testing.T) {
	// Whitespace-only values should read as empty — otherwise the
	// blank-check would skip validation and let "   " slip through.
	// Surrounded-with-whitespace valid values should pass trimming
	// and validate on their trimmed content.
	path := writeTemp(t, "app:\n  name: \"   \"\n  module: \"  github.com/acme/x  \"\nservices:\n  xsuaa: valid-xsuaa\n  destination: valid-dest\n  connectivity: valid-cc\ncf:\n  api: \"  https://api.cf.eu10.hana.ondemand.com  \"\n  org: \"   \"\n  space: dev\n  domain: cfapps.eu10.hana.ondemand.com\n")
	_, err := LoadConfig(path)
	then.AssertThat(t, err, is.Not(is.Nil()))
	msg := err.Error()
	// Whitespace-only values should each be flagged as required.
	then.AssertThat(t, strings.Contains(msg, "app.name is required"), is.True())
	then.AssertThat(t, strings.Contains(msg, "cf.org is required"), is.True())
	// Trimmed-and-valid values should NOT be flagged.
	then.AssertThat(t, strings.Contains(msg, "app.module"), is.False())
	then.AssertThat(t, strings.Contains(msg, "cf.api"), is.False())
}
