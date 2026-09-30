# web/ — SAP approuter

This is the [`@sap/approuter`](https://www.npmjs.com/package/@sap/approuter) (see `package.json`
for the version range) deployed as the `((backend-host))-web` app in `manifest.yml`. It's the
browser-facing front door in front of the Go backend.

`xs-app.json` is strict JSON and can't hold comments, so this file carries the explanation instead.

## What it does

- Runs the XSUAA OAuth auth-code login flow for a browser user and keeps a session (cookie
  `JSESSIONID`); the Go backend never has to implement a login screen.
- Forwards the resulting JWT to the Go backend as `Authorization: Bearer <jwt>`, via the
  `GoBackend` destination declared in `manifest.yml` (`forwardAuthToken: true`).
- Routes `/api/*`, `/healthz`, `/version` from `xs-app.json` to that destination. `/healthz` and
  `/version` are `authenticationType: "none"`; `/api/*` requires `authenticationType: "xsuaa"`.
- `csrfProtection` defaults to `true` per route; `xs-app.json` currently sets it `false` on
  `/api/*`. The backend's CSRF handling for on-prem writes is its outbound handshake with SAP and
  is unrelated to this setting.

## It's optional

On by default, no config switch. Whether a fork needs it, and how to remove it: root
[README, "Do you need the approuter?"](../README.md#do-you-need-the-approuter).
