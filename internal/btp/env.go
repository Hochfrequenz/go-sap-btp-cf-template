// Package btp wires a Go application into SAP BTP Cloud Foundry: it reads
// service bindings from VCAP_SERVICES, validates incoming JWTs minted by
// XSUAA, and calls on-premise SAP systems through the Connectivity and
// Destination services.
package btp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// ErrNotInCloudFoundry signals that VCAP_APPLICATION / VCAP_SERVICES were
// absent. This MWE refuses to start in that case; there is no meaningful
// local-dev mode for a BTP app, and stubbing would invite code paths that
// only ever run on a developer laptop.
var ErrNotInCloudFoundry = errors.New("no Cloud Foundry environment detected (VCAP_APPLICATION/VCAP_SERVICES unset)")

// ErrNoXSUAABinding signals the XSUAA binding is missing. XSUAA is always
// required: without it the app cannot validate any incoming JWT.
var ErrNoXSUAABinding = errors.New("xsuaa binding not found")

// ErrNoDestinationBinding signals the Destination service binding is missing.
// Returned when a feature that needs the Destination service is used.
var ErrNoDestinationBinding = errors.New("destination service not bound")

// ErrNoConnectivityBinding signals the Connectivity service binding is missing.
// Returned when an on-premise call is attempted without the binding.
var ErrNoConnectivityBinding = errors.New("connectivity service not bound")

// XSUAACredentials is the subset of the XSUAA binding the MWE reads.
// The full payload has ~20 additional fields; decoding only what we use keeps
// us resilient to unrelated changes in the service.
type XSUAACredentials struct {
	URL             string `json:"url"`
	ClientID        string `json:"clientid"`
	ClientSecret    string `json:"clientsecret"`
	XSAppName       string `json:"xsappname"`
	UAADomain       string `json:"uaadomain"`
	VerificationKey string `json:"verificationkey"`
	Identityzone    string `json:"identityzone"`
}

// JWKSURL is where XSUAA publishes its signing keys.
func (x *XSUAACredentials) JWKSURL() string {
	return trimSlash(x.URL) + "/token_keys"
}

// Validate appends all structural problems of x to errs with an "xsuaa: "
// prefix. Aggregation instead of early return is deliberate — we want every
// misconfiguration in one pass so operators fix them all at once.
func (x *XSUAACredentials) Validate(errs *[]string) {
	if x == nil {
		*errs = append(*errs, "xsuaa: binding is nil")
		return
	}
	if x.URL == "" {
		*errs = append(*errs, "xsuaa: url is required")
	} else if u, err := url.Parse(x.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		*errs = append(*errs, fmt.Sprintf("xsuaa: url %q must be a valid http/https URL", x.URL))
	}
	if x.ClientID == "" {
		*errs = append(*errs, "xsuaa: clientid is required")
	}
	if x.ClientSecret == "" {
		*errs = append(*errs, "xsuaa: clientsecret is required")
	}
	// XSAppName is no longer used for JWT validation (ClientID is), but it
	// is a standard XSUAA binding field and its absence here hints at a
	// mis-parsed VCAP payload, so we keep the completeness check.
	if x.XSAppName == "" {
		*errs = append(*errs, "xsuaa: xsappname is required")
	}
	if x.UAADomain == "" {
		*errs = append(*errs, "xsuaa: uaadomain is required")
	}
}

// String masks secrets so accidental %v / %+v logging cannot leak them.
func (x *XSUAACredentials) String() string {
	if x == nil {
		return "<nil xsuaa>"
	}
	return fmt.Sprintf("XSUAA{URL:%s ClientID:%s ClientSecret:*** XSAppName:%s UAADomain:%s}",
		x.URL, x.ClientID, x.XSAppName, x.UAADomain)
}

// Format routes %v/%+v/%#v through String() so secret scrubbing survives
// every formatter that callers might reach for in a log line.
func (x *XSUAACredentials) Format(s fmt.State, _ rune) { _, _ = fmt.Fprint(s, x.String()) }

// DestCredentials is the Destination service binding.
type DestCredentials struct {
	URI          string `json:"uri"`
	ClientID     string `json:"clientid"`
	ClientSecret string `json:"clientsecret"`
	// URL is the XSUAA token endpoint for this service instance; distinct
	// from XSUAACredentials.URL because the destination service may be
	// bound to a different UAA tenant.
	URL string `json:"url"`
}

func (d *DestCredentials) Validate(errs *[]string) {
	if d == nil {
		return
	}
	if d.URI == "" {
		*errs = append(*errs, "destination: uri is required")
	} else if u, err := url.Parse(d.URI); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		*errs = append(*errs, fmt.Sprintf("destination: uri %q must be a valid http/https URL", d.URI))
	}
	if d.ClientID == "" {
		*errs = append(*errs, "destination: clientid is required")
	}
	if d.ClientSecret == "" {
		*errs = append(*errs, "destination: clientsecret is required")
	}
	if d.URL == "" {
		*errs = append(*errs, "destination: url (token endpoint) is required")
	}
}

func (d *DestCredentials) String() string {
	if d == nil {
		return "<nil destination>"
	}
	return fmt.Sprintf("Destination{URI:%s ClientID:%s ClientSecret:*** URL:%s}",
		d.URI, d.ClientID, d.URL)
}
func (d *DestCredentials) Format(s fmt.State, _ rune) { _, _ = fmt.Fprint(s, d.String()) }

// ConnCredentials is the Connectivity service binding.
type ConnCredentials struct {
	ClientID           string `json:"clientid"`
	ClientSecret       string `json:"clientsecret"`
	URL                string `json:"url"`
	OnPremiseProxyHost string `json:"onpremise_proxy_host"`
	OnPremiseProxyPort string `json:"onpremise_proxy_port"`
}

func (c *ConnCredentials) Validate(errs *[]string) {
	if c == nil {
		return
	}
	if c.ClientID == "" {
		*errs = append(*errs, "connectivity: clientid is required")
	}
	if c.ClientSecret == "" {
		*errs = append(*errs, "connectivity: clientsecret is required")
	}
	if c.URL == "" {
		*errs = append(*errs, "connectivity: url (token endpoint) is required")
	}
	if c.OnPremiseProxyHost == "" {
		*errs = append(*errs, "connectivity: onpremise_proxy_host is required")
	}
	if c.OnPremiseProxyPort == "" {
		*errs = append(*errs, "connectivity: onpremise_proxy_port is required")
	} else if p, err := strconv.Atoi(c.OnPremiseProxyPort); err != nil || p < 1 || p > 65535 {
		*errs = append(*errs, fmt.Sprintf("connectivity: onpremise_proxy_port %q must be a number in 1..65535", c.OnPremiseProxyPort))
	}
}

func (c *ConnCredentials) String() string {
	if c == nil {
		return "<nil connectivity>"
	}
	return fmt.Sprintf("Connectivity{ClientID:%s ClientSecret:*** URL:%s ProxyHost:%s ProxyPort:%s}",
		c.ClientID, c.URL, c.OnPremiseProxyHost, c.OnPremiseProxyPort)
}
func (c *ConnCredentials) Format(s fmt.State, _ rune) { _, _ = fmt.Fprint(s, c.String()) }

// Env is the app's view of its Cloud Foundry bindings.
type Env struct {
	XSUAA *XSUAACredentials
	Dest  *DestCredentials
	Conn  *ConnCredentials
}

// Validate aggregates problems across all bindings and returns them as a
// single error with one bullet per problem. Modeled on
// Hochfrequenz/sap-mcp-config so operators see every misconfiguration in one
// startup log line instead of discovering them one request at a time.
func (e *Env) Validate() error {
	var errs []string
	if e.XSUAA == nil {
		errs = append(errs, "xsuaa: binding is required")
	} else {
		e.XSUAA.Validate(&errs)
	}
	e.Dest.Validate(&errs)
	e.Conn.Validate(&errs)
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid BTP environment:\n  - %s", strings.Join(errs, "\n  - "))
}

// LoadEnv parses VCAP_SERVICES / VCAP_APPLICATION into typed credentials and
// runs full validation. Returns ErrNotInCloudFoundry if the app is not
// running under CF; that case is fatal for this MWE.
func LoadEnv() (*Env, error) {
	// Both env vars must be present to consider the app as running under CF.
	// We don't actually consume any field of VCAP_APPLICATION — its presence
	// is the signal we are in a CF runtime; VCAP_SERVICES carries the
	// bindings we read.
	if os.Getenv("VCAP_APPLICATION") == "" {
		return nil, fmt.Errorf("%w: VCAP_APPLICATION is unset", ErrNotInCloudFoundry)
	}
	rawServices := os.Getenv("VCAP_SERVICES")
	if rawServices == "" {
		return nil, fmt.Errorf("%w: VCAP_SERVICES is unset", ErrNotInCloudFoundry)
	}

	var services vcapServices
	if err := json.Unmarshal([]byte(rawServices), &services); err != nil {
		return nil, fmt.Errorf("parse VCAP_SERVICES: %w", err)
	}

	e := &Env{}
	if err := decodeService(services, "xsuaa", &e.XSUAA); err != nil {
		return nil, fmt.Errorf("decode xsuaa binding: %w", err)
	}
	if e.XSUAA == nil {
		return nil, ErrNoXSUAABinding
	}
	if err := decodeService(services, "destination", &e.Dest); err != nil {
		return nil, fmt.Errorf("decode destination binding: %w", err)
	}
	if err := decodeService(services, "connectivity", &e.Conn); err != nil {
		return nil, fmt.Errorf("decode connectivity binding: %w", err)
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return e, nil
}

// vcapServices is the typed shape of VCAP_SERVICES. The CF runtime emits
// it as a JSON object keyed by service-broker label (e.g. "xsuaa",
// "destination", "connectivity"); each value is an array of bound
// instances of that service. We only need the first instance's
// credentials per label this MWE consumes, so the binding type itself
// is intentionally minimal — anything else CF puts in the binding
// (name, plan, tags, instance_guid, …) is dropped at parse time.
//
// Replaces github.com/cloudfoundry-community/go-cfenv (whose 2020-vintage
// transitive mapstructure dependency was the rot risk that motivated
// this change). The on-the-wire shape is stable, so a tiny typed parser
// gives us the same surface with one fewer dep, no reflection, and no
// maintenance liability.
type vcapServices map[string][]vcapBinding

// vcapBinding holds the per-instance credentials. The exact shape of
// Credentials varies by service (xsuaa is one schema, destination
// another), so we keep the field as raw JSON and decode into typed
// structs once we know which label we are looking at.
type vcapBinding struct {
	Credentials json.RawMessage `json:"credentials"`
}

// decodeService decodes the first binding with the given label into out.
// If no binding exists, out is left as nil. Distinguishing "binding absent"
// from "decode failed" matters because Env.Validate wants to report missing
// required bindings itself (with the same error shape as other problems).
func decodeService(services vcapServices, label string, out any) error {
	bindings, ok := services[label]
	if !ok || len(bindings) == 0 {
		return nil
	}
	if len(bindings[0].Credentials) == 0 {
		return fmt.Errorf("%s binding has empty credentials", label)
	}
	return json.Unmarshal(bindings[0].Credentials, out)
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
