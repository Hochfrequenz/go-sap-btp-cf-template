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

// Result accumulates per-rewriter outcomes for reporting. DryRun is
// surfaced so Report can lead with an unambiguous banner — an operator
// must never mistake a preview run for an applied one.
type Result struct {
	Rewriters []RewriterResult
	DryRun    bool
}

// RewriterResult holds the before and after bytes for one rewriter's
// application. Before == After means no-op.
type RewriterResult struct {
	Name, Path    string
	Before, After []byte
}

// pending is a file's plan entry: an absolute path plus the computed
// result. Used internally by Run to separate the Transform phase
// (may fail) from the write phase (commits to disk).
type pending struct {
	absPath string
	result  RewriterResult
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
	if r.DryRun {
		fmt.Fprintln(&b, "-- dry run, no files written --")
	}
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

// Run drives all rewriters in two phases — plan everything in memory
// first, only write if every Transform succeeded. A failure in any
// rewriter thus leaves the tree untouched instead of half-applied.
// If dryRun, the write phase is skipped entirely.
func Run(root string, cfg *Config, dryRun bool) (*Result, error) {
	// The Go-imports rewriter needs to know the *current* module path
	// so it can translate "oldModule/..." → "newModule/..." anchored
	// correctly. Read it once up front.
	oldModule, err := readGoModulePath(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil, fmt.Errorf("discover current go module path: %w", err)
	}

	res := &Result{DryRun: dryRun}

	// --- Phase 1: plan. Every Transform runs; no writes. ---
	var plan []pending

	for _, rw := range singleFileRewriters() {
		absPath := filepath.Join(root, rw.Path)
		old, err := os.ReadFile(absPath)
		if err != nil {
			return nil, fmt.Errorf("%s: read %s: %w", rw.Name, absPath, err)
		}
		newContent, err := rw.Transform(old, cfg)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rw.Name, err)
		}
		rr := RewriterResult{Name: rw.Name, Path: rw.Path, Before: old, After: newContent}
		plan = append(plan, pending{absPath: absPath, result: rr})
		res.Rewriters = append(res.Rewriters, rr)
	}

	// Walk the tree for *.go files — this isn't a single path, it's
	// a glob, so it lives outside singleFileRewriters().
	goPlan, err := planGoImports(root, oldModule, cfg)
	if err != nil {
		return nil, err
	}
	for _, p := range goPlan {
		plan = append(plan, p)
		res.Rewriters = append(res.Rewriters, p.result)
	}

	// Walk examples/ for the destination-name literal. Same plan-not-
	// write contract as planGoImports — Phase-2 atomicity preserved.
	examplesPlan, err := planExamplesDestination(root, cfg)
	if err != nil {
		return nil, err
	}
	for _, p := range examplesPlan {
		plan = append(plan, p)
		res.Rewriters = append(res.Rewriters, p.result)
	}

	// --- Phase 2: write. Only reached if every Transform in Phase 1
	// succeeded, so a half-applied tree is impossible. ---
	if dryRun {
		return res, nil
	}
	for _, p := range plan {
		if bytes.Equal(p.result.Before, p.result.After) {
			continue
		}
		if err := os.WriteFile(p.absPath, p.result.After, 0644); err != nil {
			return nil, fmt.Errorf("%s: write %s: %w", p.result.Name, p.absPath, err)
		}
	}
	return res, nil
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
			return nil, fmt.Errorf("deploy.yml: no `%s:` line found (expected in the workflow's top-level `env:` block)", s.key)
		}
		result = re.ReplaceAll(result, []byte("${1}: "+s.newValue))
	}
	return result, nil
}

// --- Go imports walker --------------------------------------------------

// planGoImports finds every *.go file under root, applies the quoted
// module-prefix replacement anchored on `"<oldModule>"` or `"<oldModule>/`
// (so sibling prefixes like `<oldModule>-extra` are not matched), and
// returns a plan of per-file rewrites. No files are written — callers
// (Run) decide whether to commit the plan, so a failure elsewhere cannot
// leave the tree half-applied.
func planGoImports(root, oldModule string, cfg *Config) ([]pending, error) {
	re := regexp.MustCompile(`"` + regexp.QuoteMeta(oldModule) + `([/"])`)
	replacement := []byte(`"` + cfg.App.Module + `$1`)

	var out []pending
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
		out = append(out, pending{
			absPath: p,
			result: RewriterResult{
				Name:   "go imports",
				Path:   rel,
				Before: old,
				After:  newContent,
			},
		})
		return nil
	})
	return out, err
}

// --- examples destination-name walker ----------------------------------

// planExamplesDestination finds every *.go file under examples/, replaces
// every quoted occurrence of the current destination-name literal with
// the configured `examples.destination_name`, and returns a plan of
// per-file rewrites. The "current" literal is discovered by looking for
// a `destinationName = "..."` constant in the first matching file
// (alphabetical walk) — same source-of-truth pattern as planGoImports
// reading go.mod for the current module path.
//
// No files are written — callers (Run) decide whether to commit the
// plan, so a failure elsewhere cannot leave the tree half-applied.
//
// Edge cases:
//   - examples/ doesn't exist (fork removed it): returns empty plan, no error.
//   - no .go file under examples/ has a `destinationName = "..."` constant:
//     returns empty plan, no error — there's nothing for the rewriter to do.
//   - current literal already matches `cfg.Examples.DestinationName`:
//     plan still includes every file (Before == After), Run skips the writes.
func planExamplesDestination(root string, cfg *Config) ([]pending, error) {
	examplesDir := filepath.Join(root, "examples")
	info, err := os.Stat(examplesDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat examples dir: %w", err)
	}
	if !info.IsDir() {
		return nil, nil
	}

	current, err := discoverCurrentExamplesDestination(examplesDir)
	if err != nil {
		return nil, err
	}
	if current == "" {
		// No `destinationName = "..."` const found anywhere under
		// examples/. Nothing for this rewriter to do.
		return nil, nil
	}
	desired := cfg.Examples.DestinationName

	re := regexp.MustCompile(`"` + regexp.QuoteMeta(current) + `"`)
	replacement := []byte(`"` + desired + `"`)

	var out []pending
	err = filepath.WalkDir(examplesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		old, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		newContent := re.ReplaceAll(old, replacement)
		rel, _ := filepath.Rel(root, p)
		out = append(out, pending{
			absPath: p,
			result: RewriterResult{
				Name:   "examples destination",
				Path:   rel,
				Before: old,
				After:  newContent,
			},
		})
		return nil
	})
	return out, err
}

// discoverCurrentExamplesDestination walks examplesDir alphabetically
// and returns the first `destinationName = "..."` literal it finds in
// any *.go file. Returns "" with nil error when none is found — the
// caller treats that as a "no-op" signal, not an error.
func discoverCurrentExamplesDestination(examplesDir string) (string, error) {
	re := regexp.MustCompile(`destinationName\s*=\s*"([^"]+)"`)
	var found string
	err := filepath.WalkDir(examplesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		if found != "" {
			return nil // already found; let walk finish so SkipAll isn't needed
		}
		raw, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		if m := re.FindSubmatch(raw); m != nil {
			found = string(m[1])
		}
		return nil
	})
	return found, err
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
