package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"
)

// testConfig returns a config that intentionally differs in every
// rewritable field from the baseline "go-btp-mwe" template, so a
// correct rewriter touches every field and a broken one fails
// visibly on at least one.
func testConfig() *Config {
	cfg := &Config{
		App: AppConfig{
			Name:   "acme-app",
			Module: "github.com/acme/cool-service",
		},
		Services: ServicesConfig{
			XSUAA:        "acme-xsuaa",
			Destination:  "acme-dest",
			Connectivity: "acme-cc",
		},
		CF: CFConfig{
			API:    "https://api.cf.us10.hana.ondemand.com",
			Org:    "ACME_core",
			Space:  "prod",
			Domain: "cfapps.us10.hana.ondemand.com",
		},
	}
	return cfg
}

func Test_transformGoMod_ReplacesModuleLine(t *testing.T) {
	in := []byte("module github.com/hochfrequenz/go-sap-btp-cloud-foundry-mwe\n\ngo 1.26\n\nrequire (\n\tgithub.com/foo/bar v1.0.0\n)\n")
	out, err := transformGoMod(in, testConfig())
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, strings.Contains(string(out), "module github.com/acme/cool-service"), is.True())
	// nothing else should change
	then.AssertThat(t, strings.Contains(string(out), "go 1.26"), is.True())
	then.AssertThat(t, strings.Contains(string(out), "github.com/foo/bar v1.0.0"), is.True())
}

func Test_transformGoMod_MissingModuleLine(t *testing.T) {
	_, err := transformGoMod([]byte("go 1.26\n"), testConfig())
	then.AssertThat(t, err, is.Not(is.Nil()))
}

func Test_transformGoMod_Idempotent(t *testing.T) {
	in := []byte("module github.com/acme/cool-service\n")
	out, err := transformGoMod(in, testConfig())
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, string(out), is.EqualTo(string(in)))
}

func Test_transformManifestYml_RenamesAllThreeServices(t *testing.T) {
	in := []byte(`applications:
  - name: ((backend-host))
    services:
      - go-xsuaa
      - go-dest
      - go-cc
  - name: ((backend-host))-web
    services:
      - go-xsuaa
`)
	out, err := transformManifestYml(in, testConfig())
	then.AssertThat(t, err, is.Nil())
	s := string(out)
	for _, want := range []string{"- acme-xsuaa", "- acme-dest", "- acme-cc"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	// approuter also gets the xsuaa rename
	then.AssertThat(t, strings.Count(s, "- acme-xsuaa"), is.EqualTo(2))
	// old names gone
	for _, old := range []string{"- go-xsuaa", "- go-dest", "- go-cc"} {
		if strings.Contains(s, old) {
			t.Errorf("old %q still present:\n%s", old, s)
		}
	}
}

func Test_transformManifestYml_ServicesWithExistingApplicationPrefix(t *testing.T) {
	// A fork may already be named `foo`, so its services are foo-xsuaa, etc.
	// The transform must still find them by suffix-heuristic.
	in := []byte(`applications:
  - name: foo
    services:
      - foo-xsuaa
      - foo-destination
      - foo-connectivity
`)
	out, err := transformManifestYml(in, testConfig())
	then.AssertThat(t, err, is.Nil())
	s := string(out)
	for _, want := range []string{"- acme-xsuaa", "- acme-dest", "- acme-cc"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func Test_transformManifestYml_FailsWhenServiceListIsIncomplete(t *testing.T) {
	in := []byte(`applications:
  - name: foo
    services:
      - go-xsuaa
`) // no dest, no cc — transform must fail-loud
	_, err := transformManifestYml(in, testConfig())
	then.AssertThat(t, err, is.Not(is.Nil()))
}

func Test_transformXsSecurityJson(t *testing.T) {
	in := []byte(`{
  "xsappname": "go-btp-mwe",
  "tenant-mode": "dedicated"
}`)
	out, err := transformXsSecurityJson(in, testConfig())
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, strings.Contains(string(out), `"xsappname": "acme-app"`), is.True())
	then.AssertThat(t, strings.Contains(string(out), `"tenant-mode": "dedicated"`), is.True())
}

func Test_transformXsSecurityJson_MissingField(t *testing.T) {
	_, err := transformXsSecurityJson([]byte(`{"foo": "bar"}`), testConfig())
	then.AssertThat(t, err, is.Not(is.Nil()))
}

func Test_transformVarsExampleYml(t *testing.T) {
	in := []byte(`backend-host: go-btp-mwe
domain: cfapps.eu10.hana.ondemand.com
`)
	out, err := transformVarsExampleYml(in, testConfig())
	then.AssertThat(t, err, is.Nil())
	s := string(out)
	then.AssertThat(t, strings.Contains(s, "backend-host: acme-app"), is.True())
	then.AssertThat(t, strings.Contains(s, "domain: cfapps.us10.hana.ondemand.com"), is.True())
}

func Test_transformVarsExampleYml_MissingLine(t *testing.T) {
	_, err := transformVarsExampleYml([]byte("backend-host: foo\n"), testConfig())
	then.AssertThat(t, err, is.Not(is.Nil()))
}

func Test_transformPackageJson_RewritesTopLevelNameOnly(t *testing.T) {
	in := []byte(`{
  "name": "go-btp-mwe-web",
  "version": "1.0.0",
  "dependencies": {
    "some-dep": {
      "name": "some-dep"
    }
  }
}`)
	out, err := transformPackageJson(in, testConfig())
	then.AssertThat(t, err, is.Nil())
	s := string(out)
	then.AssertThat(t, strings.Contains(s, `"name": "acme-app-web"`), is.True())
	// Nested "name": "some-dep" MUST be untouched (only first match is replaced).
	then.AssertThat(t, strings.Contains(s, `"name": "some-dep"`), is.True())
}

func Test_transformDeployYml_RewritesEnvBlock(t *testing.T) {
	in := []byte(`env:
  CF_API: https://api.cf.eu10.hana.ondemand.com
  CF_ORG: HF Dev Account_hf-cf
  CF_SPACE: dev
  BACKEND_HOST: go-btp-mwe
  DOMAIN: cfapps.eu10.hana.ondemand.com
  GIN_MODE: release
`)
	out, err := transformDeployYml(in, testConfig())
	then.AssertThat(t, err, is.Nil())
	s := string(out)
	for _, want := range []string{
		"CF_API: https://api.cf.us10.hana.ondemand.com",
		"CF_ORG: ACME_core",
		"CF_SPACE: prod",
		"BACKEND_HOST: acme-app",
		"DOMAIN: cfapps.us10.hana.ondemand.com",
		"GIN_MODE: release", // must not touch unrelated keys
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func Test_transformDeployYml_PreservesCRLFLineEndings(t *testing.T) {
	// Windows checkouts store .yml with CRLF. A naive `.*$` regex
	// silently strips the \r, producing a mixed-ending file. This
	// regression test locks in the fix.
	in := []byte("env:\r\n  CF_API: https://api.cf.eu10.hana.ondemand.com\r\n  CF_ORG: HF Dev Account_hf-cf\r\n  CF_SPACE: dev\r\n  BACKEND_HOST: go-btp-mwe\r\n  DOMAIN: cfapps.eu10.hana.ondemand.com\r\n")
	out, err := transformDeployYml(in, testConfig())
	then.AssertThat(t, err, is.Nil())
	// Every rewritten line must still end with \r\n.
	for _, want := range []string{
		"CF_API: https://api.cf.us10.hana.ondemand.com\r\n",
		"CF_ORG: ACME_core\r\n",
		"CF_SPACE: prod\r\n",
		"BACKEND_HOST: acme-app\r\n",
		"DOMAIN: cfapps.us10.hana.ondemand.com\r\n",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing exact byte sequence %q; got:\n%q", want, string(out))
		}
	}
}

func Test_transformManifestYml_PreservesCRLFLineEndings(t *testing.T) {
	in := []byte("applications:\r\n  - name: app\r\n    services:\r\n      - go-xsuaa\r\n      - go-dest\r\n      - go-cc\r\n")
	out, err := transformManifestYml(in, testConfig())
	then.AssertThat(t, err, is.Nil())
	for _, want := range []string{
		"- acme-xsuaa\r\n",
		"- acme-dest\r\n",
		"- acme-cc\r\n",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing exact byte sequence %q; got:\n%q", want, string(out))
		}
	}
}

func Test_transformDeployYml_FailsWhenExpectedKeyMissing(t *testing.T) {
	in := []byte(`env:
  CF_API: https://api.cf.eu10.hana.ondemand.com
`)
	_, err := transformDeployYml(in, testConfig())
	then.AssertThat(t, err, is.Not(is.Nil()))
	then.AssertThat(t, strings.Contains(err.Error(), "CF_ORG"), is.True())
}

func Test_walkGoImports_RewritesAnchoredPrefix(t *testing.T) {
	// Build a throwaway tree:
	//   root/go.mod                  (module github.com/hochfrequenz/mwe)
	//   root/cmd/server/main.go      (imports the module)
	//   root/pkg/unaffected.go       (imports a similarly-named OTHER module)
	dir := t.TempDir()
	then.AssertThat(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module github.com/hochfrequenz/mwe\n"), 0644), is.Nil())
	then.AssertThat(t, os.MkdirAll(filepath.Join(dir, "cmd", "server"), 0755), is.Nil())
	then.AssertThat(t, os.WriteFile(filepath.Join(dir, "cmd", "server", "main.go"),
		[]byte(`package main

import (
	"github.com/hochfrequenz/mwe/internal/btp"
	"github.com/hochfrequenz/mwe"
)
`), 0644), is.Nil())
	then.AssertThat(t, os.MkdirAll(filepath.Join(dir, "pkg"), 0755), is.Nil())
	// This import shares a PREFIX but is a different module. Must NOT be rewritten.
	then.AssertThat(t, os.WriteFile(filepath.Join(dir, "pkg", "unaffected.go"),
		[]byte(`package pkg

import "github.com/hochfrequenz/mwe-extra/lib"
`), 0644), is.Nil())

	cfg := testConfig()
	cfg.App.Module = "github.com/acme/cool-service"
	results, err := walkGoImports(dir, "github.com/hochfrequenz/mwe", cfg, false /* dryRun */)
	then.AssertThat(t, err, is.Nil())

	// Verify main.go got rewritten
	got, _ := os.ReadFile(filepath.Join(dir, "cmd", "server", "main.go"))
	then.AssertThat(t, strings.Contains(string(got), `"github.com/acme/cool-service/internal/btp"`), is.True())
	then.AssertThat(t, strings.Contains(string(got), `"github.com/acme/cool-service"`), is.True())
	// Verify prefix-collision file untouched
	untouched, _ := os.ReadFile(filepath.Join(dir, "pkg", "unaffected.go"))
	then.AssertThat(t, strings.Contains(string(untouched), "mwe-extra/lib"), is.True())

	// And the result set covers at least both files
	paths := map[string]bool{}
	for _, r := range results {
		paths[r.Path] = true
	}
	then.AssertThat(t, paths[filepath.Join("cmd", "server", "main.go")] || paths["cmd/server/main.go"], is.True())
}

func Test_walkGoImports_DryRunDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	then.AssertThat(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module github.com/hochfrequenz/mwe\n"), 0644), is.Nil())
	then.AssertThat(t, os.WriteFile(filepath.Join(dir, "a.go"),
		[]byte(`package a
import "github.com/hochfrequenz/mwe/sub"
`), 0644), is.Nil())

	cfg := testConfig()
	cfg.App.Module = "github.com/acme/x"
	_, err := walkGoImports(dir, "github.com/hochfrequenz/mwe", cfg, true /* dryRun */)
	then.AssertThat(t, err, is.Nil())

	got, _ := os.ReadFile(filepath.Join(dir, "a.go"))
	then.AssertThat(t, strings.Contains(string(got), "github.com/hochfrequenz/mwe/sub"), is.True())
	then.AssertThat(t, strings.Contains(string(got), "github.com/acme"), is.False())
}
