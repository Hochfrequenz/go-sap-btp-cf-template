package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Rewriter is a pure transform from the current file content + config
// to the desired content. Returning an unchanged slice is a legitimate
// no-op; returning an error means the input did not match this
// rewriter's expected shape — the driver treats that as a hard failure,
// not a silent skip, so a drifted file is surfaced loudly instead of
// leaving the tree half-applied.
type Rewriter struct {
	Name      string
	Path      string // relative to --root
	Transform func(old []byte, cfg *Config) ([]byte, error)
}

// Result accumulates per-rewriter outcomes for reporting.
type Result struct {
	Rewriters []RewriterResult
}

// RewriterResult holds the before and after bytes for one rewriter's
// application. Before == After means no-op.
type RewriterResult struct {
	Name, Path    string
	Before, After []byte
}

// HasChanges returns true if any rewriter produced a non-empty diff.
func (r *Result) HasChanges() bool {
	for _, rr := range r.Rewriters {
		if !bytes.Equal(rr.Before, rr.After) {
			return true
		}
	}
	return false
}

// Report renders a compact per-file summary. For unchanged files a
// single "[ok]" line is shown; for changed files, a short line-by-line
// diff of the first few changes so the operator can eyeball what's
// happening without opening every file.
func (r *Result) Report() string {
	var b strings.Builder
	changed := 0
	for _, rr := range r.Rewriters {
		if bytes.Equal(rr.Before, rr.After) {
			fmt.Fprintf(&b, "[  ok] %s: %s (no change)\n", rr.Name, rr.Path)
			continue
		}
		changed++
		fmt.Fprintf(&b, "[diff] %s: %s\n", rr.Name, rr.Path)
		for _, line := range shortDiff(rr.Before, rr.After, 8) {
			fmt.Fprintf(&b, "         %s\n", line)
		}
	}
	fmt.Fprintf(&b, "\n%d rewriter(s) with changes\n", changed)
	return b.String()
}

// Run drives all rewriters. If dryRun, no files are written — but
// every Transform still runs (so config-vs-tree drift is fully
// detected, not just the first mismatch).
func Run(root string, cfg *Config, dryRun bool) (*Result, error) {
	// The Go-imports rewriter needs to know the *current* module path
	// so it can translate "oldModule/..." → "newModule/..." anchored
	// correctly. Read it once up front.
	oldModule, err := readGoModulePath(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil, fmt.Errorf("discover current go module path: %w", err)
	}

	res := &Result{}
	for _, rw := range singleFileRewriters() {
		rr, err := applyRewriter(root, rw, cfg, dryRun)
		if err != nil {
			return nil, err
		}
		res.Rewriters = append(res.Rewriters, rr)
	}

	// Walk the tree for *.go files — this isn't a single path, it's
	// a glob, so it lives outside singleFileRewriters().
	goResults, err := walkGoImports(root, oldModule, cfg, dryRun)
	if err != nil {
		return nil, err
	}
	res.Rewriters = append(res.Rewriters, goResults...)

	return res, nil
}

func applyRewriter(root string, rw Rewriter, cfg *Config, dryRun bool) (RewriterResult, error) {
	path := filepath.Join(root, rw.Path)
	old, err := os.ReadFile(path)
	if err != nil {
		return RewriterResult{}, fmt.Errorf("%s: read %s: %w", rw.Name, path, err)
	}
	newContent, err := rw.Transform(old, cfg)
	if err != nil {
		return RewriterResult{}, fmt.Errorf("%s: %w", rw.Name, err)
	}
	result := RewriterResult{Name: rw.Name, Path: rw.Path, Before: old, After: newContent}
	if !dryRun && !bytes.Equal(old, newContent) {
		if err := os.WriteFile(path, newContent, 0644); err != nil {
			return RewriterResult{}, fmt.Errorf("%s: write %s: %w", rw.Name, path, err)
		}
	}
	return result, nil
}

// singleFileRewriters returns the fixed, ordered list of per-file
// rewriters. Order is chosen so a reader can follow along with the
// repo's deployment story: module identity, CF manifest, XSUAA config,
// vars template, approuter package, CI workflow.
func singleFileRewriters() []Rewriter {
	return []Rewriter{
		{Name: "go.mod", Path: "go.mod", Transform: transformGoMod},
		{Name: "manifest.yml", Path: "manifest.yml", Transform: transformManifestYml},
		{Name: "xs-security.json", Path: "xs-security.json", Transform: transformXsSecurityJson},
		{Name: "vars.example.yml", Path: "vars.example.yml", Transform: transformVarsExampleYml},
		{Name: "web/package.json", Path: "web/package.json", Transform: transformPackageJson},
		{Name: ".github/workflows/deploy.yml", Path: ".github/workflows/deploy.yml", Transform: transformDeployYml},
	}
}

// --- per-file transforms ------------------------------------------------

func transformGoMod(old []byte, cfg *Config) ([]byte, error) {
	// Replace the single `module <path>` line, preserving any trailing
	// comments after it (there usually are none, but don't eat them if
	// someone added any).
	re := regexp.MustCompile(`(?m)^module\s+\S+`)
	if !re.Match(old) {
		return nil, errors.New("go.mod: no `module <path>` line")
	}
	return re.ReplaceAll(old, []byte("module "+cfg.App.Module)), nil
}

// transformManifestYml rewrites the `services:` bindings in both the
// backend and approuter app blocks.
//
// The manifest lists services as YAML list entries prefixed with "- ":
//
//	services:
//	  - go-xsuaa
//	  - go-dest
//	  - go-cc
//
// We match the "- <name>" token specifically so a bare "go-xsuaa"
// appearing in a comment is not rewritten. Both backend and approuter
// bind to the xsuaa instance, so the xsuaa replacement legitimately
// matches twice.
func transformManifestYml(old []byte, cfg *Config) ([]byte, error) {
	// Find current service names from the manifest so we only replace
	// tokens that actually exist. This makes the transform fail fast
	// if the manifest has drifted (e.g. renamed a service outside of
	// apply-config), rather than silently injecting garbage.
	current, err := discoverCurrentManifestServices(old)
	if err != nil {
		return nil, err
	}
	desired := []string{cfg.Services.XSUAA, cfg.Services.Destination, cfg.Services.Connectivity}

	result := old
	for i, from := range current {
		to := desired[i]
		if from == to {
			continue
		}
		// Anchor as YAML list item: `<indent>- <name>` followed by EOL.
		// Use an explicit line-ending capture rather than `$` so CRLF
		// files have their `\r` preserved through the replacement.
		re := regexp.MustCompile(`(?m)^(\s+-\s+)` + regexp.QuoteMeta(from) + `(\r?\n|\r|$)`)
		if !re.Match(result) {
			return nil, fmt.Errorf("manifest.yml: expected `- %s` list entry not found", from)
		}
		result = re.ReplaceAll(result, []byte("${1}"+to+"${2}"))
	}
	return result, nil
}

// discoverCurrentManifestServices returns the three service names
// currently declared in the backend app's services: block, in xsuaa /
// destination / connectivity order. Uses ordering: xsuaa suffix first,
// then something containing "dest", then something containing "c[cn]".
// Picks the three list entries from the file-wide set.
func discoverCurrentManifestServices(manifest []byte) ([3]string, error) {
	// Collect every "- <name>" list entry anywhere in the manifest.
	re := regexp.MustCompile(`(?m)^\s+-\s+(\S+)\s*$`)
	matches := re.FindAllSubmatch(manifest, -1)
	seen := map[string]bool{}
	var names []string
	for _, m := range matches {
		n := string(m[1])
		if seen[n] {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	var out [3]string
	for _, n := range names {
		switch {
		case strings.Contains(n, "xsuaa") && out[0] == "":
			out[0] = n
		case (strings.Contains(n, "dest") || strings.Contains(n, "destination")) && out[1] == "":
			out[1] = n
		case (strings.Contains(n, "cc") || strings.Contains(n, "conn")) && out[2] == "":
			out[2] = n
		}
	}
	if out[0] == "" || out[1] == "" || out[2] == "" {
		return out, fmt.Errorf("manifest.yml: could not identify all three service bindings (got xsuaa=%q dest=%q conn=%q)", out[0], out[1], out[2])
	}
	return out, nil
}

func transformXsSecurityJson(old []byte, cfg *Config) ([]byte, error) {
	re := regexp.MustCompile(`"xsappname"\s*:\s*"[^"]*"`)
	if !re.Match(old) {
		return nil, errors.New("xs-security.json: no xsappname field found")
	}
	return re.ReplaceAll(old, []byte(fmt.Sprintf(`"xsappname": %q`, cfg.App.Name))), nil
}

func transformVarsExampleYml(old []byte, cfg *Config) ([]byte, error) {
	reHost := regexp.MustCompile(`(?m)^backend-host:\s*\S+`)
	reDom := regexp.MustCompile(`(?m)^domain:\s*\S+`)
	if !reHost.Match(old) {
		return nil, errors.New("vars.example.yml: no `backend-host:` line")
	}
	if !reDom.Match(old) {
		return nil, errors.New("vars.example.yml: no `domain:` line")
	}
	out := reHost.ReplaceAll(old, []byte("backend-host: "+cfg.App.Name))
	out = reDom.ReplaceAll(out, []byte("domain: "+cfg.CF.Domain))
	return out, nil
}

// transformPackageJson rewrites only the top-level "name" field. We
// match the first occurrence rather than every occurrence, so nested
// dependency names are not touched.
func transformPackageJson(old []byte, cfg *Config) ([]byte, error) {
	re := regexp.MustCompile(`"name"\s*:\s*"[^"]*"`)
	loc := re.FindIndex(old)
	if loc == nil {
		return nil, errors.New(`web/package.json: no "name" field found`)
	}
	replacement := []byte(fmt.Sprintf(`"name": %q`, cfg.App.Name+"-web"))
	out := make([]byte, 0, len(old)+len(replacement))
	out = append(out, old[:loc[0]]...)
	out = append(out, replacement...)
	out = append(out, old[loc[1]:]...)
	return out, nil
}

func transformDeployYml(old []byte, cfg *Config) ([]byte, error) {
	subs := []struct{ key, newValue string }{
		{"CF_API", cfg.CF.API},
		{"CF_ORG", cfg.CF.Org},
		{"CF_SPACE", cfg.CF.Space},
		{"BACKEND_HOST", cfg.App.Name},
		{"DOMAIN", cfg.CF.Domain},
	}
	result := old
	for _, s := range subs {
		// Use `[^\r\n]*` instead of `.*$` — Go's `.` matches `\r`, so
		// `.*$` on a CRLF file consumes the CR and the replacement
		// silently strips line endings. `[^\r\n]*` stops at either line
		// terminator without consuming it.
		re := regexp.MustCompile(`(?m)^(\s+` + regexp.QuoteMeta(s.key) + `):\s[^\r\n]*`)
		if !re.Match(result) {
			return nil, fmt.Errorf("deploy.yml: no `%s:` env line found", s.key)
		}
		result = re.ReplaceAll(result, []byte("${1}: "+s.newValue))
	}
	return result, nil
}

// --- Go imports walker --------------------------------------------------

// walkGoImports finds every *.go file under root and replaces the
// quoted module prefix `"<oldModule>` followed by `"` or `/`. Anchoring
// on the trailing `"` or `/` avoids false matches against longer module
// names that share a prefix (e.g. -extra, -v2).
func walkGoImports(root, oldModule string, cfg *Config, dryRun bool) ([]RewriterResult, error) {
	if oldModule == cfg.App.Module {
		// No rewrites will happen, but we still walk and report no-ops
		// so the summary covers every .go file.
	}
	re := regexp.MustCompile(`"` + regexp.QuoteMeta(oldModule) + `([/"])`)
	replacement := []byte(`"` + cfg.App.Module + `$1`)

	var out []RewriterResult
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", ".git", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		old, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		newContent := re.ReplaceAll(old, replacement)
		rel, _ := filepath.Rel(root, p)
		out = append(out, RewriterResult{
			Name:   "go imports",
			Path:   rel,
			Before: old,
			After:  newContent,
		})
		if !dryRun && !bytes.Equal(old, newContent) {
			if err := os.WriteFile(p, newContent, 0644); err != nil {
				return fmt.Errorf("go imports: write %s: %w", p, err)
			}
		}
		return nil
	})
	return out, err
}

// --- helpers -----------------------------------------------------------

func readGoModulePath(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	re := regexp.MustCompile(`(?m)^module\s+(\S+)`)
	m := re.FindSubmatch(raw)
	if m == nil {
		return "", errors.New("go.mod: no `module <path>` line found")
	}
	return string(m[1]), nil
}

// shortDiff returns up to `limit` "- old / + new" lines where old and
// new differ. It is purely for the --dry-run console report and does
// not need to be a proper unified diff.
func shortDiff(a, b []byte, limit int) []string {
	la := strings.Split(string(a), "\n")
	lb := strings.Split(string(b), "\n")
	var out []string
	n := len(la)
	if len(lb) > n {
		n = len(lb)
	}
	for i := 0; i < n && len(out) < limit*2; i++ {
		var oldLine, newLine string
		if i < len(la) {
			oldLine = la[i]
		}
		if i < len(lb) {
			newLine = lb[i]
		}
		if oldLine == newLine {
			continue
		}
		out = append(out, "- "+oldLine)
		out = append(out, "+ "+newLine)
	}
	if len(out) >= limit*2 {
		out = append(out, "... (more diffs elided)")
	}
	return out
}
