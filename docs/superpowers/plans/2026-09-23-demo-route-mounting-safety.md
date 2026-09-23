# Demo-Route Mounting Safety (#125) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a route-table allow-list test that fails CI when an unexpected route is mounted on the router, plus a README signpost row + gate entry, so a fork consciously decides to mount demo routes rather than inheriting them silently.

**Architecture:** `buildRouter`'s validator parameter changes from the concrete `*btp.JWTValidator` to an unexported one-method interface `routeGuard`, letting the route-table test pass a no-op fake and avoid the live JWKS fetch. The test walks `r.Routes()` (the real gin route table — not the OpenAPI doc, which is blind to gin-registered `POST /api/adt-checkrun`) and asserts an explicit allow-list, pinned from first-run output. A README row + a `PATTERNS` heredoc entry in `template-guards.yml` bit-rot-check the demo `Register` calls.

**Tech Stack:** Go 1.24, gin, huma v2.38.0, gocrest matchers, GitHub Actions (template-guards.yml).

**Spec:** `docs/superpowers/specs/2026-09-23-demo-route-mounting-safety-design.md`

---

## File Structure

| File | Responsibility | Change |
| --- | --- | --- |
| `cmd/server/main.go` | Wiring + `buildRouter`. | Add `routeGuard` interface; change `buildRouter` validator param type. |
| `cmd/server/router_test.go` | New. Route-table allow-list test. | Create. |
| `README.md` | Manual-fork-chores table. | One signpost row. |
| `.github/workflows/template-guards.yml` | Bit-rot gate for the chores table. | One `PATTERNS` heredoc entry. |

---

### Task 1: Introduce the `routeGuard` interface and retype `buildRouter`

**Files:**
- Modify: `cmd/server/main.go:187`

The interface is unexported, `cmd/server`-local. `*btp.JWTValidator` already satisfies it (`internal/btp/auth.go:78` defines `func (v *JWTValidator) Middleware() gin.HandlerFunc`), so `main()` at `main.go:55` keeps passing its real validator unchanged.

- [ ] **Step 1: Add the interface + change the signature**

In `cmd/server/main.go`, immediately above `buildRouter` (before its doc comment at line 172), add:

```go
// routeGuard is the wiring-level dependency buildRouter needs: something
// that supplies a gin middleware enforcing a valid JWT. *btp.JWTValidator
// satisfies it. Decoupling buildRouter from the concrete type lets the
// route-table test pass a no-op fake (see router_test.go) instead of a
// real validator, whose constructor does a live JWKS fetch.
type routeGuard interface {
	Middleware() gin.HandlerFunc
}
```

Then change the signature at line 187 from:

```go
func buildRouter(validator *btp.JWTValidator, caller btp.OnPremCaller, mutator btp.OnPremMutator, logger *slog.Logger) *gin.Engine {
```

to:

```go
func buildRouter(validator routeGuard, caller btp.OnPremCaller, mutator btp.OnPremMutator, logger *slog.Logger) *gin.Engine {
```

No other change in `main.go`. `main()` still passes `validator` (a `*btp.JWTValidator`) at line 55 — it satisfies `routeGuard`.

- [ ] **Step 2: Verify the package still compiles**

Run: `go build ./cmd/server/`
Expected: no output, exit 0. (If it fails, `btp` is no longer referenced — it isn't, `main()` still uses it elsewhere; confirm `import "github.com/hochfrequenz/.../internal/btp"` stays.)

Run: `go vet ./cmd/server/`
Expected: no output, exit 0.

- [ ] **Step 3: Verify the existing server tests still pass**

Run: `go test ./cmd/server/ -v`
Expected: PASS — `Test_logLevelFromEnv_MapsKnownAndUnknown`, `Test_recoverPanic_*`, `Test_requestLog_*`, `Test_securityHeaders_*` all green. None of these call `buildRouter`, so the signature change is invisible to them.

- [ ] **Step 4: Commit**

```bash
git add cmd/server/main.go
git commit -m "refactor(server): buildRouter depends on routeGuard interface

Decouples buildRouter from the concrete *btp.JWTValidator so a
route-table test can pass a no-op fake instead of triggering the
live JWKS fetch in NewJWTValidator. *btp.JWTValidator already
satisfies the one-method interface; main() is unchanged.

Part of #125."
```

---

### Task 2: Write the route-table allow-list test (failing first)

**Files:**
- Create: `cmd/server/router_test.go`

The test is in package `main` (same package as `buildRouter`) so it can reference the unexported `routeGuard`. It reuses the one-method fake shape from `examples/adtdiscovery/handler_test.go` (`fakeCaller`) and `examples/adtcheckrun/handler_test.go` (`fakeMutator`) — do not invent a new fake vocabulary.

The allow-list is intentionally pinned from first-run `r.Routes()` output. The first run of this test WILL FAIL with a diff showing the full actual route set (the huma-generated block). That output is the source of truth the implementer copies into `want`.

- [ ] **Step 1: Write the failing test**

Create `cmd/server/router_test.go`:

```go
package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/corbym/gocrest/is"
	"github.com/corbym/gocrest/then"
	"github.com/gin-gonic/gin"

	"github.com/hochfrequenz/go-sap-btp-cf-template/internal/btp"
)

// fakeGuard satisfies the unexported routeGuard interface for tests.
// Its middleware is a pass-through — the route-table test never sends a
// request through it; it only needs a value that lets buildRouter
// type-check, without the live JWKS fetch NewJWTValidator performs.
type fakeGuard struct{}

func (fakeGuard) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) { c.Next() }
}

// fakeRouteCaller is a one-method fake satisfying btp.OnPremCaller, in the
// shape of examples/adtdiscovery/handler_test.go's fakeCaller. The
// route-table test never invokes it; buildRouter just needs a value.
type fakeRouteCaller struct{}

func (fakeRouteCaller) CallOnPremise(_ context.Context, _, _, _ string,
	_ http.Header, _ io.Reader) (*http.Response, error) {
	return nil, nil
}

// fakeRouteMutator is a one-method fake satisfying btp.OnPremMutator, in the
// shape of examples/adtcheckrun/handler_test.go's fakeMutator. Unused at
// call time; buildRouter just needs a value.
type fakeRouteMutator struct{}

func (fakeRouteMutator) CallOnPremiseMutating(_ context.Context, _, _, _ string,
	_ http.Header, _ io.Reader) (*http.Response, error) {
	return nil, nil
}

// Test_RouterAllowList pins the exact set of routes buildRouter mounts.
// It walks the REAL gin route table (r.Routes()), not the OpenAPI document —
// POST /api/adt-checkrun is gin-registered and appears in no OpenAPI spec,
// so an OpenAPI-only assertion would be blind to it. It covers the root
// router too: a demo mounted on the root sits outside the JWT middleware
// entirely, the worse case.
//
// A fork that consciously mounts a new route adds it to want and the gate
// passes again. A route that lands without a want entry fails the build —
// the surprise #125 is about.
func Test_RouterAllowList(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	r := buildRouter(fakeGuard{}, fakeRouteCaller{}, fakeRouteMutator{}, logger)

	got := make(map[string]bool) // "METHOD /path" -> true
	for _, ri := range r.Routes() {
		got[ri.Method+" "+ri.Path] = true
	}

	// want is pinned from the first run of r.Routes() output. The
	// huma-generated block (four OpenAPI-spec variants + /docs +
	// /schemas/:schema) is captured wholesale rather than hand-guessed,
	// because huma v2.38.0 registers two /api/openapi-3.0.* downgrade
	// routes that a hand-list is easy to miss.
	want := map[string]bool{
		"GET /healthz":                  true,
		"GET /version":                 true,
		"GET /api/me":                  true,
		"GET /api/adt-discovery":       true,
		"POST /api/adt-checkrun":       true,
		// --- huma-generated block: replace with first-run output ---
		"GET /api/openapi.json":        true,
		"GET /api/openapi-3.0.json":   true,
		"GET /api/openapi.yaml":        true,
		"GET /api/openapi-3.0.yaml":   true,
		"GET /api/docs":                true,
		"GET /api/schemas/:schema":     true,
	}

	missing := diff(want, got)
	unexpected := diff(got, want)

	if len(missing) > 0 || len(unexpected) > 0 {
		sort.Strings(missing)
		sort.Strings(unexpected)
		t.Errorf("route table drifted from allow-list\n"+
			"  missing (in want, not in router):\n    %s\n"+
			"  unexpected (in router, not in want):\n    %s",
			strings.Join(missing, "\n    "),
			strings.Join(unexpected, "\n    "))
	}
}

// diff returns the keys in a that are not in b.
func diff(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	return out
}
```

- [ ] **Step 2: Run the test and capture the first-run output**

Run: `go test ./cmd/server/ -run Test_RouterAllowList -v`
Expected: FAIL. The error message prints `missing` and `unexpected` lists. Two outcomes:

- If the huma block in `want` is exactly right: the test may PASS on first run. If so, skip to Step 4 (the pinning already matches).
- If it FAILS: copy the actual `r.Routes()` entries from the `unexpected` (or `missing`) list EXACTLY as gin emits them (method + space + path, including `:schema` not `*`, and any `-3.0` variants) into `want`. Re-run until PASS.

The point of pinning from output rather than guessing is that huma's internal route set is version-specific; copying the observed set is authoritative.

- [ ] **Step 3: Reconcile `want` to match observed output**

Edit `want` in `router_test.go` until `go test ./cmd/server/ -run Test_RouterAllowList -v` passes. Only the huma-generated block should need adjustment; the five hand-known routes (`/healthz`, `/version`, `/api/me`, `/api/adt-discovery`, `/api/adt-checkrun`) must be present — if one of those is missing from `got`, something is wrong with `buildRouter`, not with `want`.

- [ ] **Step 4: Run the full server test suite**

Run: `go test ./cmd/server/ -v`
Expected: all tests PASS, including `Test_RouterAllowList`.

- [ ] **Step 5: Prove the test catches a stray route**

Temporarily add a stray route inside `buildRouter` in `cmd/server/main.go`, after line 258 (before `return r`):

```go
		api.GET("/stray", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
```

Run: `go test ./cmd/server/ -run Test_RouterAllowList -v`
Expected: FAIL, with `unexpected` listing `GET /api/stray`.

Then REMOVE the stray route line.

Run: `go test ./cmd/server/ -run Test_RouterAllowList -v`
Expected: PASS (restored).

- [ ] **Step 6: Commit**

```bash
git add cmd/server/router_test.go
git commit -m "test(server): pin shipped route table in allow-list test

Walks r.Routes() (not the OpenAPI doc, which is blind to the
gin-registered POST /api/adt-checkrun) and asserts the exact
shipped route set. Covers the root router too — a demo mounted
there sits outside the JWT middleware. A fork adding a route
consciously extends the allow-list; an unexpected route fails CI.

The surprise #125 is about is now caught mechanically.

Part of #125."
```

---

### Task 3: Add the README signpost row

**Files:**
- Modify: `README.md` (the "Not rewritten — manual fork chores" table, around line 112)

The table has four columns: `Item | Where | How to find | Why not rewritten`. The new row's `How to find` cell contains a runnable `rg` command. ripgrep uses Rust regex — alternation is an unescaped `|`, NOT `\|` (which matches a literal pipe character and would match nothing here).

- [ ] **Step 1: Add the row**

In `README.md`, in the manual-chores table (after the `LICENSE copyright line` row, before the closing of the table), add:

```markdown
| Demo routes | `cmd/server/main.go` | `rg 'adtdiscovery\.Register\|adtcheckrun\.Register' cmd/server/main.go` | The two demo `Register` calls go live once `examples.destination_name` points at your destination; remove them if you don't want the routes. |
```

Note the `How to find` cell shows `rg 'adtdiscovery\.Register\|adtcheckrun\.Register'` — wait. The correct runnable form is an UNESCAPED pipe: `rg 'adtdiscovery\.Register|adtcheckrun\.Register'`. Use that exact form. (The spec iteration caught that `\|` matches nothing under ripgrep's Rust-regex engine.)

So the row to commit is:

```markdown
| Demo routes | `cmd/server/main.go` | `rg 'adtdiscovery\.Register|adtcheckrun\.Register' cmd/server/main.go` | The two demo `Register` calls go live once `examples.destination_name` points at your destination; remove them if you don't want the routes. |
```

- [ ] **Step 2: Verify the pattern matches both lines**

Run: `rg 'adtdiscovery\.Register|adtcheckrun\.Register' cmd/server/main.go`
Expected: two lines of output:
```
	adtdiscovery.Register(hapi, caller)
	adtcheckrun.Register(api, mutator)
```

If only one line prints, the regex is wrong (likely `\|` instead of `|`).

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs(readme): signpost demo routes in manual-fork-chores table

The two demo Register calls go live once a fork sets
examples.destination_name. Adding the row here so a fork-author
sees them as a manual chore, not an invisible default.

Part of #125."
```

---

### Task 4: Wire the bit-rot gate entry in template-guards.yml

**Files:**
- Modify: `.github/workflows/template-guards.yml:262-265` (the `PATTERNS` heredoc)

The manual-chores gate (`.github/workflows/template-guards.yml:244-268`) does NOT parse README rows. It reads `label|cmd` entries from a hardcoded bash heredoc (`<<'PATTERNS' ... PATTERNS`, lines 262-265) and runs each `cmd`; a command that returns non-zero (0 hits) sets `fail=1`. Adding the README row alone does nothing — the pattern must also be appended here, or the spec's bit-rot claim does not hold.

Existing entries (lines 263-264):
```
          LICENSE Hochfrequenz copyright|rg --quiet Hochfrequenz LICENSE
          CODEOWNERS Hochfrequenz team|rg --quiet Hochfrequenz .github/CODEOWNERS
```

- [ ] **Step 1: Append the new entry**

In `.github/workflows/template-guards.yml`, inside the `PATTERNS` heredoc, after the `CODEOWNERS` line (line 264) and before `PATTERNS` (line 265), add:

```
          Demo routes|rg --quiet 'adtdiscovery\.Register|adtcheckrun\.Register' cmd/server/main.go
```

The `label|cmd` shape matches the existing rows. The `IFS='|' read -r label cmd` loop at line 255 splits on the first `|`; the regex's internal `|` is inside single quotes so the shell passes it to `rg` intact, and `rg`'s Rust-regex engine treats the unescaped `|` as alternation.

- [ ] **Step 2: Verify the gate command passes locally**

Run the exact command the gate will run:
```bash
rg --quiet 'adtdiscovery\.Register|adtcheckrun\.Register' cmd/server/main.go
echo "exit=$?"
```
Expected: `exit=0` (both lines match → `--quiet` exits 0 → gate passes on a healthy tree).

Then verify the failure mode: temporarily comment out both `Register` calls in `cmd/server/main.go:257-258`, re-run the command, confirm `exit=1`. Restore the calls afterward. (Building will fail with the calls commented — that's fine, you're only testing the `rg` exit code; comment them, run `rg`, then restore before committing.)

- [ ] **Step 3: Verify the gate's heredoc still parses**

Run a local simulation of the gate loop:
```bash
while IFS='|' read -r label cmd; do
  [ -z "$label" ] && continue
  if ! eval "$cmd"; then
    echo "FAIL: $label"
  else
    echo "OK:   $label"
  fi
done <<'PATTERNS'
LICENSE Hochfrequenz copyright|rg --quiet Hochfrequenz LICENSE
CODEOWNERS Hochfrequenz team|rg --quiet Hochfrequenz .github/CODEOWNERS
Demo routes|rg --quiet 'adtdiscovery\.Register|adtcheckrun\.Register' cmd/server/main.go
PATTERNS
```
Expected: three `OK:` lines (the two existing + `Demo routes`). No `FAIL:` lines.

- [ ] **Step 4: Run the full test suite + vet**

Run: `go vet ./... && go test ./...`
Expected: all PASS. (The workflow file isn't tested by Go, but this confirms nothing else broke.)

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/template-guards.yml
git commit -m "ci(guards): bit-rot-check the demo Register calls

The manual-chores gate reads a hardcoded PATTERNS heredoc, not README
rows — so the new 'Demo routes' README row needs a matching entry
here or it has no bit-rot detection. Removing both Register calls
now makes this entry return 0 hits and fail the gate.

Part of #125."
```

---

### Task 5: Final verification

**Files:** none (verification only).

- [ ] **Step 1: Full build + test + vet**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS.

- [ ] **Step 2: Confirm the spec's Verification criteria**

- `go test ./cmd/server/ -run TestRouter` passes against the shipped route set (allow-list pinned from first-run `r.Routes()`, including the two huma `-3.0` downgrade routes). — Run it; confirm PASS.
- Adding a stray `api.GET("/stray", ...)` to `buildRouter` fails the test with a message naming the unexpected route. — Done in Task 2 Step 5; re-confirm once.
- `main()` still compiles passing its real `*btp.JWTValidator`. — `go build ./cmd/server/` confirms.
- `template-guards.yml`'s manual-chores gate enforces the new row: removing both `Register` calls makes the appended `PATTERNS` entry return 0 hits and the gate fails. — Done in Task 4 Step 2; the local simulation confirms.

- [ ] **Step 3: Confirm the README pattern and gate pattern are identical**

Run:
```bash
grep -n "adtdiscovery" README.md .github/workflows/template-guards.yml
```
Confirm both files use `adtdiscovery\.Register|adtcheckrun\.Register` (unescaped `|`), not `\|`.

- [ ] **Step 4: Final commit if any cleanup remains, else done**

If all steps pass, #125's implementation is complete. The plan produces four commits (one per task) plus this verification step (no commit).

---

## Notes for the implementer

- **TDD discipline:** Task 2 writes the failing test first. The first run WILL likely fail (the huma block is a guess until pinned). That's the design — pin from observed output, don't hand-guess. Do not "fix" the test by deleting entries that don't match; reconcile `want` to the observed truth.
- **The `\|` trap:** ripgrep's Rust-regex engine treats `\|` as a literal pipe. The pattern must use an unescaped `|`. This bit the spec twice during review; do not reintroduce it.
- **Package choice:** `router_test.go` is in package `main` (not `main_test`) because it references the unexported `routeGuard` interface and `buildRouter`. This matches `main_test.go` which is also package `main`.
- **No `RequireScope`:** Per the spec's non-goals, the demos stay mounted with only `validator.Middleware()` guarding them. Do not add scope checks.
- **The `http` import:** Task 1 does not need `net/http`; Task 2's test file imports it for the fake signatures. The stray-route probe in Task 2 Step 5 uses `gin.H` and `http.StatusOK` — ensure `net/http` is imported in `main.go` (it already is, line 13).
