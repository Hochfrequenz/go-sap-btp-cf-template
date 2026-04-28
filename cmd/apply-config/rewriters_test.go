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

// planResultFor extracts the (Before, After) pair for a given relative
// path from a planGoImports plan slice. Helper to keep the tests tidy.
func planResultFor(t *testing.T, plan []pending, relPath string) (string, string) {
	t.Helper()
	for _, p := range plan {
		if filepath.ToSlash(p.result.Path) == relPath {
			return string(p.result.Before), string(p.result.After)
		}
	}
	t.Fatalf("plan does not include %q; paths: %v", relPath, planPaths(plan))
	return "", ""
}

func planPaths(plan []pending) []string {
	var out []string
	for _, p := range plan {
		out = append(out, filepath.ToSlash(p.result.Path))
	}
	return out
}

func Test_planGoImports_RewritesAnchoredPrefix(t *testing.T) {
	// Build a throwaway tree:
	//   root/go.mod                  (module github.com/hochfrequenz/mwe)
	//   root/cmd/server/main.go      (imports the module, plus a subpackage)
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
	plan, err := planGoImports(dir, "github.com/hochfrequenz/mwe", cfg)
	then.AssertThat(t, err, is.Nil())

	// main.go gets rewritten in the plan, both the root import and the
	// subpackage import.
	_, mainAfter := planResultFor(t, plan, "cmd/server/main.go")
	then.AssertThat(t, strings.Contains(mainAfter, `"github.com/acme/cool-service/internal/btp"`), is.True())
	then.AssertThat(t, strings.Contains(mainAfter, `"github.com/acme/cool-service"`), is.True())
	// Prefix-collision file stays untouched in the plan.
	unBefore, unAfter := planResultFor(t, plan, "pkg/unaffected.go")
	then.AssertThat(t, unBefore, is.EqualTo(unAfter))
	then.AssertThat(t, strings.Contains(unAfter, "mwe-extra/lib"), is.True())

	// planGoImports is a PLAN, not a write. The on-disk file must still
	// match the original — no writes happen until Run's Phase 2.
	ondisk, _ := os.ReadFile(filepath.Join(dir, "cmd", "server", "main.go"))
	then.AssertThat(t, strings.Contains(string(ondisk), "github.com/hochfrequenz/mwe"), is.True())
	then.AssertThat(t, strings.Contains(string(ondisk), "github.com/acme"), is.False())
}

func Test_planGoImports_BlankAndAliasedImports(t *testing.T) {
	// A fork may use blank imports (`_ "module"`) or aliased imports
	// (`alias "module"`). The anchor `"<module>"` or `"<module>/`
	// should still hit them; this test locks that in.
	dir := t.TempDir()
	then.AssertThat(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module github.com/hochfrequenz/mwe\n"), 0644), is.Nil())
	then.AssertThat(t, os.WriteFile(filepath.Join(dir, "file.go"),
		[]byte(`package p

import (
	_     "github.com/hochfrequenz/mwe/side/effect"
	btp   "github.com/hochfrequenz/mwe/internal/btp"
	. "github.com/hochfrequenz/mwe/dotted"
)
`), 0644), is.Nil())

	cfg := testConfig()
	cfg.App.Module = "github.com/acme/x"
	plan, err := planGoImports(dir, "github.com/hochfrequenz/mwe", cfg)
	then.AssertThat(t, err, is.Nil())

	_, after := planResultFor(t, plan, "file.go")
	for _, want := range []string{
		`_     "github.com/acme/x/side/effect"`,
		`btp   "github.com/acme/x/internal/btp"`,
		`. "github.com/acme/x/dotted"`,
	} {
		if !strings.Contains(after, want) {
			t.Errorf("expected %q in rewritten file; got:\n%s", want, after)
		}
	}
}

func Test_planExamplesDestination_RewritesQuotedLiteralAcrossExamples(t *testing.T) {
	// Setup throwaway examples/ tree:
	//   examples/foo/handler.go      — has destinationName = "HF_S4" + a usage
	//   examples/foo/handler_test.go — asserts is.EqualTo("HF_S4")
	//   examples/bar/handler.go      — no HF_S4 anywhere
	dir := t.TempDir()
	writeFile := func(rel, content string) {
		full := filepath.Join(dir, rel)
		then.AssertThat(t, os.MkdirAll(filepath.Dir(full), 0755), is.Nil())
		then.AssertThat(t, os.WriteFile(full, []byte(content), 0644), is.Nil())
	}
	writeFile("examples/foo/handler.go",
		`package foo

const (
	destinationName = "HF_S4"
	sapPath         = "/sap/bc/adt/discovery"
)

var x = destinationName // uses the const, not the literal directly
`)
	writeFile("examples/foo/handler_test.go",
		`package foo_test

import "testing"

func Test_X(t *testing.T) {
	if got := "HF_S4"; got != "HF_S4" {
		t.Fail()
	}
}
`)
	writeFile("examples/bar/handler.go",
		`package bar

func Hello() string { return "hello" }
`)

	cfg := testConfig()
	cfg.Examples.DestinationName = "ACME_S4"
	plan, err := planExamplesDestination(dir, cfg)
	then.AssertThat(t, err, is.Nil())

	// foo/handler.go is rewritten: "HF_S4" → "ACME_S4" (twice — const + assertion)
	_, fooHandlerAfter := planResultFor(t, plan, "examples/foo/handler.go")
	then.AssertThat(t, strings.Contains(fooHandlerAfter, `"ACME_S4"`), is.True())
	then.AssertThat(t, strings.Contains(fooHandlerAfter, `"HF_S4"`), is.False())

	// foo/handler_test.go is rewritten too
	_, fooTestAfter := planResultFor(t, plan, "examples/foo/handler_test.go")
	then.AssertThat(t, strings.Contains(fooTestAfter, `"ACME_S4"`), is.True())
	then.AssertThat(t, strings.Contains(fooTestAfter, `"HF_S4"`), is.False())

	// bar/handler.go is in the plan but unchanged (Before == After)
	barBefore, barAfter := planResultFor(t, plan, "examples/bar/handler.go")
	then.AssertThat(t, barBefore, is.EqualTo(barAfter))

	// PLAN, not write — on-disk file must still have HF_S4.
	ondisk, _ := os.ReadFile(filepath.Join(dir, "examples", "foo", "handler.go"))
	then.AssertThat(t, strings.Contains(string(ondisk), "HF_S4"), is.True())
	then.AssertThat(t, strings.Contains(string(ondisk), "ACME_S4"), is.False())
}

func Test_planExamplesDestination_NoOpWhenAlreadyMatches(t *testing.T) {
	// After fork: code already says my-dest, config says my-dest, plan
	// should be all no-ops (Before == After everywhere).
	dir := t.TempDir()
	writeFile := func(rel, content string) {
		full := filepath.Join(dir, rel)
		then.AssertThat(t, os.MkdirAll(filepath.Dir(full), 0755), is.Nil())
		then.AssertThat(t, os.WriteFile(full, []byte(content), 0644), is.Nil())
	}
	writeFile("examples/foo/handler.go",
		`package foo

const destinationName = "my-dest"
`)

	cfg := testConfig()
	cfg.Examples.DestinationName = "my-dest"
	plan, err := planExamplesDestination(dir, cfg)
	then.AssertThat(t, err, is.Nil())

	for _, p := range plan {
		then.AssertThat(t, string(p.result.Before), is.EqualTo(string(p.result.After)))
	}
}

func Test_planExamplesDestination_NoOpWhenNoExamplesDir(t *testing.T) {
	// Fork has deleted examples/. Rewriter should return cleanly with
	// an empty plan — there's nothing to rewrite.
	dir := t.TempDir() // no examples/ subdir
	cfg := testConfig()
	cfg.Examples.DestinationName = "ACME_S4"
	plan, err := planExamplesDestination(dir, cfg)
	then.AssertThat(t, err, is.Nil())
	then.AssertThat(t, len(plan), is.EqualTo(0))
}

func Test_Run_IsAtomicWhenATransformFails(t *testing.T) {
	// Phase-2 write must not run if any Phase-1 Transform errors. Set
	// up a tree where go.mod is fine but manifest.yml is intentionally
	// broken (no matching services: block); transformManifestYml will
	// fail, and we verify go.mod on disk is unchanged afterwards.
	dir := t.TempDir()
	writeFile := func(rel, content string) {
		full := filepath.Join(dir, rel)
		then.AssertThat(t, os.MkdirAll(filepath.Dir(full), 0755), is.Nil())
		then.AssertThat(t, os.WriteFile(full, []byte(content), 0644), is.Nil())
	}
	writeFile("go.mod", "module github.com/hochfrequenz/mwe\n")
	writeFile("manifest.yml", "applications:\n  - name: foo\n    # no services block — transform will fail\n")
	// Other files present so reads don't fail before manifest's Transform runs.
	writeFile("xs-security.json", `{"xsappname":"mwe"}`)
	writeFile("vars.example.yml", "backend-host: mwe\ndomain: cfapps.eu10.hana.ondemand.com\n")
	writeFile("web/package.json", `{"name":"mwe-web"}`)
	writeFile(".github/workflows/deploy.yml", "env:\n  CF_API: x\n  CF_ORG: x\n  CF_SPACE: x\n  BACKEND_HOST: x\n  DOMAIN: x\n")

	cfg := testConfig()
	_, err := Run(dir, cfg, false /* dryRun */)
	then.AssertThat(t, err, is.Not(is.Nil())) // transformManifestYml should fail

	// go.mod on disk must still be the pre-run content — the atomicity
	// guarantee. If this fails, the tree is half-applied.
	gomod, _ := os.ReadFile(filepath.Join(dir, "go.mod"))
	then.AssertThat(t, string(gomod), is.EqualTo("module github.com/hochfrequenz/mwe\n"))
}

func Test_transformVarsExampleYml_PreservesCRLFLineEndings(t *testing.T) {
	in := []byte("backend-host: go-btp-mwe\r\ndomain: cfapps.eu10.hana.ondemand.com\r\n")
	out, err := transformVarsExampleYml(in, testConfig())
	then.AssertThat(t, err, is.Nil())
	for _, want := range []string{
		"backend-host: acme-app\r\n",
		"domain: cfapps.us10.hana.ondemand.com\r\n",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing exact byte sequence %q; got:\n%q", want, string(out))
		}
	}
}
