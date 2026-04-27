# go-sap-btp-cf-template

A starter template for Go webservices on SAP BTP Cloud Foundry, aimed at a middle ground SAP-side and Go-side examples we found tend to skip individually. The Go examples on the public web we surveyed tend to skip the SAP-side ceremony (XSUAA token quirks, Cloud Connector's three-leg dance, CSRF on writes); the SAP examples we surveyed tend to skip the Go-side engineering (typed errors, testable handlers, CI gates, two-levels logging). This tries to cover both.

Once the BTP prerequisites are in place (see [Deployment](#deployment) — XSUAA / Destination / Connectivity service instances, Cloud Connector paired and exposing a virtual host, a destination pointing at it): fork, `go run ./cmd/apply-config` to rewrite module path + app name + CF coordinates from a single `config.yml`, build `./bin/server`, `cf push`.

**What it is, concretely:**

- runs on **SAP BTP Cloud Foundry** (two apps: Go backend + SAP approuter),
- authenticates users via **XSUAA** (JWKS-pinned JWT validation: RS256 signature, audience, expiry — see `internal/btp/auth.go` for why issuer is intentionally not checked),
- calls an **on-premise SAP system** through the **Connectivity + Destination** services (the Cloud Connector three-leg dance), with a **transparent CSRF handshake** on writes — one call on your side, full fetch → attach → retry on ours (the mutating call buffers its request body up-front so the retry can replay it),
- is built on **Gin** with a `DestinationAuthenticator` registry designed so **Principal Propagation** / SSO / OAuth2 client-credentials can be added as plug-in authenticators without touching the three-leg dance itself. The template ships `NoAuthentication` and `BasicAuthentication`; Principal Propagation specifically plugs in cleanly because the inbound JWT is already stashed under `btp.ForwardedUserTokenKey{}`. Swapping the user-auth layer to Auth0 is a different exercise — it replaces the JWT validator wiring in `cmd/server/main.go`, not a destination authenticator.

**The template ships two typed demo endpoints, not a transparent proxy.** Strict typing at the Gin boundary is only consistent with a finite, fixed-path endpoint set — a `/api/sap/:destination/*path` passthrough is by definition untyped (any body, any response). The two shipped routes show the constrained-proxy pattern instead:

- `GET /api/adt-discovery` — reads SAP's ADT service document, parses the ATOM XML, emits a typed JSON view.
- `POST /api/adt-checkrun` — takes a typed JSON request, builds the SAP-side XML internally, runs an ATC syntax check through the CSRF handshake, parses SAP's XML reply, emits typed JSON.

Both have the destination name and SAP path hard-coded at route registration, validator tags on the request struct, and handler tests that use a one-method fake. The `svc.ProxyHandler` method on `*btp.Service` is still available for forks that explicitly want to wire a transparent proxy — but they're expected to gate it behind `btp.RequireScope("...User")` and accept that typed validation ends at that route. The template's deployed showcase does not expose it.

What the template abstracts for you: XSUAA client-creds for both the Destination service and the Connectivity service, destination lookup, the proxied on-prem call through the Cloud Connector, Basic auth on the SAP side, CSRF fetch/attach/retry for writes. What it does NOT abstract: destination-service caching, destination-secret rotation, Cloud Connector failover, XSUAA zone/tenant switching — follow-ups for when they become the bottleneck.

> **Signpost.** First time here?
> - Building a new endpoint on top of an existing fork → [Adding your service](#adding-your-service--the-80--case).
> - Just forked this repo and need to configure it → [Using this repo as a template](#using-this-repo-as-a-template).
> - Deploying for the first time → [Deployment](#deployment).
> - Wondering what the three-leg dance actually looks like on the wire → [How it works under the hood](#how-it-works-under-the-hood).

## Table of contents

1. [Repository layout](#repository-layout)
2. [Using this repo as a template](#using-this-repo-as-a-template)
3. [Adding your service — the 80 % case](#adding-your-service--the-80--case)
4. [Deployment](#deployment)
5. [Continuous deployment](#continuous-deployment)
6. [Local development](#local-development)
7. [Extension points](#extension-points)
8. [What this MWE deliberately does *not* do](#what-this-mwe-deliberately-does-not-do)
9. [How it works under the hood](#how-it-works-under-the-hood)
10. [References](#references)

## Repository layout

```
cmd/server/main.go          Gin entry point; graceful shutdown; structured logs
internal/btp/
  env.go                    typed VCAP_SERVICES parsing + eager validation
  tokens.go                 XSUAA client-credentials fetcher (TTL cache, singleflight)
  destination.go            Destination-service lookup + typed AuthType/ProxyType
  proxy.go                  http.RoundTripper tunnelling via Connectivity proxy
  auth.go                   XSUAA JWT middleware (signature, aud, exp; see doc)
  authenticator.go          pluggable DestinationAuthenticator registry
  doc.go                    library-intent surface list (what forks may depend on)
  httperr.go                typed error envelope + AbortError helper
  middleware.go             RequestID + RequireScope helpers
  service.go                orchestrates the three-leg call + CSRF handshake for writes
web/                        SAP approuter
  package.json              pulls @sap/approuter
  xs-app.json               routes /api/* to the Go backend destination
manifest.yml                two CF apps + service bindings (uses manifest vars)
xs-security.json            XSUAA app config
Procfile, .cfignore
vars.example.yml            template for cf push --vars-file vars.yml
```

## Using this repo as a template

All per-deployment string values (app name, Go module path, CF subaccount coordinates, service instance names) live in a single `config.yml` at the repo root. `cmd/apply-config` is a small Go tool that reads that file, type-checks every field, and rewrites the rest of the tree from it — `go.mod`, every Go import, `manifest.yml`, `xs-security.json`, `vars.example.yml`, `web/package.json`, and `.github/workflows/deploy.yml`.

Recommended flow after forking:

```sh
gh repo create my-org/my-app --template Hochfrequenz/go-sap-btp-cf-template
cd my-app
$EDITOR config.yml                            # adjust app.name, app.module, cf.* etc.

go run ./cmd/apply-config --dry-run           # preview every planned change
go run ./cmd/apply-config                     # apply to the tree
go test ./...                                 # sanity check
git add -A && git commit -m "chore: configure fork"
```

Properties of the tool:

- **Typed.** `config.yml` is parsed into a Go struct with aggregated validation — all problems reported in one run, same pattern as `internal/btp/env.go`.
- **Idempotent.** Running twice with an unchanged `config.yml` is a no-op. CI can enforce that with `go run ./cmd/apply-config --check` (exit 1 if the tree drifts from the config).
- **Fails loudly.** Each file rewriter asserts its expected shape; a drifted target returns a clear error instead of silently producing garbage.
- **Zero internal dependencies.** The tool imports nothing from `internal/`, so it keeps working even while it's rewriting import paths.

`README.md` and `docs/btp-deploy-walkthrough.de.md` are **not** rewritten by the tool — they describe the original Hochfrequenz deployment and are meant to read as HF-flavoured prose. Strip or replace them in your fork as you see fit.

## Adding your service — the 80 % case

Building a new value-adding service on top of on-prem SAP is **two anchors**: an ABAP endpoint on the SAP side, and a Gin handler on the Go side. The BTP plumbing between them (XSUAA login, Destination lookup, Cloud Connector tunnel, Basic Auth forwarding) is already wired up and you do not need to touch it.

```mermaid
flowchart LR
    User([BTP user]) --> AR[Approuter]
    AR --> Go["<b>Anchor 2: Go</b><br/>your Gin handler in<br/><code>cmd/server/main.go</code>"]
    Go -->|svc.CallOnPremise| BTP["BTP plumbing<br/><i>already wired</i><br/>XSUAA · Destination · CC"]
    BTP -->|HTTPS + Basic Auth| SAP["<b>Anchor 1: SAP</b><br/>your ABAP endpoint<br/><code>/sap/bc/rest/zmy_service</code>"]

    style Go fill:#d1e7dd,stroke:#0f5132,stroke-width:2px,color:#0f5132
    style SAP fill:#fff3cd,stroke:#664d03,stroke-width:2px,color:#664d03
    style BTP fill:#e9ecef,stroke:#6c757d,stroke-dasharray:5 5,color:#495057
```

Green and yellow boxes are where you work; the dashed grey box is plumbing that comes with the template.

---

### Anchor 1 — in the SAP system: the ABAP endpoint

Your ABAP developer (or you, with a pair of hands on SE80 / ADT) builds the endpoint. Anything reachable by an ICF service node works — pick the framework that fits the job:

| ABAP framework | URL prefix | Good for |
| --- | --- | --- |
| RESTful BSP | `/sap/bc/rest/...` | quick custom JSON/XML endpoints |
| ADT | `/sap/bc/adt/...` | metadata, source, and development-tool operations |
| RAP service | `/sap/opu/odata4/sap/...` | new-style OData with transactional behaviour |
| Legacy SOAP | `/sap/bc/soap/...` | existing SOAP web services |

Three things to pin down in SAP terms before you write Go code:

| What | Example | Where it's used |
| --- | --- | --- |
| URL path | `/sap/bc/rest/zmy_invoice_sync` | goes into `svc.CallOnPremise`'s `path` arg |
| Authentication | Basic Auth (default) | stored on the BTP Destination, not in the Go code |
| SAP authorization objects | `S_DEVELOP`, `S_ICF`, domain-specific roles | granted to the technical user, not the BTP user |

> **One-time cockpit chore** (covered in section 5c of "Post-deploy manual steps" below): create a **Destination** in the BTP subaccount pointing at that SAP system's virtual host on the Cloud Connector, with the technical user's credentials. From then on, every Go handler reaches the SAP side through that Destination by name.

---

### Anchor 2 — in this repo: the Gin handler

A handler is four mechanical steps:

1. **Register the route** in `cmd/server/main.go`'s `buildRouter`. Anything on the `api` group is already behind XSUAA JWT validation.
2. **(optional) Read the authenticated user** from the validated JWT claims the middleware dropped into the Gin context.
3. **Call on-prem** via `svc.CallOnPremise` — one function call runs the whole three-leg dance.
4. **Shape the response** back to the caller (stream, transform, wrap, whatever your product wants).

**Step 1 — register the route.**

```go
// In buildRouter, next to api.GET("/me", ...):
api.POST("/invoice-sync", invoiceSyncHandler(svc))
```

**Steps 2–4 — the handler.** The full, typed, compileable example lives at [**`examples/invoicesync/handler.go`**](examples/invoicesync/handler.go) — read that file for the complete pattern (request type with validation tags, `svc.CallOnPremise` call, response streaming).

Here's a mental model-size sketch of what the file contains:

```go
type Request struct {
    CompanyCode string    `json:"company_code" binding:"required,len=4,uppercase"`
    AmountCents int64     `json:"amount_cents" binding:"required,min=1"`
    // ... more fields with validation tags
}

func Handler(svc btp.OnPremCaller) gin.HandlerFunc {
    return func(c *gin.Context) {
        var req Request
        if err := c.ShouldBindJSON(&req); err != nil {            // step 2: validate
            btp.AbortError(c, http.StatusBadRequest, btp.CodeInvalidRequest,
                err.Error(), nil)                                  //   validator messages are safe to surface
            return
        }
        claims := c.MustGet("jwtClaims").(jwt.MapClaims)          // step 3: know the caller
        _ = claims["user_name"]

        body, _ := json.Marshal(toABAPPayload(req))
        resp, err := svc.CallOnPremise(                            // step 4: one call, full chain
            c.Request.Context(), "HF_S4", http.MethodPost,
            "/sap/bc/rest/zmy_invoice_sync",
            http.Header{"Content-Type": {"application/json"}},
            bytes.NewReader(body),
        )
        if err != nil {
            btp.AbortError(c, http.StatusBadGateway, btp.CodeUpstreamUnreachable,
                "on-premise call failed", err)                     //   err goes to the log, not the response body
            return
        }
        defer resp.Body.Close()

        c.DataFromReader(resp.StatusCode, resp.ContentLength,      // step 5: shape response
            resp.Header.Get("Content-Type"), resp.Body, nil)
    }
}
```

The template does **not** ship a transparent-proxy route by default — strict typing at the Gin boundary needs a fixed endpoint set, and the security story is much better when every path is explicit. If a fork genuinely wants a catch-all pass-through, `svc.ProxyHandler` is still a method on `*btp.Service`; wire it yourself, gate it with `btp.RequireScope("...User")`, and be deliberate about which users can reach it. **For anything that writes state on the SAP side, read the next sub-section first** — validation-before-SAP is how you keep on-prem Short Dumps out of your life.

Unit-test the handler with the fixtures in `internal/btp/service_test.go`; they stand up stubs that respond like the real XSUAA / Destination / CC stack, so you can assert request shape and response translation without deploying.

---

### Validate and sanitise at the Gin layer, not in SAP

Sanitising a bad payload in ABAP is painful — data-type shaping is verbose, exception handling is heavy, and you cannot easily test an error path without a full transport-request round-trip. The same sanitising in Go is a struct tag and one line. Put the discipline as far to the left as possible.

> **Rule of thumb.** Every byte that reaches `svc.CallOnPremise` has already passed type checks, required-field checks, format and range checks, and enum-value checks in the Gin handler. If a request can fail validation, it fails here — with a `400` and a clear message — not on the SAP side with a Short Dump.

The request type in [`examples/invoicesync/handler.go`](examples/invoicesync/handler.go) is what a validated body looks like in practice — struct tags do the heavy lifting:

```go
type Request struct {
    CompanyCode string    `json:"company_code" binding:"required,len=4,uppercase"`
    PostingDate time.Time `json:"posting_date" binding:"required"`
    AmountCents int64     `json:"amount_cents" binding:"required,min=1"`
    Currency    string    `json:"currency"     binding:"required,oneof=EUR USD GBP"`
    Reference   string    `json:"reference"    binding:"max=16"`
}
```

Gin's binding uses [`go-playground/validator`](https://github.com/go-playground/validator); the tag vocabulary covers required / length / regex / enum / cross-field rules (`required_with`, `gtfield`, etc.). For shape-checks beyond the tag language, add a `Validate()` method on the request type and call it right after `ShouldBindJSON`.

> **Do not fool around with raw bytes.** The raw-forward pattern — reading `c.Request.Body` and piping it straight into `svc.CallOnPremise` — is what `svc.ProxyHandler` does and why the template does not wire that route by default (see previous sub-section). For every endpoint you write: unmarshal into a typed struct, validate via struct tags (or an explicit `Validate()`), marshal the ABAP-side shape yourself, and — if SAP returns XML — parse it back into Go structs and emit JSON, the way `examples/adtcheckrun/` and `examples/adtdiscovery/` do. `[]byte` and `json.RawMessage` that travel to `svc.CallOnPremise` unchecked are how SAP ends up with Short Dumps and how you end up debugging across three layers at 23:00.

Two things to apply the same discipline to, that are easy to forget:

- **Query and path parameters.** `c.ShouldBindQuery(&req)` and `c.ShouldBindUri(&req)` take the same struct-tag rules. Do not read raw `c.Query("amount")` and pass it on.
- **`interface{}` or `json.RawMessage` in a payload.** If the shape genuinely varies, wrap with a typed envelope that selects the variant, validate the envelope, then parse the raw field once the variant is known. An `interface{}` that travels to `svc.CallOnPremise` is a gift to SAP's Short-Dump generator.

---

### Return errors with a stable envelope

Raw `c.JSON(..., gin.H{"error": err.Error()})` is tempting and wrong — it leaks jwt/keyfunc internals, SAP response bodies, and stack-flavoured Go error text into the response the client reads.
The template ships one helper (`btp.AbortError`) and one envelope shape, and every handler in the repo uses them:

```go
btp.AbortError(c, http.StatusBadGateway, btp.CodeUpstreamUnreachable,
    "on-premise call failed", err)
// Response body is always:
// { "error": { "code": "upstream_unreachable", "message": "on-premise call failed", "request_id": "…" } }
```

The split: the **user-facing message** is what the client sees — keep it stable and generic, clients switch on `code`, not on `message`. The **underlying `err`** goes to `slog.ErrorContext` server-side with the status, code, and request ID so an operator can grep by request ID and see the real cause.

One exception where it's safe (and useful) to pass `err.Error()` as the user message: `go-playground/validator` errors from `c.ShouldBindJSON`. Those messages describe struct-tag violations that the client caused and needs to fix — surfacing them is the whole point.

Canonical codes live in [`internal/btp/httperr.go`](internal/btp/httperr.go) (`CodeInvalidRequest`, `CodeUnauthorized`, `CodeForbidden`, `CodeNotFound`, `CodeUpstreamUnreachable`, `CodeInternal`); declare your own `ErrorCode` constants if you need more.

---

### Guard routes with scopes and correlate logs with a request ID

The JWT middleware only checks signature, audience, and expiry — it does **not** enforce scopes, because this MWE ships no scope-gated route.
The moment you add one, use `btp.RequireScope(...)` rather than reading the `scope` claim by hand: the helper does exact-match (no `strings.Contains` trap where `Unauthorized-User` would match `User`) and produces the same 403 envelope as every other error in the repo.

```go
api := r.Group("/api")
api.Use(validator.Middleware())                                // authn: valid JWT
api.GET("/admin",
    btp.RequireScope("go-btp-mwe!t1234.Admin"),                // authz: exact-match qualified scope
    adminHandler)
```

> **XSUAA qualified-scope gotcha.** Real XSUAA tokens emit scopes as `<xsappname>!t<tenant>.<ScopeName>` (e.g. `go-btp-mwe!t1234.Admin`), NOT as a bare `"Admin"`. Pass the exact string you see in the token's `scope` claim — hit `/api/me` once after login and copy the shape from there. A bare scope name will 403 every request.

`btp.RequestID()` sits higher on the middleware chain (it's already wired in `cmd/server/main.go`) and serves two purposes at once:

- The `X-Request-Id` on the response echoes the inbound header or a generated ID, so a client retrying a flaky call can quote "my call with request-id `abc123` failed" and oncall greps straight to the right log line.
- The ID lands in the Gin context under `btp.RequestIDContextKey`. `btp.AbortError` picks it up automatically and the access log in `requestLog` emits it as a structured field, so every line related to one request shares one ID without any per-handler plumbing.

Two log layers, one rule each:

| Layer | Fields | What it is NOT |
| --- | --- | --- |
| Access log in `cmd/server/main.go` — one line per request | method, path (no query), status, duration, client IP, request_id | Never user identity, never claim values, never query string. |
| Handler-level `slog.InfoContext` (e.g. `invoicesync` emits the authenticated user for audit) | whatever the business event needs | Never a per-stage trace; keep to one line per *business event*, not per middleware. |

The split exists because putting `user_name` into the access log looks convenient until you put that user's email into a `?owner=…` query string and leak PII through the side channel. Keep access logs claim-free; put audit-worthy fields where the handler knows the context.

---

### Calling SAP with a POST — the CSRF case

SAP ICF nodes enforce an [X-CSRF-Token handshake](https://help.sap.com/docs/btp/sap-business-technology-platform/cross-site-request-forgery-protection-3ae1ed51.html) on every write (POST / PUT / DELETE / PATCH): you fetch a token with a GET first, attach it plus the session cookies SAP sets alongside it on the mutating call, and re-fetch once on `403 X-CSRF-Token: Required`. Getting that dance wrong is where most "my first ADT POST returns 403" reports come from.

The Service runs it for you via a second method:

```go
resp, err := svc.CallOnPremiseMutating(                   // not CallOnPremise
    c.Request.Context(), "HF_S4", http.MethodPost,
    "/sap/bc/adt/checkruns",
    http.Header{
        "Content-Type": {"application/vnd.sap.adt.checkobjects+xml"},
        "Accept":       {"application/vnd.sap.adt.checkmessages+xml"},
    },
    bytes.NewReader(body),
)
```

What `CallOnPremiseMutating` does behind the scenes:

1. **First call per destination** — GET the configured fetch path (default `/sap/bc/adt/discovery`; override with `btp.WithCSRFFetchPath` for non-ADT destinations) with `X-CSRF-Token: Fetch`. The returned token and any SAP session cookies (`SAP_SESSIONID_*`, `sap-usercontext`) are cached on the Service.
2. **This call and every subsequent mutating call** — reuse the cache, attach the token as `X-CSRF-Token` header and the cookies as a combined `Cookie` header.
3. **On 403 with `X-CSRF-Token: Required`** — the server-side session was recycled. Invalidate the cache, re-fetch once, retry the mutating call a single time. A 403 WITHOUT that header is a real authorization failure and surfaces to the caller unchanged — retrying wouldn't help.

The request body is buffered up-front so the retry can re-read it. For bodies too large to buffer, write your own handshake on top of `CallOnPremise`.

For handler tests, depend on the narrow `btp.OnPremMutator` interface (same shape as `OnPremCaller`, single method `CallOnPremiseMutating`) and substitute a one-method fake — the CSRF logic is the Service's concern, already tested in `internal/btp/service_csrf_test.go`. Handlers that mix reads and writes declare a composite interface at the usage site:

```go
type MyClient interface {
    btp.OnPremCaller
    btp.OnPremMutator
}
```

A complete example with a handler test lives in [`examples/adtcheckrun/`](examples/adtcheckrun/) — ADT syntax-check POST, typed request body with validator tags, and a `fakeMutator` test double.

---

### Logging — two levels, no warnings

The whole codebase runs on Dave Cheney's [two-levels discipline](https://dave.cheney.net/2015/11/05/lets-talk-about-logging). If you're coming from Java or ABAP, the rules are probably tighter than you're used to — deliberately.

1. **Only two levels matter.** `INFO` is useful operational output. `DEBUG` is useful to a developer chasing a specific problem and is off by default in production. `ERROR` exists only as an output-filter knob (`LOG_LEVEL=error` for low-noise deployments), not as a level you ever *write* to — errors are returned, not logged (see rule 2).
2. **Errors are not a log level.** An error is a return value. Handlers return it; only the boundary that cannot return any further (the HTTP response, or `main`) logs it. `btp.AbortError` is that boundary for HTTP responses — it writes the envelope and logs the underlying Go error once, server-side, with the request ID.
3. **`WARN` doesn't exist in this repo.** If it's serious, handle it as an error. If it isn't, log `INFO`. "Something odd happened but I'm going to continue" is where warnings accumulate that nobody ever acts on — don't write those.
4. **One access-log line per request.** Already wired in `cmd/server/main.go`'s `requestLog`; don't add "entering handler" / "leaving handler" lines on top. If a handler needs business-event context (e.g. `invoicesync` logs `user + company_code`), emit exactly one line per business event, not per middleware stage.

Enforcement: `.github/workflows/template-guards.yml` greps the tree for `.Warn(` and fails CI on any hit. A new PR that reintroduces warnings is blocked at merge; the error message points back to this section.

Local debugging: set `LOG_LEVEL=debug` before running the server to see `DEBUG`-level lines (e.g. client-disconnect details from `Service.ProxyHandler`). `INFO` is the production default; `ERROR` is available for low-noise deployments.

---

### Test your handler without touching SAP

> **Coming from ABAP?** Unit-testing in Go is nothing like testing in the SAP stack, and this is good news. A Go test compiles and runs in well under a second — no transport request round-trip, no test user to maintain, no colleague's session to lock, no data to clean up in table `BKPF` afterwards. The red-green-refactor loop that never really worked in ABAP works here because the feedback is cheap. If that sounds unfamiliar, the three tests in this sub-section are a good first encounter: copy them, break something in the handler, watch the test fail in 0.3 s, fix it, watch it pass. That's the whole loop.

Integration-testing against a real on-prem SAP is a pain: transport requests, user lockouts, Short Dumps on edge cases, an ABAP stack that boots in minutes. The template is designed so you almost never have to. Every handler depends on the narrow `btp.OnPremCaller` interface rather than the concrete `*btp.Service`:

```go
// internal/btp/service.go — the contract handlers should depend on:
type OnPremCaller interface {
    CallOnPremise(ctx context.Context, destName, method, pathSuffix string,
        headers http.Header, body io.Reader) (*http.Response, error)
}
```

The concrete `Service` type satisfies it in production. Tests substitute a one-method fake that records the request the handler produced and returns a canned response — no XSUAA, no Destination lookup, no Cloud Connector tunnel, no ABAP.

> **Library-intent surface.** The set of identifiers handlers are allowed to lean on lives at [`internal/btp/doc.go`](internal/btp/doc.go). Anything else in the package is template-internal and may move without notice. A CI gate blocks PRs that add a dependency on the concrete `Service` type from outside `cmd/server/main.go`.

The canonical test lives next to the example handler: [`examples/invoicesync/handler_test.go`](examples/invoicesync/handler_test.go). Its shape:

```go
type fakeOnPrem struct {
    gotDest, gotMethod, gotPath string
    gotBody                     []byte
    resp                        *http.Response
    err                         error
}

func (f *fakeOnPrem) CallOnPremise(_ context.Context, dest, method, path string,
    _ http.Header, body io.Reader) (*http.Response, error) {
    f.gotDest, f.gotMethod, f.gotPath = dest, method, path
    if body != nil { f.gotBody, _ = io.ReadAll(body) }
    return f.resp, f.err
}

func Test_Handler_MarshalsRequestIntoABAPShape(t *testing.T) {
    fake := &fakeOnPrem{resp: &http.Response{
        StatusCode: http.StatusOK,
        Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
    }}
    r := newRouter(fake) // see the test file for the gin setup helper

    req := httptest.NewRequest(http.MethodPost, "/invoice-sync",
        strings.NewReader(`{"company_code":"1000", ...}`))
    req.Header.Set("Content-Type", "application/json")
    w := httptest.NewRecorder()
    r.ServeHTTP(w, req)

    // Assert what the handler did with SAP.
    then.AssertThat(t, fake.gotDest,   is.EqualTo("HF_S4"))
    then.AssertThat(t, fake.gotPath,   is.EqualTo("/sap/bc/rest/zmy_invoice_sync"))
    // Decode fake.gotBody into your expected ABAP-side struct and assert field by field.
}
```

> **Stay typed all the way through.** The test decodes `fake.gotBody` into a **named struct** that describes the ABAP-side shape (BUKRS / WAERS / …) — not into `map[string]any`. Every place where JSON crosses a boundary in this template — caller → handler, handler → SAP, SAP response → caller — is asserted through typed Go. A failed field name or a wrong type fails at compile time or at a typed unmarshal, never silently as "oh, that key was missing from the map".

The full test file shows three cases worth copying into your own handler tests:

1. **Happy path** — valid payload, fake returns 200, handler translates to the expected typed ABAP shape and forwards.
2. **Validation fails before SAP** — an invalid payload must 400 **without ever calling `CallOnPremise`** (the test asserts the fake was not invoked).
3. **On-prem error surfaces as 502** — when the fake returns an error, the handler responds 502, not 500.

If you need to exercise the three-leg token dance end-to-end (destination lookup + XSUAA client_credentials + Connectivity token + Cloud Connector proxy), the heavier `httptest.NewServer` pattern is in [`internal/btp/service_test.go`](internal/btp/service_test.go). For everyday handler work the interface-plus-fake pattern above is faster and more targeted.

---

### What you do NOT need to understand

The middleware and service code hide most of the BTP specifics. For the 80 % case you can assume each of the following without looking:

| Concern | Handled by | Assume that |
| --- | --- | --- |
| XSUAA `aud` / `iss` claim shapes | `internal/btp/auth.go` | the caller is authenticated if your handler runs |
| The three-leg token dance (XSUAA → Dest → XSUAA → CC) | `internal/btp/service.go` | `svc.CallOnPremise` just works; one call, full round-trip |
| Which headers get forwarded | `skipForwardedHeader` in `service.go` | hop-by-hop + `Authorization` + `Cookie` + `Host` are stripped; everything else flows through |
| Path-traversal defence | `Service.CallOnPremise` | `..` in the `path` suffix is rejected before anything leaves the process |
| XSUAA token expiry mid-request | `tokens.go` + retry in `service.go` | a stale connectivity token on a body-less call self-heals once without a 500 |

If you do hit a wall, [How it works under the hood](#how-it-works-under-the-hood) near the bottom of this README has the full diagram and the per-layer invariants.

---

### When you need to look deeper

- **Your Destination uses Principal Propagation, not Basic Auth.** The approuter-forwarded user JWT is stashed in the request context under `btp.ForwardedUserTokenKey{}`; implement a `DestinationAuthenticator` that reads it and sets `SAP-Connectivity-Authentication`. See "Extension points" below.
- **Your on-prem endpoint needs CSRF tokens for writes** (most ADT writes do). Use `svc.CallOnPremiseMutating` — it runs the `X-CSRF-Token: Fetch` → attach-token-and-cookies → retry-once-on-403 dance transparently. See [Calling SAP with a POST — the CSRF case](#calling-sap-with-a-post--the-csrf-case) below.
- **One of the demo endpoints (`/api/adt-discovery`, `/api/adt-checkrun`) returns 502 or an unexpected 401.** See the failure-mode ladder under "Smoke tests" below.

## Deployment

### Prerequisites

Before any `cf push` there are **three things a human has to do** in addition to having a Cloud Foundry CLI session:

#### 1. A Cloud Connector exposing the on-prem endpoint

A SAP Cloud Connector must be installed on the on-prem network, paired with your BTP subaccount, and must expose the internal SAP virtual host/port you want to reach. See [Install the Cloud Connector](https://help.sap.com/docs/connectivity/sap-btp-connectivity-cf/installation) and [Configure Access Control](https://help.sap.com/docs/connectivity/sap-btp-connectivity-cf/configure-access-control-http). Without this, the three-leg dance succeeds up to the final hop and then times out against a host that cannot be reached.

If your subaccount has more than one Cloud Connector, note each one's **Location ID** — you will set it on the Destination below as `CloudConnectorLocationId`.

#### 2. Three service instances

Create the XSUAA, Destination, and Connectivity instances with these **exact names** (referenced by `manifest.yml`):

```sh
cf create-service xsuaa application go-xsuaa -c xs-security.json
cf create-service destination lite    go-dest
cf create-service connectivity lite   go-cc
```

`cf push` (step 4 below) binds them to the app automatically via `manifest.yml`'s `services:` section.

#### 3. (First deploy only) Why this repo ships with `binary_buildpack`

This template deploys via the CF **`binary_buildpack`**: you cross-compile a static Linux/amd64 binary locally (or in CI), push it with the rest of the app, and the CF stager runs it as-is.
That means the stager never invokes the Go toolchain, never fetches deps, never runs `go build`.

Why not `go_buildpack`? On `eu10` as of April 2026 the classic `go_buildpack` (cflinuxfs4 v1.10.44) ships **Go 1.23.12**; this repo's `go.mod` declares `go 1.26`, so Go's auto-toolchain kicks in on the stager, tries to fetch Go 1.26 plus the full dep tree, and the stager OOMs under the org-level memory quota.
`binary_buildpack` sidesteps all of that.

If your landscape doesn't have this problem (bigger stager memory, `paketo-buildpacks/go` pre-installed, an egress-restricted build container), you can switch back to classic Go staging. Replace the `buildpacks:` block in `manifest.yml` with one of:

```yaml
# Classic CF go_buildpack (if you've confirmed it stages cleanly)
buildpacks:
  - go_buildpack
env:
  GIN_MODE: release
  GO_INSTALL_PACKAGE_SPEC: ./cmd/server
```

```yaml
# Paketo (auto-discovers main via BP_GO_TARGETS)
buildpacks:
  - paketo-buildpacks/go
env:
  GIN_MODE: release
  BP_GO_TARGETS: ./cmd/server
```

Confirmed working on eu10 (AWS Frankfurt) via `binary_buildpack` + cross-compile; `go_buildpack` cflinuxfs4 v1.10.44 OOMs there. [See the walkthrough](docs/btp-deploy-walkthrough.de.md) (DE) for the full replay.

### Pre-flight gotchas

Compiled from the first real deploy on SAP's `eu10` landscape in April 2026.
Two conventions to keep in mind while reading this section:

- `<angle-brackets>` mark values you substitute for your own deploy — they are reusable patterns.
- `` `backticks on specific strings` `` mark the concrete values I used on Hochfrequenz's subaccount (e.g. `eu10`, `go-btp-mwe`, `go_buildpack cflinuxfs4 v1.10.44`), shown for illustration only — change them for yours.

1. **Org-level route quota, not space-level.**
   `cf routes` only lists the currently-targeted space, but the route quota is an **org-wide** limit.
   Before `cf push`, confirm at least **two** free route slots (one per app — backend + approuter):

   ```sh
   # Set this to the name shown under "org:" in 'cf target' (HF example: "HF Dev Account_hf-cf"):
   ORG="<your-cf-org-name>"

   ORG_GUID=$(cf org "$ORG" --guid)
   QUOTA_GUID=$(cf curl "/v3/organizations/$ORG_GUID" | jq -r .relationships.quota.data.guid)

   echo "Used:  $(cf curl "/v3/routes?per_page=100&organization_guids=$ORG_GUID" | jq .pagination.total_results)"
   echo "Quota: $(cf curl "/v3/organization_quotas/$QUOTA_GUID"                  | jq .routes.total_routes)"
   ```

   If you're within 2 of the quota, either free slots by removing a stopped app with its routes (`cf delete <app-name> -r -f` — note that `-r` can free *more* slots than `cf routes` in the current space suggested, because orphan routes from other spaces travel with the app), or ask a global-account admin to raise the quota.
   Symptom when ignored: `Routes quota exceeded for organization '<org>'`, raised before staging begins.

2. **You MUST cross-compile `./bin/server` before `cf push`.**
   The default buildpack here is `binary_buildpack` (see section 3 above for why), which expects the binary to exist in the pushed bundle.
   `make build-linux` (POSIX) or `scripts\build.ps1` (Windows PowerShell) produces it.
   A fresh clone without a build-step produces a stager error like `/home/vcap/app/bin/server: not found`.
   The CD workflow in `.github/workflows/deploy.yml` already does this; manual pushes need to remember the step.

3. **The cockpit steps in section 5b + 5c below require subaccount admin rights.**
   Creating a Role Collection and creating a Destination are both blocked for a plain Space Developer.
   A `cf push` will succeed and then stall on permissions when you try to click through the cockpit.
   Check with whoever administers your subaccount before starting.

4. **The cockpit URL pattern you may remember has changed in some regions.**
   The historical `https://cockpit.<region>.hana.ondemand.com/` pattern has been replaced — e.g. EMEA is now at `https://emea.cockpit.btp.cloud.sap/cockpit`.
   The stable portable entry is `https://account.hana.ondemand.com/`, which redirects to whichever regional cockpit your user belongs to.

5. **Windows-only: winget quirks for the CF CLI (skip if not on Windows).**
   The correct winget package ID is `CloudFoundry.CLI.v8` (or `.v7` for the previous major).
   `CloudFoundry.CloudFoundryCLI` — a plausible-looking ID — does **not** exist and `winget install` will refuse it.
   After `winget install`, close and reopen your terminal before running `cf`; already-open shells keep their old `PATH` and will report `cf: command not found` even though the binary is installed.

### 4. cf push

Two steps: build the binary, then push.

```sh
cp vars.example.yml vars.yml    # edit for your subaccount domain

# Build the static Linux binary the binary_buildpack will run:
make build-linux                # POSIX
# or on Windows PowerShell:
.\scripts\build.ps1

cf push -f manifest.yml --vars-file vars.yml
```

`vars.example.yml` contains:

```yaml
backend-host: go-btp-mwe
domain: cfapps.eu10.hana.ondemand.com
```

Both recipes wrap the same one-liner, in case you prefer to run it inline:

```sh
# POSIX:
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/server ./cmd/server
```

```powershell
# Windows PowerShell:
$env:CGO_ENABLED='0'; $env:GOOS='linux'; $env:GOARCH='amd64'
go build -o bin/server ./cmd/server
```

`manifest.yml` declares `binary_buildpack` + `command: ./bin/server` so a bare `cf push` works — no `-b` / `-c` flags needed. The CD workflow in `.github/workflows/deploy.yml` does the same thing, it just builds on the GitHub runner instead of your laptop.

### 5. Post-deploy manual steps (required for the app to work)

Even after a green `cf push`, three things still need to be done by a human in the BTP cockpit before requests succeed. Each is a one-time chore per deploy — expand the step you're on:

<details>
<summary>5a. Update XSUAA redirect URIs — makes OAuth login not bounce with "redirect URI mismatch"</summary>

The shipped `xs-security.json` has an empty `redirect-uris` array — we cannot know the approuter's URL until the first push. After deploy:

1. Find the approuter route: `cf app go-btp-mwe-web` (it prints the `routes:` — note the HTTPS URL).
2. Edit `xs-security.json` and add `"https://<approuter-host>.<domain>/**"` to `oauth2-configuration.redirect-uris`.
3. Push the updated security config: `cf update-service go-xsuaa -c xs-security.json`.
4. **Immediately run `git restore xs-security.json`** so your deploy-specific URL doesn't end up in a commit. `redirect-uris` lives inside XSUAA now; the file in the repo stays empty by convention, one edit per deploy, never committed.

Skipping this yields "redirect URI mismatch" on the first OAuth login.

</details>

<details>
<summary>5b. Create a Role Collection and assign it to yourself — only matters once you add a scope-gated route</summary>

`xs-security.json` defines a `User` role template. XSUAA issues tokens without scopes until a Role Collection containing that role is assigned to your BTP user.

1. BTP cockpit → your subaccount → Security → **Role Collections** → Create.
2. Name it e.g. `go-btp-mwe-users`, add the role template `User` under `go-btp-mwe`.
3. Security → **Users** → your user → add the new Role Collection.
4. If you were already logged in through the approuter, log out (`/logout`) and back in so the new token carries the scope.

The `/api/*` routes this MWE ships with do **not** enforce the `User` scope — the JWT middleware only validates signature, audience, and expiry, so a valid XSUAA user passes regardless of Role Collection. 5b matters the moment you add a scope-gated route (e.g. `c.MustGet("jwtClaims")` then checking `scope` contains `User`); without 5b, that route would 403 even though login succeeds.

</details>

<details>
<summary>5c. Create a Destination pointing at your on-prem system — this is what <code>svc.CallOnPremise</code> resolves through</summary>

BTP cockpit → subaccount → Connectivity → **Destinations** → New Destination:

| Field | Value |
| --- | --- |
| **Name** | `HfSap` (the destination name your handlers hard-code — e.g. `adtdiscovery` and `adtcheckrun` reference `HF_S4` — plus anything a fork adds) |
| **Type** | `HTTP` |
| **URL** | virtual host as exposed by the Cloud Connector (e.g. `http://hfsap.cc:8000`) |
| **Proxy Type** | `OnPremise` |
| **Authentication** | `BasicAuthentication` (or `NoAuthentication` for a smoke-test endpoint) |
| **User / Password** | the SAP account on the on-prem system |
| **Additional Properties** (optional) | `CloudConnectorLocationId` = `<your location ID>` if you have multiple CCs |

Once this exists (and a handler that references it by name is wired in `buildRouter` — `adtdiscovery` / `adtcheckrun` reference `HF_S4`), calls through that handler will work. Until the destination exists, the handler returns a typed `upstream_unreachable` 502 envelope.

</details>

### 6. Smoke tests

Three layered checks, running in order. If one fails, the earlier ones still isolate where in the chain things broke. Substitute `<approuter-host>` and `<domain>` for your deploy — typically `go-btp-mwe-web` and e.g. `cfapps.eu10.hana.ondemand.com`.

<details>
<summary>6a. <code>/healthz</code> — approuter reaches the Go backend (no auth)</summary>

```sh
curl -i https://<approuter-host>.<domain>/healthz
```

Expected: `HTTP/1.1 200 OK` with body `ok`. `/healthz` is explicitly marked `authenticationType: none` in `web/xs-app.json`, so this proves the approuter is up and forwarding without needing auth. If this fails, the approuter or backend didn't start — check `cf app <backend-host>` and `cf app <approuter-host>`.

</details>

<details>
<summary>6b. <code>/api/me</code> — full OAuth + JWT-validation chain (needs a browser)</summary>

Open in a browser — `curl` alone cannot complete the XSUAA SSO dance. Use whatever your OS opens HTTPS URLs with (`open` on macOS, `start` on Windows cmd, `xdg-open` on Linux), or paste the URL:

```
https://<approuter-host>.<domain>/api/me
```

Expected: a JSON body with a `claims` object — your `email`, `given_name`, `family_name`, `scope`, and `xs.system.attributes.xs.rolecollections`. This exercises the approuter's XSUAA auth-code flow, the Go backend's JWKS-pinned signature verification, and the `aud` / exp / leeway checks in `internal/btp/auth.go`.

A `401 invalid token: ... invalid audience` here points at section 7 of this deployment's tracking issue — XSUAA emits `aud` in the `sb-<xsappname>!t<tenant>` (`ClientID`) form, not bare `<xsappname>`.

</details>

<details>
<summary>6c. <code>/api/adt-discovery</code> — full three-leg call to on-prem SAP</summary>

Simplest: open the URL in the same browser you used for 6b — the approuter session cookie is already set, and the browser will render the JSON response. A compact `{"workspaces":[…]}` payload is the success signal.

If you want `curl` instead, export the approuter session cookie from your browser (DevTools → Application → Cookies → copy `JSESSIONID`, or use a "cookies.txt" browser extension) and pass it via `-b`:

```sh
curl -L -b "JSESSIONID=<value-from-devtools>" \
  https://<approuter-host>.<domain>/api/adt-discovery
```

Expected: `200 application/json` with a body starting `{"workspaces":[…]}`. That one call exercises the destination-service lookup, both XSUAA `client_credentials` fetches, the Cloud Connector proxy tunnel, and Basic Auth against the on-premise SAP system — in the three-leg sequence described in the architecture diagram at the bottom of this README. Internally, the handler fetches `/sap/bc/adt/discovery` from the destination (returned as XML), parses the ATOM service document, and emits the typed JSON view.

Why `/sap/bc/adt/discovery` as the probe: it's a standard ABAP Development Tools endpoint (used by [`Hochfrequenz/adtler`](https://github.com/Hochfrequenz/adtler) as the CSRF-preflight target), available on any ADT-enabled S/4 system, and reachable by any authenticated ADT developer user — so it rarely trips on fine-grained authorization. If your destination points at a non-S/4 system (for example a HANA XS-only host), change the hard-coded path in `examples/adtdiscovery/handler.go` to something your destination user has read access to.

</details>

<details>
<summary>Failure-mode ladder — what each error code usually means, in order of "more annoying to diagnose"</summary>

- `404` from the approuter: the backend isn't bound to the destination `GoBackend` that `web/xs-app.json` references, or the backend crashed. `cf logs <backend-host> --recent`.
- `401 invalid audience` or `... invalid issuer`: section 7. The JWT middleware expects a token shape XSUAA does not emit. Fix is in `internal/btp/auth.go`.
- `502` from the Go backend: the destination lookup succeeded but the on-premise call failed. Most common causes: the destination's virtual host is not exposed by the Cloud Connector, or the Cloud Connector is red.
- `403` with an SAP-branded body: the destination's stored user authenticated successfully but does not have authorization for the path you chose. Switch the path, not the destination.

</details>

## Continuous deployment

`.github/workflows/deploy.yml` deploys both apps on every **published GitHub Release** (not on push-to-main, not on plain tag pushes — only the explicit "Publish release" click). The workflow has two jobs:

1. **`gate`** — `go vet`, `go test ./... -race`, `golangci-lint`, `gofmt --diff`. All four must pass green.
2. **`deploy`** — only runs if `gate` was green. Cross-compiles a static Linux binary on the runner (same `make build-linux` a laptop would run), installs `cf` v8, pushes backend and approuter per `manifest.yml` (backend is already `binary_buildpack` with `command: ./bin/server`, approuter keeps `nodejs_buildpack`), then curls `/healthz` and fails the workflow if it isn't `200 ok`.

### Required repository secrets

| Secret | What it is |
| --- | --- |
| `CF_USER` | Email of a CF user with the `SpaceDeveloper` role on the deploy target space. See the caveat below — this usually needs to be a **technical user** created by a Subaccount Administrator, because SSO-only users cannot authenticate non-interactively. |
| `CF_PASSWORD` | That user's password. |

Set both at **Settings → Secrets and variables → Actions → New repository secret**. If either is missing, the deploy job fails loudly at `cf auth` instead of silently pushing with empty credentials.

### Caveat on CF identity

BTP subaccounts commonly enforce SAP ID Service SSO for human users, which means those users have **no usable password** for `cf login -u/-p`. The workflow needs a non-interactive credential pair, which usually means creating a dedicated CF user ("technical user") via **BTP cockpit → Security → Users → New** with a local password, then granting `SpaceDeveloper` on the target space via `cf set-space-role`. Creating the user requires Subaccount Administrator rights.

The alternative — `cf login --sso` with a temporary passcode — is interactive-only and not suitable for CI.

### What the manifest does vs. what CI does

The committed `manifest.yml` declares `buildpacks: [binary_buildpack]` with `command: ./bin/server`, so a manual laptop-side `cf push` and the GitHub runner's push do the same thing. Both paths build `./bin/server` first — the CD workflow on the runner, a developer via `make build-linux` / `scripts\build.ps1`. No `-b` / `-c` override flags, no code divergence between manual and automated deploys.

## Local development

**The server refuses to start without `VCAP_APPLICATION` and `VCAP_SERVICES`.**
That is deliberate: there is no meaningful "just run it" mode for a BTP-integrated app, and the alternative (stubbing each service) leads to code paths that only ever run on a developer laptop.

The recommended feedback loop is the unit test suite:

```sh
go test ./... -race
go test ./... -covermode=count -coverprofile=coverage.out
```

CI enforces 90% line coverage (`.github/workflows/coverage.yml`).

If you need to exercise Gin handlers against real (stub) BTP services locally, set the env explicitly — the required shape matches `internal/btp/env.go` struct tags.

<details>
<summary>VCAP env stub — expand to copy</summary>

```sh
export VCAP_APPLICATION='{"application_id":"x","application_name":"go-btp-mwe","space_name":"dev","uris":["localhost"]}'
export VCAP_SERVICES='{
  "xsuaa":[{"label":"xsuaa","name":"go-xsuaa","credentials":{
    "url":"https://your-stub","clientid":"cid","clientsecret":"csec",
    "xsappname":"go-btp-mwe","uaadomain":"your-stub","identityzone":"dev"}}],
  "destination":[{"label":"destination","name":"go-dest","credentials":{
    "uri":"https://your-dest-stub","clientid":"d","clientsecret":"ds","url":"https://your-stub"}}],
  "connectivity":[{"label":"connectivity","name":"go-cc","credentials":{
    "clientid":"c","clientsecret":"cs","url":"https://your-stub",
    "onpremise_proxy_host":"127.0.0.1","onpremise_proxy_port":"20003"}}]
}'
go run ./cmd/server
```

</details>

A broken `VCAP_SERVICES` payload produces a single error listing **all** problems at once — modeled on [Hochfrequenz/sap-mcp-config](https://github.com/Hochfrequenz/sap-mcp-config). No more "fix one field, redeploy, find the next missing field."

## Extension points

The primary extension surface is `btp.DestinationAuthenticator`:

```go
type DestinationAuthenticator interface {
    AuthType() AuthType
    Apply(ctx context.Context, req *http.Request, dest *Destination) error
}
```

Register more at startup without touching the call site:

```go
svc, _ := btp.NewService(env)
svc.Authenticators().Register(myAuth0Authenticator{})
svc.Authenticators().Register(myOAuth2ClientCredsAuthenticator{})
```

Shipped out of the box: `AuthNone` and `AuthBasic`, plus a rejecting fallback so unknown auth types fail loudly rather than travelling unauthenticated. The authenticator registry is where Auth0/SSO/`OAuth2ClientCredentials`/`PrincipalPropagation` plug in.

For Principal Propagation specifically: the approuter-forwarded user JWT is stashed in the request context under `btp.ForwardedUserTokenKey{}` — a PP authenticator reads it from there and sets `SAP-Connectivity-Authentication`.

## What this MWE deliberately does *not* do

- **No fake `Mozilla/5.0` User-Agent.** The PHP/Python reference impersonates a browser as a HF-SAP workaround; the SAP BTP spec has no such requirement.
- **No local-dev mock layer.** Stubbing VCAP is documented above; writing mocks into the code is where drift starts.
- **No destination-service caching.** Destinations change rarely but we re-fetch on every request for now; add a TTL cache once there is a reason to.

## How it works under the hood

You do not need this section to write a handler. It is here for when a deploy misbehaves, a token doesn't validate, or you want to understand what `svc.CallOnPremise` actually does on the wire.

Two CF applications share one XSUAA instance. The approuter is the browser-facing front door; the Go backend is the thing that actually talks to the on-premise SAP system. The Destination and Connectivity services are bound only to the backend.

```mermaid
sequenceDiagram
    autonumber
    participant Browser
    participant AR as web<br/>(SAP approuter)
    participant Go as go-btp-mwe<br/>(Gin)
    participant XSUAA
    participant Dest as Destination<br/>service
    participant Conn as Connectivity<br/>service
    participant CC as Cloud<br/>Connector
    participant SAP as On-premise<br/>SAP system

    Browser->>AR: GET /api/adt-discovery (or other typed route)
    AR->>XSUAA: OAuth auth-code flow (first time only)
    XSUAA-->>AR: session + JWT
    AR->>Go: forward request with Authorization: Bearer <jwt>
    Go->>XSUAA: fetch JWKS (cached by keyfunc, refreshed in background)
    XSUAA-->>Go: signing keys
    Note over Go: verify RS256 signature,<br/>audience, expiry (issuer<br/>intentionally not checked)

    Go->>XSUAA: client_credentials<br/>(destination service creds)
    XSUAA-->>Go: destination token
    Go->>Dest: GET /destination-configuration/v1/destinations/<destination>
    Dest-->>Go: {URL, Authentication, User, Password, CloudConnectorLocationId, …}

    Go->>XSUAA: client_credentials<br/>(connectivity service creds)
    XSUAA-->>Go: connectivity token

    Go->>CC: request via HTTP proxy<br/>Proxy-Authorization: Bearer <conn-token><br/>Authorization: <destination-auth><br/>SAP-Connectivity-SCC-Location_ID: <loc-id?>
    CC->>SAP: forward on the on-prem network
    SAP-->>CC: response
    CC-->>Go: response
    Go-->>AR: response
    AR-->>Browser: response
```

XSUAA client-credentials tokens are cached with a 30 s refresh leeway and collapsed via singleflight so a burst of concurrent requests does not hammer XSUAA. A `401` from the on-prem system on a GET-style (body-less) call invalidates the cached connectivity token and retries once, so mid-flight token expiry self-heals instead of bubbling up; a `403` is **not** retried (that means "authenticated but not authorized", which a fresh token cannot fix).

## References

- [SAP BTP Connectivity & Destination (Help Portal)](https://help.sap.com/docs/connectivity/sap-btp-connectivity-cf)
- [SAP Cloud Connector install guide](https://help.sap.com/docs/connectivity/sap-btp-connectivity-cf/installation)
- [Destination service REST API](https://help.sap.com/docs/connectivity/sap-btp-connectivity-cf/destinations-destination-service-rest-api) — `/destination-configuration/v1/destinations/{name}` is the generic lookup this MWE uses
- [SAP Cloud SDK (JS) — on-premise connectivity headers](https://sap.github.io/cloud-sdk/docs/js/features/connectivity/on-premise)
- [BTP-Python-Template](https://github.com/Hochfrequenz/BTP-Python-Template) — the Python analogue
- [sap-mcp-config](https://github.com/Hochfrequenz/sap-mcp-config) — source of the eager-validation pattern used in `env.go`
