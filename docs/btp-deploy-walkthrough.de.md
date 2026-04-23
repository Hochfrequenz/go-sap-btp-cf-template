# BTP-Deploy-Walkthrough (Schritt-für-Schritt, reproduzierbar)

Dieses Dokument protokolliert chronologisch, was beim ersten realen Deploy des MWE nach SAP BTP Cloud Foundry getan wurde — inklusive Klicks im Cockpit, ausgeführter Shell-Kommandos und getroffener Entscheidungen.
Ziel: Ein:e andere:r Entwickler:in kann den Ablauf auf einer ähnlichen Umgebung (Windows 11, neues BTP-Konto) eins-zu-eins nachvollziehen.

Die autoritative Checkliste liegt in [Issue #4](https://github.com/Hochfrequenz/go-sap-btp-cloud-foundry-mwe/issues/4).
Dieses Walkthrough ergänzt sie um reale URLs, tatsächlich beobachtete Cockpit-Labels und Abweichungen.

---

## Umgebung

- **Betriebssystem:** Windows 11 Pro (10.0.26200), deutsche Lokalisierung
- **Shell:** Git-Bash + Windows PowerShell 5.1
- **Vorhandene Tools vor dem Start:** `gh` 2.88.0
- **Zielkonto:** HF Dev Account (Global Account) → Subaccount `HF CloudFoundry`

---

## Phase 0 — Werkzeuge bereitstellen

### 0.1  Cloud Foundry CLI via winget installieren

Zuerst über `winget search` die korrekte Paket-ID ermittelt.
Wichtig, weil eine naive Vermutung (`CloudFoundry.CloudFoundryCLI`) **fehlschlägt**:

```powershell
winget search "cloud foundry" --accept-source-agreements
# Ergebnis:
# CloudFoundry.CLI.v7   7.7.11
# CloudFoundry.CLI.v8   8.7.11
```

Issue #4 fordert v8+, daher v8 installiert:

```powershell
winget install --id CloudFoundry.CLI.v8 `
  --accept-source-agreements --accept-package-agreements --disable-interactivity
```

Die Installation legt `cf` und `cf8` als CLI-Aliase an und aktualisiert die System-PATH-Variable.

### 0.2  Go 1.26 via winget installieren

```powershell
winget search --id GoLang.Go
# Ergebnis: Go Programming Language 1.26.2

winget install --id GoLang.Go `
  --accept-source-agreements --accept-package-agreements --disable-interactivity
```

Das Repo pinnt in `go.mod` auf `go 1.26`, und 1.26.2 erfüllt das.

### 0.3  Fallstrick: PATH in bestehenden Shells

winget schreibt neue Einträge in die System- und User-PATH-Variable.
**Bereits geöffnete Terminals sehen diese Änderung nicht.**
Konsequenz: Nach der Installation das Terminal schließen und neu öffnen, sonst:

```
bash: cf: command not found
```

Verifikation in einem frischen PowerShell-Fenster:

```powershell
cf --version   # cf.exe version 8.7.11+b1b4068ff.2024-07-09
go version     # go version go1.26.2 windows/amd64
```

---

## Phase 1 — BTP-Cockpit öffnen und Region ermitteln

Ziel dieser Phase: Die Region-Kennung (`eu10`, `us10`, …) herausfinden, die später in `cf api` und `vars.yml` fließt.

### 1.1  Generischen Cockpit-Einstieg ansteuern

Im Browser (hier: Playwright-gesteuertes Chromium):

```
https://account.hana.ondemand.com/
```

Diese URL leitet auf den regionalen Cockpit weiter.
In unserem Fall lautet das Ziel:

```
https://emea.cockpit.btp.cloud.sap/cockpit
```

> **Abweichung zu Issue #4, Sektion 0.**
> Der Hinweis dort — *"Verify at: `https://cockpit.<region>.hana.ondemand.com/`"* — ist veraltet.
> Die aktuelle EMEA-Cockpit-URL ist `https://emea.cockpit.btp.cloud.sap/cockpit`.
> Der generische Einstieg `https://account.hana.ondemand.com/` funktioniert weiterhin und leitet korrekt weiter.

### 1.2  SSO-Login

Der Cockpit zeigte einen Anmelden-Button; nach Klick lief die SAP-ID-Service- bzw. IdP-Authentifizierung durch.
Für dieses Protokoll wurde der Login vom Menschen durchgeführt, nicht durch den Browser-Automaten.

Nach erfolgreichem Login: angemeldet als `Konstantin Klein (konstantin.klein@hochfrequenz.de)`.

### 1.3  Global Account wählen

Der Cockpit fragte: *"Wählen Sie Ihr globales Konto"*.
Zur Auswahl standen:

| Global Account | Subdomain |
| --- | --- |
| **HF Dev Account** | `hochfrequenzunternehmensberatunggmbh` |
| Hochfrequenz Unternehmensberatung GmbH | `hochfrequenzunternehmensberatunggmbh-02` |

**Entscheidung:** `HF Dev Account`, weil das MWE explizit ein Entwicklungs- bzw. Experimentier-Artefakt ist.
Klick auf die Kachel, dann Klick auf **Weiter**.

Ergebnis-URL:

```
https://emea.cockpit.btp.cloud.sap/cockpit#/globalaccount/CA10691993TID000000000740400366/accountModel…
```

### 1.4  Subaccounts sichten

Im HF Dev Account existieren **zwei** Subaccounts:

| Subaccount | Provider | Region (Cockpit-Anzeige) | Umgebung |
| --- | --- | --- | --- |
| **HF CloudFoundry** | AWS | Europe (Frankfurt) – AWS | Umgebungsübergreifend (Cloud Foundry) |
| Hochfrequenz Unternehmensberatung GmbH | SAP (Neo) | Europe (Rot) | **Neo** — wird zum 31.12.2028 eingestellt |

**Entscheidung:** `HF CloudFoundry`.
Der Neo-Subaccount scheidet aus: das MWE zielt explizit auf Cloud Foundry, und Neo befindet sich bereits in der Auslaufphase.

### 1.5  API-Endpunkt ablesen → Region

Klick auf die Kachel **HF CloudFoundry** öffnet die Subaccount-Übersicht.
Dort steht im Block **Cloud Foundry**:

```
API Endpoint:  https://api.cf.eu10.hana.ondemand.com
```

Daraus direkt abgelesen:

- **Region-Slug:** `eu10`
- **cfapps-Domain:** `cfapps.eu10.hana.ondemand.com`

Diese beiden Werte fließen in die folgenden Schritte der Issue-#4-Checkliste ein:

| Datei / Kommando | Wert |
| --- | --- |
| `cf api https://api.cf.<region>.hana.ondemand.com` | `eu10` |
| `vars.yml` → Feld `domain:` | `cfapps.eu10.hana.ondemand.com` |

---

## Phase 2 — Cockpit-Verifikationen (Issue #4, Sektion 0)

### 2.1  Subaccount-Übersicht sichten

Im Subaccount `HF CloudFoundry` wurden auf der Übersichtsseite folgende Kernwerte abgelesen:

| Feld | Wert |
| --- | --- |
| Subdomain | `hf-cf` |
| Region | Europe (Frankfurt) – AWS |
| CF API Endpoint | `https://api.cf.eu10.hana.ondemand.com` |
| Org Name | `HF Dev Account_hf-cf` |
| Org ID | `6bb025f9-f118-4112-9c07-9b35627e4f0f` |
| Org Memory Limit | 2.048 MB |
| Produktiveinsatz | Nein |

Der Subaccount enthält bereits **4 Spaces** mit insgesamt 15 Applikationen und 16 Service-Instanzen:

| Space | Apps | Service-Instanzen |
| --- | --- | --- |
| `dev` | 11 | 10 |
| `listener` | 2 | 3 |
| `process-diagram` | 2 | 3 |
| `prod` | 0 | 0 |

**Entscheidung:** Deploy in den bestehenden `dev`-Space.
Ein eigener Space (z. B. `go-btp-mwe`) wäre sauberer isoliert, setzt aber Subaccount-Admin-Rechte voraus und ist nicht zwingend nötig.

> **Beobachtung (potenzieller Blocker):** Im Cockpit erscheint oben der Hinweis
> *"Einige Daten und Funktionen auf dieser Seite sind nicht für Sie verfügbar. Sie müssen Administrator eines globalen Kontos oder Unterkontos sein."*
> Damit könnten Schritte 4b (Role Collection anlegen) und 4c (Destination anlegen) aus Issue #4 an fehlenden Admin-Rechten scheitern.
> Wir gehen vorläufig weiter und wissen spätestens in Phase 4 Bescheid.

### 2.2  Cloud Connector verifizieren

Navigation: **Konnektivität → Cloud-Connectors**.

Aktueller Stand laut Cockpit:

- Kopfzeile: **Aktive Verbindungen: 1**
- Tabelle: ein Eintrag mit Standort-ID `(default)`
- Zeilen-Aktion "Trennen der Verbindung erzwingen" ist verfügbar → bestätigt, dass die Verbindung gerade aktiv ist.

Daraus folgen zwei verwertbare Erkenntnisse:

1. Der Cloud Connector ist gepaart und online — Voraussetzung aus Sektion 0 erfüllt.
2. Weil es nur genau **eine** Cloud-Connector-Verbindung gibt (Location ID `(default)`), muss in Sektion 4c **kein** `CloudConnectorLocationId` auf der Destination gesetzt werden.

> **Abweichung zu Issue #4, Sektion 0:**
> Der Checklisten-Text sagt, die CC-Zeile müsse *"Connected (grün)"* anzeigen.
> Das aktuelle Cockpit zeigt **keinen** farbigen Pro-Zeile-Status, sondern stattdessen oben einen Zähler `Aktive Verbindungen: N` und listet unten die Location-IDs der gerade verbundenen Cloud Connectors.
> Erscheint die Location-ID im Grid, gilt der CC als verbunden.

### 2.3  Entitlements (nicht verifizierbar)

Ein eigener Nav-Eintrag *"Berechtigungen / Entitlements"* war in der linken Navigation **nicht sichtbar**.
Wahrscheinliche Ursache: fehlende Admin-Rechte (passt zur obigen Warnmeldung).

**Vorgehen:** Wir überspringen die explizite Entitlement-Verifikation und lassen das spätere `cf create-service` in Sektion 2 als De-facto-Prüfung laufen.
Das Risiko ist gering, weil der Subaccount bereits produktiv 11 Apps in `dev` betreibt; XSUAA, Destination und Connectivity sind faktisch mit hoher Wahrscheinlichkeit entitlet.
Falls doch nicht, schlägt `cf create-service` explizit mit einem Entitlement-Fehler fehl und wir stoppen dort.

---

## Phase 3 — CF-CLI-Login und lokales Preflight (Issue #4, Sektion 1)

### 3.1  API-Endpunkt setzen und anmelden

```powershell
cf api https://api.cf.eu10.hana.ondemand.com     # setzt den Endpunkt für die Session
cf login --sso                                    # → druckt eine /passcode-URL, öffnen, Code einfügen
# Auswahl-Prompts:
#   Org   → HF Dev Account_hf-cf
#   Space → dev
```

Nach erfolgreichem Login zeigt `cf target`:

```
API endpoint:   https://api.cf.eu10.hana.ondemand.com
API version:    3.215.0
user:           konstantin.klein@hochfrequenz.de
org:            HF Dev Account_hf-cf
space:          dev
```

### 3.2  Preflight-Checks

| Kommando | Ergebnis |
| --- | --- |
| `go test ./...` | ✅ alle Tests grün |
| `go test ./... -race` | ❌ `-race requires cgo; enable cgo by setting CGO_ENABLED=1` — unter Windows ohne MinGW-gcc nicht verfügbar; CI läuft auf Linux mit `-race`, darum lokal übersprungen |
| `go vet ./...` | ✅ sauber |
| `cf buildpacks \| grep -i go` | ✅ `go_buildpack cflinuxfs4 v1.10.44` → klassisches CF-Buildpack, **kein** Paketo |
| `cp vars.example.yml vars.yml` | ✅ Default `cfapps.eu10.hana.ondemand.com` bereits korrekt — keine Editierung nötig |

> **Beobachtung:** `vars.yml` ist nicht in `.gitignore`.
> Enthält hier nur Hostname und Domain (keine Geheimnisse), sollte aber der Konsistenz halber ignoriert werden.
> Follow-up.

### 3.3  Kollisions-Prechecks

Im bestehenden `dev`-Space:

- **App-Namen:** `go-btp-mwe`, `go-btp-mwe-web` existieren dort noch nicht — keine Kollision.
- **Service-Namen:** `go-xsuaa`, `go-dest`, `go-cc` existieren dort noch nicht — keine Kollision.
- **Entitlements:** Im Space existieren bereits Instanzen von `xsuaa/application`, `destination/lite` und `connectivity/lite` (unter anderen Namen wie `authTest`, `destinationService`, `connectivityService`, `s4md-xsuaa`).
  Das belegt indirekt, dass die benötigten Entitlements im Subaccount vorhanden sind — der zuvor nicht verifizierbare Punkt aus Phase 2.3 ist damit erledigt.

---

## Phase 4 — Service-Instanzen anlegen (Issue #4, Sektion 2)

```powershell
cf create-service xsuaa        application go-xsuaa -c xs-security.json
cf create-service destination  lite        go-dest
cf create-service connectivity lite        go-cc
```

`go-dest` und `go-cc` werden **synchron** erstellt (`OK` zurück in <1 s).
`go-xsuaa` läuft **asynchron** (`Create in progress`), ist aber typischerweise in <5 s fertig.

Polling per `cf service go-xsuaa`:

```
status:    create succeeded
started:   2026-04-23T12:11:09Z
```

Alle drei Instanzen waren nach ca. 10 s vorhanden.

---

## Phase 5 — `cf push` (Issue #4, Sektion 3)

Zwei Fehlschläge vor dem erfolgreichen Push.
Die Details sind wichtig, weil beide Fehlermodi in der Checkliste nicht gewarnt waren.

### 5.1  Erster Fehlschlag — Route-Kontingent überschritten

```
FAILED
For application 'go-btp-mwe': Routes quota exceeded for organization 'HF Dev Account_hf-cf'.
```

Ursache: Die Org hat ein Routen-Kontingent von **20** und lag bereits bei exakt `20/20` Routen.
`cf push` benötigt für zwei Apps zwei zusätzliche Routen — dafür war kein Platz.

**Diagnose-Kommando** (org-weit, alle Spaces, im Gegensatz zu `cf routes`, das nur den aktuellen Space zeigt):

```powershell
cf curl "/v3/routes?per_page=100&organization_guids=<org-guid>" `
  | ConvertFrom-Json `
  | Select-Object -ExpandProperty pagination `
  | Select-Object -ExpandProperty total_results
```

Kontingent ablesen:

```powershell
cf curl /v3/organization_quotas/<quota-guid> | ConvertFrom-Json | Select-Object routes
```

**Lösung:** Eine gestoppte App entfernt, deren Routen-Slots freizugeben:

```powershell
cf delete hf-learn -f -r
```

Überraschung: obwohl `cf routes` im `dev`-Space nur eine Route für `hf-learn` zeigte, hat das Löschen mit `-r` **drei** Routen-Slots freigegeben (org-weit, inklusive Orphan-Routen aus anderen Spaces).
Route-Count nach Löschung: `17/20`.

### 5.2  Zweiter Fehlschlag — Buildpack findet `main` nicht

Staging lief, dann:

```
-----> Installing go 1.23.12
**WARNING** Installing package '.' (default)
-----> Running: go install -tags cloudfoundry -buildmode pie .
no Go files in /tmp/app
**ERROR** Unable to compile application: exit status 1
BuildpackCompileFailed
```

Ursache: Das klassische CF-`go_buildpack` **erkennt `cmd/server` nicht automatisch** — es führt `go install .` im Modul-Root aus, und dort liegen keine `.go`-Dateien.
Die Auto-Erkennung von `cmd/server` ist eine **Paketo-spezifische Eigenschaft** (via `BP_GO_TARGETS`), die das klassische Buildpack **nicht** hat.
Der bisherige Kommentar in `manifest.yml` und die README hatten das falsch dargestellt.

**Fix:** `GO_INSTALL_PACKAGE_SPEC: ./cmd/server` als `env:` auf der Backend-App setzen.
Der zugehörige PR ist [#8](https://github.com/Hochfrequenz/go-sap-btp-cloud-foundry-mwe/pull/8).

### 5.3  Beobachtung zu Go-Versionen (kein Fehler, aber Risiko)

Das Buildpack installiert **Go 1.23.12**, obwohl `go.mod` `go 1.26` verlangt.
Go 1.23 ist mit dem 1.26-Release (Feb 2026) EOL gegangen.
Der Build hat in unserem Fall trotzdem funktioniert — entweder zieht Gos Auto-Toolchain-Feature `go 1.26` über das Netzwerk (BTP-Stager erlaubt hier offenbar Egress), oder der Code nutzt keine post-1.23-Features.
Wird ein zukünftiger Commit 1.26-Features nutzen, schlägt der Build ohne Vorwarnung fehl.

### 5.4  Dritter Versuch — grün

Nach dem Fix erreichten beide Apps `running 1/1`:

| App | URL | RAM | Status |
| --- | --- | --- | --- |
| `go-btp-mwe` | `https://go-btp-mwe.cfapps.eu10.hana.ondemand.com` | 19,9 MB / 128 MB | running |
| `go-btp-mwe-web` | `https://go-btp-mwe-web.cfapps.eu10.hana.ondemand.com` | 61,9 MB / 128 MB | running |

Smoke-Test:

```powershell
Invoke-WebRequest https://go-btp-mwe-web.cfapps.eu10.hana.ondemand.com/healthz
# status=200 body=ok
```

`/healthz` ist in `web/xs-app.json` explizit `authenticationType: none` — bestätigt, dass Approuter → Backend durchreicht, Gin-Handler lebt und die Kette vollständig verdrahtet ist.

---

## Aktueller Stand

- Phasen 0–5 abgeschlossen.
- Beide Apps laufen, `/healthz` grün.
- Weiter: Sektionen 4a (Redirect-URIs in XSUAA), 4b (Role Collection + Zuweisung), optional 4c (Destination), 5 (vollständiger Smoke-Test einschließlich `/api/me`).

---

## Beobachtete Stolpersteine (Zusammenfassung)

Die folgenden Punkte sind bei einer Reproduktion auf einer leeren Windows-Entwicklermaschine nicht trivial und lohnen das Aufschreiben:

1. **Richtiger winget-Paketname für die CF-CLI.** `CloudFoundry.CloudFoundryCLI` existiert nicht — korrekt ist `CloudFoundry.CLI.v8`.
2. **PATH wird erst in frischen Shells sichtbar.** Terminal schließen und neu öffnen, nicht im selben Fenster weiterarbeiten.
3. **EMEA-Cockpit-URL hat sich geändert.** Nicht mehr `cockpit.eu10.hana.ondemand.com`, sondern `emea.cockpit.btp.cloud.sap`. Der generische Einstieg `account.hana.ondemand.com` bleibt stabil und ist der sicherste Ausgangspunkt.
4. **Zwei Subaccounts im HF Dev Account.** Nur `HF CloudFoundry` ist relevant; der zweite (Neo) ist ein toter Pfad für dieses Projekt.
5. **`cf routes` zeigt nur den aktuellen Space, das Routen-Kontingent gilt org-weit.** Vor jedem `cf push` org-weit gegen das Kontingent prüfen, sonst schlägt der Push fehl, bevor Staging überhaupt startet.
6. **Das klassische `go_buildpack` erkennt `cmd/server` nicht automatisch.** `GO_INSTALL_PACKAGE_SPEC: ./cmd/server` ist Pflicht, nicht optional. (Fix in PR #8.)
7. **Das Cockpit zeigt für Cloud Connectors keinen farbigen "Connected (grün)"-Status pro Zeile.** Stattdessen: oben `Aktive Verbindungen: N`, unten die Liste der aktiven Location-IDs. Erscheint die Location-ID, gilt der CC als verbunden.
8. **Das eu10-Buildpack ist auf Go 1.23.12 eingefroren.** Go 1.23 ist EOL. Deploys funktionieren heute noch, aber sobald Code post-1.23-Features nutzt, wird der Build brechen.
