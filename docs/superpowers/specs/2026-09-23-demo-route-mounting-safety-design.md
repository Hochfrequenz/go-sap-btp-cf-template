# Design: demo-route mounting safety (#125)

Issue: [#125 — Demo routes are mounted by default](https://github.com/Hochfrequenz/go-sap-btp-cf-template/issues/125)

## Problem

`cmd/server/main.go:257-258` mounts two demo handlers unconditionally inside
`buildRouter`:

```go
adtdiscovery.Register(hapi, caller)
adtcheckrun.Register(api, mutator)
```

`apply-config`'s destination-name rewriter rewrites the `destinationName =
"..."` constant inside those demo handlers to the fork's real BTP
destination. So the one documented onboarding step — setting
`examples.destination_name` — arms both demo routes against the fork's real
SAP system, under the destination's technical-user authority. The only gate
is `validator.Middleware()` (any valid token from the bound XSUAA instance);
`buildRouter` uses no `btp.RequireScope` anywhere.

One route is `POST /api/adt-checkrun` through the CSRF handshake
(`adtcheckrun/handler.go:128`, `svc.CallOnPremiseMutating`). It mutates
nothing in SAP (ADT check runs are protocol-mutating, not data-writing), but
it is still a POST to `/sap/bc/adt/checkruns` that a fork did not knowingly
expose. The issue author shipped both to two production deployments and only
noticed when someone asked what `/api/adt-checkrun` was.

Mounting is currently a default, not a decision. The fix makes it a
decision, with a test as the safety net rather than prose a fork-author is
expected to read.

## Goal

A fork that sets `examples.destination_name` should not silently expose live
routes to its real SAP system. Mounting the demos must be a conscious act,
and the safety net must be a test that fails CI when an unexpected route
appears.

## Non-goals

- Removing the demos. They are good crib-sheets; the issue author kept them.
- Gating the demos behind `btp.RequireScope`. Demos carrying a real
  authorization scope mis-teach the constrained-proxy pattern.
- Changing `apply-config`'s destination-name rewriter. Rewriting the literal
  into a handler the fork keeps is correct behaviour.
- Touching `invoicesync` (already an unmounted, compile-only example).
- The OpenAPI `securitySchemes` gap (#127) — separate spec.

## Decision: demos stay mounted + checklist row + route-table test

Per the brainstorming decision, the demos stay mounted and runnable out of
the box. The safety net is a route-table allow-list test, plus one README
signpost row. This relies least on fork-author reading and catches the
surprise mechanically.

## Mechanism 1: route-table allow-list test

New file `cmd/server/router_test.go`.

The test calls `buildRouter`, walks `r.Routes()`, and asserts the route set
matches an explicit allow-list. Any route not on the list fails the build.

Two design points from the issue shape this test directly:

1. **Walk the real route table**, not the OpenAPI document. `POST
   /api/adt-checkrun` is a gin route and appears in no OpenAPI document —
   an OpenAPI-only assertion would be blind to it.
2. **Cover the root router**, not just `/api`. A demo mounted on the root
   router sits outside the JWT middleware entirely — the worse case. Root
   routes (`/healthz`, `/version`) are part of the allow-list too.

### Expected shipped allow-list

The huma-generated routes are pinned wholesale from the first run of
`r.Routes()` rather than enumerated by hand. huma v2.38.0 registers four
OpenAPI-spec routes (`/api/openapi.json`, `/api/openapi-3.0.json`,
`/api/openapi.yaml`, `/api/openapi-3.0.yaml`), plus `/api/docs` and
`/api/schemas/:schema` (gin emits the path-param form `:schema`, not a
`*` wildcard). Hand-enumerating these invites the exact defect the reviewer
caught (two `-3.0` downgrade routes omitted), so the allow-list is built by
capturing the observed route table on first run and committing that set.

The hand-known, non-huma routes the allow-list must contain:

```
GET    /healthz
GET    /version
GET    /api/me
GET    /api/adt-discovery      # huma-registered, appears in OpenAPI
POST   /api/adt-checkrun        # gin-registered, NOT in OpenAPI
```

Plus the huma-generated block (all four OpenAPI-spec variants, `/api/docs`,
`/api/schemas/:schema`), captured from first-run `r.Routes()` output. A huma
version bump that changes internals surfaces as a real diff against the
committed set.

### Failure mode

The failure message lists the unexpected routes (or missing expected ones)
so a fork-author sees what landed, not just that something differed.

Once a fork consciously mounts a demo, it adds its route to the allow-list
and the gate passes again.

## Mechanism 2: the validator testability fix

`buildRouter` today takes a concrete `*btp.JWTValidator`:

```go
func buildRouter(validator *btp.JWTValidator, caller btp.OnPremCaller, mutator btp.OnPremMutator, logger *slog.Logger) *gin.Engine
```

`NewJWTValidator` does a live JWKS fetch (`keyfunc.NewDefaultCtx` over
`xsuaa.JWKSURL()`). A route-table test that builds a real validator hits the
network in CI — unacceptable for a test that never invokes the middleware.

The route-table test only calls `buildRouter` and walks `r.Routes()`. It
never sends a request through `validator.Middleware()`. The validator is, for
this test, pure ballast.

Per the brainstorming decision (interface over testcontainer JWKS over
httptest JWKS), `buildRouter` takes a one-method, unexported interface:

```go
type routeGuard interface { Middleware() gin.HandlerFunc }

func buildRouter(validator routeGuard, caller btp.OnPremCaller, mutator btp.OnPremMutator, logger *slog.Logger) *gin.Engine
```

`*btp.JWTValidator` already satisfies it (it has `Middleware()
gin.HandlerFunc`). `main()` passes its real validator unchanged — no caller
changes. The interface is unexported and `cmd/server`-local; it is a wiring
concern, not a library intent, so it does not touch `internal/btp/doc.go`.

This makes explicit that `buildRouter` depends on "something that gates
routes", not "the concrete JWKS-fetching type".

### The test fakes

- Guard: `type fakeGuard struct{}; func (fakeGuard) Middleware() gin.HandlerFunc { return func(c *gin.Context){c.Next()} }`.
- `caller` / `mutator`: reuse the one-method fake shape from
  `examples/*/handler_test.go` (`fakeOnPrem` / `fakeMutator`). Do not invent
  a new fake vocabulary.
- Logger: `slog.New(slog.NewJSONHandler(io.Discard, nil))`.

## Mechanism 3: README signpost row + gate entry

One row added to the "manual fork chores" table at `README.md:112`:

| Item | Where | How to find | Why not rewritten |
| --- | --- | --- | --- |
| Demo routes | `cmd/server/main.go` | `rg 'adtdiscovery\.Register|adtcheckrun\.Register' cmd/server/main.go` | The two demo `Register` calls go live once `examples.destination_name` points at your destination; remove them if you don't want the routes. |

The pattern is `rg 'adtdiscovery\.Register|adtcheckrun\.Register'` (most
precise; matches both lines 257-258, where `adtdiscovery.Register` takes
`hapi` and `adtcheckrun.Register` takes `api` — a naive `Register\(api`
matches only the second). Use an unescaped `|` for alternation: ripgrep's
default Rust-regex engine treats `\|` as a literal pipe character, which
matches nothing on these lines.

The manual-chores gate does NOT parse README rows. It reads patterns from a
hardcoded bash heredoc in `.github/workflows/template-guards.yml:262-265`
(the `<<'PATTERNS' ... PATTERNS` block). Adding the README row alone does
nothing for bit-rot detection. To wire the safety property — "if someone
removes both calls, the gate fails" — append one line to that heredoc:

```
Demo routes|rg --quiet 'adtdiscovery\.Register|adtcheckrun\.Register' cmd/server/main.go
```

This is why `.github/workflows/template-guards.yml` is in the Files-touched
table below. Without that edit, the README row is pure documentation and
the spec's bit-rot claim does not hold.

## What this does not change

- Demos stay mounted and runnable. No `RequireScope`.
- No change to `apply-config`'s destination rewriter.
- No change to `invoicesync`.
- #127's OpenAPI `securitySchemes` is a separate spec; this test is
  intentionally blind to OpenAPI content, only to the gin route table.

## Files touched

| File | Change |
| --- | --- |
| `cmd/server/main.go` | `buildRouter` signature: `*btp.JWTValidator` → unexported `routeGuard` interface. |
| `cmd/server/router_test.go` | New. Route-table allow-list test. |
| `README.md` | One signpost row in the manual-chores table. |
| `.github/workflows/template-guards.yml` | Append `Demo routes\|rg --quiet '...'` to the `PATTERNS` heredoc (lines 262-265) so the new row is bit-rot-checked. |

## Verification

- `go test ./cmd/server/ -run TestRouter` passes against the shipped route
  set (allow-list pinned from first-run `r.Routes()` output, including the
  two huma `-3.0` downgrade routes).
- Adding a stray `api.GET("/stray", ...)` to `buildRouter` fails the test
  with a message naming the unexpected route.
- `main()` still compiles passing its real `*btp.JWTValidator`.
- `template-guards.yml`'s manual-chores gate enforces the new row: removing
  both `Register` calls makes the appended `PATTERNS` entry return 0 hits
  and the gate fails.
