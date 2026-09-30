# CLAUDE.md — project rules for AI assistants

You are helping a developer work on a Go template for SAP BTP Cloud Foundry services. This file is the rules + pointers an AI assistant needs to stay on-rails. Read it on every prompt before changing code.

The reasons behind each rule live in `README.md` — this file deliberately does not duplicate that prose. When a rule cites a section, follow the link to confirm the why before deviating.

## Audience reminder

Many fork-authors here are SAP-fluent (ABAP / cockpit / Cloud Connector) but not Go-fluent — they trust your output. That makes drift from these rules invisible to them. Stay strict.

## When asked to add an HTTP handler

1. **Pick the style.** huma (typed, OpenAPI-generated) vs gin (direct claim access, `ginpingo.AbortError`). Decision table at README §"OpenAPI 3.1 + interactive docs via huma" (right after the huma example): GET + want OpenAPI → huma; POST/PUT/DELETE/PATCH + need `user_name` for audit → gin; everything else → gin (default). Never mix the two styles in one handler.
2. **Place files.** New handlers live at `examples/<your-name>/handler.go` + `handler_test.go`. Tests sit next to the handler.
3. **Register the route** from `buildRouter` in `cmd/server/main.go` (`func buildRouter`). New routes hang off the JWT-guarded `api` group. Writes on api must be JSON (requireJSONBody, 415 otherwise); a non-JSON route goes on a sibling `r.Group("/api", authMW)`.
4. **Depend on interfaces, not Service.** Handler signatures take `btpingo.OnPremCaller` (reads) or `btpingo.OnPremMutator` (CSRF writes). Never `*btpingo.Service`. The full library-intent surface lives at `btpingo`'s `doc.go` (https://pkg.go.dev/github.com/hochfrequenz/btpingo) and `ginpingo`'s `doc.go` (https://pkg.go.dev/github.com/hochfrequenz/btpingo/ginpingo).
5. **Validate at the Gin / huma boundary.** Request struct with `binding:"required,..."` tags, never raw `c.Request.Body` or `json.RawMessage` to `CallOnPremise`. See README §"Validate and sanitise at the Gin layer, not in SAP".
6. **Errors via the blessed envelope.**
   - gin handlers: `ginpingo.AbortError(c, status, code, userMessage, underlyingErr)`. Codes live in `btpingo`'s `httperr.go`.
   - huma handlers: `huma.Error5xx*("user-safe message")`. The classifier `btpingo.ClassifyOnPremError(err)` returns the right kind+detail for the err path; `btpingo.OnPremNon2xxDetail(status)` for the non-2xx-from-SAP path.
7. **Destination name comes from config**, not a fresh literal. The constant `destinationName = "..."` in your handler is rewritten by `apply-config` from `examples.destination_name` in `config.yml`. Don't hardcode a different literal — `apply-config` won't catch it.
8. **Tests use a one-method fake.**
   - gin: copy `examples/invoicesync/handler_test.go`'s `fakeOnPrem`.
   - huma: copy `examples/adtdiscovery/handler_test.go`'s `fakeCaller`.
   - mutating: copy `examples/adtcheckrun/handler_test.go`'s `fakeMutator`.
     The CSRF dance is the Service's concern, fully tested in `btpingo`'s `service_csrf_test.go`. Handler tests do not stub it.
9. **Add a `// FORK:` comment** at the destination-name constant (mirroring `examples/adtdiscovery/handler.go:107`) if your handler is intended as a fork crib-sheet.

## Forbidden patterns (CI gates these where marked)

- `c.JSON(..., gin.H{"error": ...})` — leaks `err.Error()` into the response. Use `ginpingo.AbortError`. See README §"Return errors with a stable envelope".
- `*btpingo.Service` outside `cmd/server/main.go` — **gated** by `template-guards.yml`'s "Handlers must depend on btpingo interfaces" step.
- `slog.Warn(...)` / any `.Warn(` log call — **gated** by `template-guards.yml`'s "No `.Warn(` log calls" step. Either return an error (then the boundary logs it once) or log INFO/DEBUG. See README §"Logging — two levels, no warnings".
- Raw `c.Request.Body` to `CallOnPremise` — turns the handler into a transparent proxy with the technical-user authority. `ginpingo.ProxyHandler` is the one place this exists, gated behind `ginpingo.RequireScope`.
- A new destination-name string literal in `examples/**/*.go` outside the rewriter's scope — `apply-config` will not catch it on the fork's run, every endpoint will 502.
- `redirect-uris` non-empty in `xs-security.json` — **gated** by `template-guards.yml`'s "xs-security.json redirect-uris must stay empty" step. Edit-update-restore is per-deploy only; never commit. See README §5a.
- A token-checking middleware that enforces `iss` against `xsuaa.URL` — XSUAA emits a SAP-internal `iss` literal (`http://<zone>.localhost:8080/uaa/oauth/token`), not a public URL. Issuer is intentionally not enforced; signature + audience + expiry are. See `btpingo`'s `auth.go`.

## Project structure (where to put things)

| Path                                | Role                                                | Notes                                                                                                                                                                                                                                  |
| ----------------------------------- | --------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `cmd/server/main.go`                | wiring, route registration, JWT validator setup     | Construction site for `*btpingo.Service`. The only place `*btpingo.Service` is allowed.                                                                                                                                                |
| `cmd/apply-config/`                 | fork-time string rewriter                           | Only rewrites this repo's own module path; does not touch `btpingo`. Adds new walkers when a new template-coupled literal lands.                                                                                                       |
| `github.com/hochfrequenz/btpingo`   | library-intent surface (external module)            | What handlers may depend on is listed at [pkg.go.dev/.../btpingo](https://pkg.go.dev/github.com/hochfrequenz/btpingo) (`doc.go`) and [.../ginpingo](https://pkg.go.dev/github.com/hochfrequenz/btpingo/ginpingo) for the Gin adapters. |
| `examples/`                         | handler crib-sheets                                 | Each example is self-contained: `handler.go` + `handler_test.go`. Tests use one-method fakes.                                                                                                                                          |
| `web/`                              | SAP approuter (Node.js)                             | Only `xs-app.json` and `package.json` typically need editing. On by default but optional — see `web/README.md` / README §"Do you need the approuter?" before assuming a fork needs it.                                                 |
| `docs/btp-deploy-walkthrough.de.md` | chronological deploy diary                          | German + HF-flavoured. Has a "Since-section" at the bottom; new substantive PRs land an entry there.                                                                                                                                   |
| `docs/abap-backend-playbook.md`     | building your own ABAP HTTP API for the Go app      | Read before writing the ABAP half: conventions both halves share, SAP pitfalls, copyable ABAP code.                                                                                                                                    |
| `config.yml`                        | single source of truth for fork-customisable values | Adding a new value → also add a rewriter in `cmd/apply-config/` and a config field.                                                                                                                                                    |

## When stuck

- **Handler authoring** → README §"Adding your service — the 80 % case".
- **Deploy** → README §Deployment, then `docs/btp-deploy-walkthrough.de.md` §"Seit dem ersten Deploy gelandete Follow-ups" for what's changed since the chronological diary was written.
- **Your own ABAP HTTP API behind the Go app** → `docs/abap-backend-playbook.md` (conventions, SAP pitfalls symptom-first, reference ABAP code).
- **Library surface** → `btpingo`'s `doc.go` / [pkg.go.dev/github.com/hochfrequenz/btpingo](https://pkg.go.dev/github.com/hochfrequenz/btpingo) (and `ginpingo`'s for the Gin adapters). If your fix needs an unexported identifier there, propose adding it upstream in `btpingo`, not by re-forking `internal/btp`.
- **CI gate fired** → read the gate's error message in `template-guards.yml`. The gate names the exact fix.
- **Test patterns** → the fakes in `examples/*/handler_test.go` are the canonical references.
