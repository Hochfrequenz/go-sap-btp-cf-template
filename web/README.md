# web/ — SAP approuter

This is the [`@sap/approuter`](https://www.npmjs.com/package/@sap/approuter) (see `package.json`
for the pinned version) deployed as the `((backend-host))-web` app in `manifest.yml`. It's the
browser-facing front door in front of the Go backend.

`xs-app.json` is strict JSON and can't hold comments, so this file carries the explanation instead.

## What it does

- Runs the XSUAA OAuth auth-code login flow for a browser user and keeps a session (cookie
  `JSESSIONID`); the Go backend never has to implement a login screen.
- Forwards the resulting JWT to the Go backend as `Authorization: Bearer <jwt>`, via the
  `GoBackend` destination declared in `manifest.yml` (`forwardAuthToken: true`).
- Routes `/api/*`, `/healthz`, `/version` from `xs-app.json` to that destination. `/healthz` and
  `/version` are `authenticationType: "none"`; `/api/*` requires `authenticationType: "xsuaa"`.
- CSRF protection is the approuter's own default for a route (`csrfProtection: true` unless set),
  but this template explicitly sets `csrfProtection: false` on `/api/*` — the Go backend does its
  own CSRF fetch/attach/retry for on-prem writes (see the root README's "Calling SAP with a POST /
  CSRF" section), so the approuter's variant would be redundant there.

## It's optional

The approuter ships and deploys by default and there is no config switch to turn it off, but
nothing about the Go backend requires it — it validates the JWT itself regardless of who sent the
request. See the root [README, "Do you need the approuter?"](../README.md#do-you-need-the-approuter)
for the full trade-off (what you keep, what you lose, and what removing it takes).

Short version: keep it if your consumers are browser users going through the XSUAA login flow;
drop it if they're machine clients that already hold a `client_credentials` bearer token and can
call `https://((backend-host)).((domain))/api/...` directly.
