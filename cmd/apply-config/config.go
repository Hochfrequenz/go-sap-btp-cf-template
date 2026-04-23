package main

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"
)

// Config is the complete typed view of config.yml. Every field that
// maps onto a CF or XSUAA artifact is required; Validate() aggregates
// all problems into a single error so the operator sees the whole
// picture in one run instead of fixing one field at a time (same
// aggregated-error pattern as internal/btp/env.go's Validate).
type Config struct {
	App      AppConfig      `yaml:"app"`
	Services ServicesConfig `yaml:"services"`
	CF       CFConfig       `yaml:"cf"`
}

// AppConfig identifies the CF backend app and the Go module it lives in.
type AppConfig struct {
	// Name is the CF backend app name. The approuter is auto-derived
	// as <name>-web by manifest.yml's app block, not a separate field.
	Name string `yaml:"name"`
	// Module is the Go module path — exactly what ends up in go.mod
	// and in every Go import statement.
	Module string `yaml:"module"`
}

// ServicesConfig names the three CF service instances the backend
// binds to. Any field left blank is derived from AppConfig.Name with
// the conventional suffix; see applyDefaults.
type ServicesConfig struct {
	XSUAA        string `yaml:"xsuaa"`
	Destination  string `yaml:"destination"`
	Connectivity string `yaml:"connectivity"`
}

// CFConfig is the CF landscape coordinates the deploy workflow pushes to.
type CFConfig struct {
	// API is the Cloud Controller endpoint (https://api.cf.<region>...).
	API string `yaml:"api"`
	// Org is the CF org name. SAP BTP uses the literal "Global Account
	// Name_subaccount-subdomain" with an underscore, e.g.
	// "HF Dev Account_hf-cf". Spaces and underscores are legitimate here.
	Org string `yaml:"org"`
	// Space is the CF space inside that org (e.g. "dev", "prod").
	Space string `yaml:"space"`
	// Domain is the apps shared-domain suffix
	// (cfapps.<region>.hana.ondemand.com).
	Domain string `yaml:"domain"`
}

// LoadConfig reads, defaults, and validates config.yml at path.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// applyDefaults fills blank service-instance names by appending the
// conventional suffix to App.Name. Leaves the field alone if App.Name
// is itself empty — Validate will flag that separately.
func (c *Config) applyDefaults() {
	if c.App.Name == "" {
		return
	}
	if c.Services.XSUAA == "" {
		c.Services.XSUAA = c.App.Name + "-xsuaa"
	}
	if c.Services.Destination == "" {
		c.Services.Destination = c.App.Name + "-dest"
	}
	if c.Services.Connectivity == "" {
		c.Services.Connectivity = c.App.Name + "-cc"
	}
}

// cfAppNameRegex matches Cloud Foundry app / service-instance name
// rules: lowercase, alphanumeric, hyphens; no leading or trailing
// hyphen. CF accepts a broader set of characters in service-instance
// names, but we stay conservative because manifest and cf-cli behave
// predictably only on this subset.
var cfAppNameRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Validate returns nil or a single error that lists every field that
// failed a check.
func (c *Config) Validate() error {
	var errs []string

	if c.App.Name == "" {
		errs = append(errs, "app.name is required")
	} else if !cfAppNameRegex.MatchString(c.App.Name) {
		errs = append(errs, fmt.Sprintf("app.name %q must be lowercase alphanumeric with hyphens (CF app-name rules)", c.App.Name))
	}

	if c.App.Module == "" {
		errs = append(errs, "app.module is required (Go module path)")
	} else if !strings.Contains(c.App.Module, "/") || !strings.Contains(c.App.Module, ".") {
		errs = append(errs, fmt.Sprintf("app.module %q does not look like a Go module path (expected host/org/repo form)", c.App.Module))
	}

	if c.CF.API == "" {
		errs = append(errs, "cf.api is required")
	} else if u, err := url.Parse(c.CF.API); err != nil || u.Scheme != "https" || u.Host == "" {
		errs = append(errs, fmt.Sprintf("cf.api %q must be a full https URL (scheme + host)", c.CF.API))
	}
	if c.CF.Org == "" {
		errs = append(errs, "cf.org is required")
	}
	if c.CF.Space == "" {
		errs = append(errs, "cf.space is required")
	}
	if c.CF.Domain == "" {
		errs = append(errs, "cf.domain is required")
	}

	for _, s := range []struct{ name, val string }{
		{"services.xsuaa", c.Services.XSUAA},
		{"services.destination", c.Services.Destination},
		{"services.connectivity", c.Services.Connectivity},
	} {
		if s.val == "" {
			errs = append(errs, s.name+" is required (leave blank to derive from app.name)")
		} else if !cfAppNameRegex.MatchString(s.val) {
			errs = append(errs, fmt.Sprintf("%s %q must be lowercase alphanumeric with hyphens", s.name, s.val))
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid config:\n  - %s", strings.Join(errs, "\n  - "))
}
