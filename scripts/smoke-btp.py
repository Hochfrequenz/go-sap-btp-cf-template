#!/usr/bin/env python3
"""Authenticated smoke test of a deployed app on SAP BTP Cloud Foundry.

!!! RUN THIS ONCE PER DEPLOY. NEVER IN A LOOP, NEVER FROM CRON OR A WATCH COMMAND. !!!

SAP USER LOCKOUT WARNING: with --with-sap, the call to /api/adt-discovery reaches the SAP system.
The Go service then logs on with the technical user stored in the BTP destination. With a wrong or
expired destination password, ONE such call can cost TWO failed SAP logons: btpingo re-reads the
destination and retries once on a 401 (btpingo v0.5.0 service.go). SAP locks the user after a few
failed logons, which takes down every consumer of that user. The default run (without --with-sap)
never reaches SAP, but still keep it to one run: there is deliberately no retry logic. If a call
fails, fix the cause before running again.

What it does (one call per endpoint, no retries, nothing is changed):
  1. `cf app <app>` to derive the route (skipped with --host),
  2. GET /healthz                 (no auth),
  3. GET /version                 (no auth; checked against --expect-version if given),
  4. `cf env <app>`: extracts the XSUAA credentials in memory only, then fetches one
     client_credentials token,
  5. GET /api/me                  (JWT-guarded, answered by the Go service itself, does NOT reach SAP),
  6. GET /api/adt-discovery       (JWT-guarded, REACHES SAP; only with --with-sap),
  and exits non-zero at the first failure.

Secrets (client id and secret, the token) are never printed or stored. Do not redirect or paste
`cf env` output elsewhere; this script only holds it in memory.

Usage:
  python3 scripts/smoke-btp.py
  python3 scripts/smoke-btp.py --app my-app --expect-version v1.2.3
  python3 scripts/smoke-btp.py --with-sap          # also the one call that reaches SAP
"""

import argparse
import base64
import json
import re
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request

# Rewritten by `go run ./cmd/apply-config` from app.name in config.yml. Keep this line shape.
DEFAULT_APP = "go-btp-mwe"

TIMEOUT = 60


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


_OPENER = urllib.request.build_opener(_NoRedirect)


def run_cf(*args):
    try:
        return subprocess.run(["cf", *args], capture_output=True, text=True, check=True, timeout=60).stdout
    except FileNotFoundError:
        sys.exit("cf CLI not found in PATH")
    except subprocess.TimeoutExpired:
        sys.exit(f"`cf {args[0]}` timed out after 60s; check your network and `cf login`")
    except subprocess.CalledProcessError as e:
        sys.exit(f"`cf {args[0]}` failed (exit {e.returncode}); are you logged in and targeting the right space?")


def xsuaa_credentials(app):
    """Returns the credentials dict of the xsuaa binding from `cf env`; kept in memory only."""
    out = run_cf("env", app)
    marker = out.find("VCAP_SERVICES")
    start = out.find("{", marker if marker >= 0 else out.find("System-Provided:"))
    if start < 0:
        sys.exit("no VCAP_SERVICES found in `cf env` output")
    vcap, _ = json.JSONDecoder().raw_decode(out[start:])
    # `cf env` prints either {"VCAP_SERVICES": {...}} (older CLIs) or `VCAP_SERVICES: {...}` (CLI 8)
    services = vcap.get("VCAP_SERVICES", vcap)
    bindings = services.get("xsuaa", [])
    if not bindings:
        sys.exit(f"app {app} has no xsuaa binding")
    return bindings[0]["credentials"]


def default_host(app):
    out = run_cf("app", app)
    m = re.search(r"^routes:[ \t]+(\S+?)(?:,|\s|$)", out, re.MULTILINE)
    if not m:
        sys.exit(f"cannot derive the route from `cf app {app}`; pass --host")
    return m.group(1)


def http(url, headers=None, data=None):
    """One request, no retries. Returns (status, body bytes). Redirects are not followed, so the
    Authorization header never leaves the host."""
    method = "POST" if data is not None else "GET"
    req = urllib.request.Request(url, data=data, headers=headers or {}, method=method)
    path = urllib.parse.urlsplit(url).path
    try:
        with _OPENER.open(req, timeout=TIMEOUT) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except urllib.error.URLError as e:
        sys.exit(f"{method} {path}: connection failed: {e.reason}")
    except (TimeoutError, OSError) as e:
        sys.exit(f"{method} {path}: {type(e).__name__}")


def get_token(cred):
    if "clientsecret" not in cred:
        sys.exit("the xsuaa binding has no clientsecret (x509 binding?); this script supports client-secret bindings only")
    basic = base64.b64encode(f"{cred['clientid']}:{cred['clientsecret']}".encode()).decode()
    status, body = http(
        cred["url"].rstrip("/") + "/oauth/token",
        {"Authorization": f"Basic {basic}", "Content-Type": "application/x-www-form-urlencoded"},
        b"grant_type=client_credentials",
    )
    if status != 200:
        sys.exit(f"token request failed with {status}")
    try:
        return json.loads(body)["access_token"]
    except (ValueError, KeyError, TypeError):
        sys.exit("token response is not valid (no access_token)")


def main():
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    p.add_argument("--app", default=DEFAULT_APP, help=f"CF app name (default: {DEFAULT_APP})")
    p.add_argument("--host", help="route host of the app (default: first route of `cf app`)")
    p.add_argument("--expect-version", help="the /version `version` must equal this (e.g. the release tag)")
    p.add_argument("--with-sap", action="store_true",
                   help="also call GET /api/adt-discovery, which REACHES SAP and logs on with the technical user")
    a = p.parse_args()

    base = f"https://{a.host or default_host(a.app)}"

    def get(path, headers=None, sap=False):
        status, body = http(base + path, headers)
        print(f"GET {path} -> {status}" + ("  [REACHES SAP]" if sap else ""))
        if status != 200:
            print(f"FAIL: expected 200, body: {body[:300].decode(errors='replace')}")
            sys.exit(1)
        return body

    body = get("/healthz")
    if body.strip() != b"ok":
        sys.exit("FAIL: /healthz body is not 'ok'")

    try:
        info = json.loads(get("/version"))
    except ValueError:
        sys.exit("FAIL: /version did not return JSON")
    print(f"  version={info.get('version')} commit={info.get('commit')} branch={info.get('branch')}")
    if info.get("commit") in (None, "", "unknown"):
        sys.exit("FAIL: /version commit is unknown (ldflags not injected)")
    if a.expect_version and info.get("version") != a.expect_version:
        sys.exit(f"FAIL: /version is {info.get('version')!r}, expected {a.expect_version!r}")

    token = get_token(xsuaa_credentials(a.app))
    print("token: obtained")
    auth = {"Authorization": f"Bearer {token}"}

    try:
        claims = json.loads(get("/api/me", auth)).get("claims")
    except ValueError:
        sys.exit("FAIL: /api/me did not return JSON")
    if not isinstance(claims, dict):
        sys.exit("FAIL: /api/me has no claims object")
    print("  claims object present (JWT accepted)")

    if a.with_sap:
        try:
            disc = json.loads(get("/api/adt-discovery", auth, sap=True))
        except ValueError:
            sys.exit("FAIL: /api/adt-discovery did not return JSON")
        print(f"  workspaces: {len(disc.get('workspaces') or [])}")
    else:
        print("skipped /api/adt-discovery (reaches SAP; pass --with-sap, once)")

    print("RESULT: ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
