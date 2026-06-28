// Package config loads and validates the router's single YAML configuration
// (ADR-0010). It is loaded once at startup and fully validated before the server
// begins listening; validation failures abort the process with a clear message.
//
// gopkg.in/yaml.v3 is the sole sanctioned non-stdlib dependency (ADR-0010,
// ADR-0015): JSON cannot carry the comments operators need.
package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole router configuration.
type Config struct {
	Listen      string           `yaml:"listen"`
	MaxBodySize ByteSize         `yaml:"max_body_size"`
	Auth        Auth             `yaml:"auth"`
	Backends    []Backend        `yaml:"backends"`
	Aliases     map[string]Alias `yaml:"aliases"`
	Health      Health           `yaml:"health"`
	Timeouts    Timeouts         `yaml:"timeouts"`
}

// Auth holds the optional inbound token allowlist (ADR-0009).
type Auth struct {
	Tokens []string `yaml:"tokens"`
}

// UnmarshalYAML decodes the auth block while PRESERVING token entries that
// resolved to empty. The default []string decoder silently drops a null
// sequence entry — which is exactly what an unset ${ENV} placeholder becomes
// after interpolation (`- ${ROUTER_API_KEY}` → `- ` → null). Dropping it would
// turn a config that intended to REQUIRE a key into an empty allowlist (auth
// disabled = open router). Keeping the empty entry lets validate() fail closed
// (ADR-0009/0010). An absent, null, or `tokens: []` value remains valid and
// means "trust the LAN".
func (a *Auth) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Tokens yaml.Node `yaml:"tokens"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}
	a.Tokens = nil
	switch raw.Tokens.Kind {
	case 0: // tokens key absent
		return nil
	case yaml.ScalarNode:
		if raw.Tokens.Tag == "!!null" {
			return nil // explicit null → LAN trust
		}
		return fmt.Errorf("auth.tokens must be a list of strings")
	case yaml.SequenceNode:
		for _, item := range raw.Tokens.Content {
			var s string
			if err := item.Decode(&s); err != nil {
				return fmt.Errorf("auth.tokens entry: %w", err)
			}
			a.Tokens = append(a.Tokens, s)
		}
		return nil
	default:
		return fmt.Errorf("auth.tokens must be a list of strings")
	}
}

// Backend is one upstream, defined by its OpenAI-compatible base URL alone
// (ADR-0002).
type Backend struct {
	Name        string      `yaml:"name"`
	BaseURL     string      `yaml:"base_url"`
	Protocol    string      `yaml:"protocol"` // openai|anthropic, default openai
	Credentials Credentials `yaml:"credentials"`
}

// Credentials are the operator-owned, outbound-injected provider credentials
// (ADR-0009). They are env-interpolated and never logged.
type Credentials struct {
	APIKey           string `yaml:"api_key"`
	AnthropicVersion string `yaml:"anthropic_version"`
}

// Alias is a stable friendly name plus its routing strategy (ADR-0004, ADR-0006).
type Alias struct {
	Type     string `yaml:"type"`     // proxy|fusion, default proxy
	Selector string `yaml:"selector"` // round_robin|pareto, default round_robin

	// proxy + round_robin: a single upstream model over one or more backends.
	Model    string   `yaml:"model"`
	Backends []string `yaml:"backends"`

	// proxy + pareto: a pool of candidate models with quality scores (ADR-0013).
	MinQuality float64     `yaml:"min_quality"`
	Pool       []PoolEntry `yaml:"pool"`

	// fusion: panel → judge → synthesis (ADR-0014).
	Panel               []PoolEntry `yaml:"panel"`
	Judge               Target      `yaml:"judge"`
	Synthesis           Target      `yaml:"synthesis"`
	Temperature         float64     `yaml:"temperature"`
	MaxCompletionTokens int         `yaml:"max_completion_tokens"`
	// MinPanelResponses is the smallest number of panelist answers fusion will
	// proceed with (ADR-0014). 0 means the default of 1; otherwise it must be
	// between 1 and the panel size.
	MinPanelResponses int `yaml:"min_panel_responses"`
}

// PoolEntry is one candidate model in a pareto pool or fusion panel.
type PoolEntry struct {
	Model    string   `yaml:"model"`
	Backends []string `yaml:"backends"`
	Quality  float64  `yaml:"quality"`
}

// Target is a single model + backend set (fusion judge/synthesis).
type Target struct {
	Model    string   `yaml:"model"`
	Backends []string `yaml:"backends"`
}

// Health configures the discovery/liveness loop (ADR-0005).
type Health struct {
	Interval Duration `yaml:"interval"`
	Timeout  Duration `yaml:"timeout"`
}

// Timeouts configures per-request upstream deadlines (ADR-0007).
type Timeouts struct {
	Connect Duration `yaml:"connect"`
	Request Duration `yaml:"request"`
	Idle    Duration `yaml:"idle"`
}

// Load reads, env-expands, parses, and fully validates the config at path. On
// any error the caller should exit non-zero before listening (ADR-0010).
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	expanded := expandEnv(string(raw))

	var c Config
	if err := yaml.Unmarshal([]byte(expanded), &c); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${VAR} with the environment value (empty if unset). Only
// the braced form is expanded, so YAML containing bare `$` is left intact.
func expandEnv(s string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-1]
		return os.Getenv(name)
	})
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.MaxBodySize == 0 {
		c.MaxBodySize = 256 << 20 // 256 MiB
	}
	if c.Health.Interval == 0 {
		c.Health.Interval = Duration(10 * time.Second)
	}
	if c.Health.Timeout == 0 {
		c.Health.Timeout = Duration(2 * time.Second)
	}
	if c.Timeouts.Connect == 0 {
		c.Timeouts.Connect = Duration(5 * time.Second)
	}
	if c.Timeouts.Request == 0 {
		c.Timeouts.Request = Duration(120 * time.Second)
	}
	if c.Timeouts.Idle == 0 {
		c.Timeouts.Idle = Duration(30 * time.Second)
	}
	for i := range c.Backends {
		if c.Backends[i].Protocol == "" {
			c.Backends[i].Protocol = "openai"
		}
	}
	for name, a := range c.Aliases {
		if a.Type == "" {
			a.Type = "proxy"
		}
		if a.Selector == "" {
			a.Selector = "round_robin"
		}
		// Normalize min_panel_responses ONCE at the config boundary (ADR-0014): an
		// unset (0) value means the default of 1. Downstream (router resolve, fusion
		// execute) trusts MinPanel is >= 1 and never re-normalizes.
		if a.MinPanelResponses == 0 {
			a.MinPanelResponses = 1
		}
		c.Aliases[name] = a
	}
}

func (c *Config) validate() error {
	// Fail closed on auth (ADR-0009/0010): an empty/absent list means "trust the
	// LAN", but an entry that resolved to empty almost certainly came from an
	// unset ${ENV} placeholder. Silently dropping it would turn a config that
	// intended to REQUIRE a key into an open router, so reject it instead.
	for _, t := range c.Auth.Tokens {
		if strings.TrimSpace(t) == "" {
			return fmt.Errorf("config: auth.tokens contains an entry that resolved to empty (unset ${ENV}?)")
		}
	}

	if len(c.Backends) == 0 {
		return fmt.Errorf("config: at least one backend is required")
	}
	defined := map[string]bool{}
	for _, b := range c.Backends {
		if b.Name == "" {
			return fmt.Errorf("config: a backend is missing a name")
		}
		if defined[b.Name] {
			return fmt.Errorf("config: duplicate backend name %q", b.Name)
		}
		defined[b.Name] = true
		if err := validateURL(b.BaseURL); err != nil {
			return fmt.Errorf("config: backend %q base_url: %w", b.Name, err)
		}
		if b.Protocol != "openai" && b.Protocol != "anthropic" {
			return fmt.Errorf("config: backend %q protocol %q must be openai or anthropic", b.Name, b.Protocol)
		}
		// Anthropic backends require their credentials up front (ADR-0009/0010):
		// without an api_key and anthropic_version the outbound injection is
		// incomplete and every call/probe 5xxes at runtime. Fail fast instead of
		// shipping a silently-misconfigured backend.
		if b.Protocol == "anthropic" {
			if strings.TrimSpace(b.Credentials.APIKey) == "" {
				return fmt.Errorf("config: anthropic backend %q requires credentials.api_key", b.Name)
			}
			if strings.TrimSpace(b.Credentials.AnthropicVersion) == "" {
				return fmt.Errorf("config: anthropic backend %q requires credentials.anthropic_version", b.Name)
			}
		}
	}

	for name, a := range c.Aliases {
		// Selector is an enum independent of type (ADR-0010): reject an illegal
		// value on ANY alias that sets one, so a fusion alias with `selector: bogus`
		// can't slip through the type-specific checks below.
		if a.Selector != "" && a.Selector != "round_robin" && a.Selector != "pareto" {
			return fmt.Errorf("config: alias %q selector %q must be round_robin or pareto", name, a.Selector)
		}
		switch a.Type {
		case "proxy":
			if err := validateProxyAlias(name, a, defined); err != nil {
				return err
			}
		case "fusion":
			if err := validateFusionAlias(name, a, defined); err != nil {
				return err
			}
		default:
			return fmt.Errorf("config: alias %q type %q must be proxy or fusion", name, a.Type)
		}
	}
	return nil
}

// validateProxyAlias validates a proxy alias (ADR-0004, ADR-0013). The selector
// enum is checked by the caller; here pareto pools and round_robin single-models
// are validated against their backend references.
func validateProxyAlias(name string, a Alias, defined map[string]bool) error {
	if a.Selector == "pareto" {
		if len(a.Pool) == 0 {
			return fmt.Errorf("config: pareto alias %q needs a non-empty pool", name)
		}
		for _, p := range a.Pool {
			if p.Model == "" {
				return fmt.Errorf("config: pareto alias %q has a pool entry with no model", name)
			}
			if err := checkRefs(name, p.Backends, defined); err != nil {
				return err
			}
		}
		return nil
	}
	// round_robin (the default selector).
	if a.Model == "" {
		return fmt.Errorf("config: proxy alias %q needs a model", name)
	}
	if len(a.Backends) == 0 {
		return fmt.Errorf("config: proxy alias %q needs at least one backend", name)
	}
	return checkRefs(name, a.Backends, defined)
}

// validateFusionAlias validates a fusion alias (ADR-0014): a bounded, non-empty
// panel, a reachable min_panel_responses threshold, and judge/synthesis targets
// with valid backend references.
func validateFusionAlias(name string, a Alias, defined map[string]bool) error {
	if len(a.Panel) == 0 {
		return fmt.Errorf("config: fusion alias %q needs a non-empty panel", name)
	}
	if len(a.Panel) > 8 {
		return fmt.Errorf("config: fusion alias %q panel exceeds 8 panelists", name)
	}
	for _, p := range a.Panel {
		if p.Model == "" {
			return fmt.Errorf("config: fusion alias %q has a panelist with no model", name)
		}
		if err := checkRefs(name, p.Backends, defined); err != nil {
			return err
		}
	}
	// min_panel_responses is normalized to >= 1 at the config boundary; it must be
	// reachable given the panel size (ADR-0014).
	if a.MinPanelResponses < 1 || a.MinPanelResponses > len(a.Panel) {
		return fmt.Errorf("config: fusion alias %q min_panel_responses %d must be between 1 and the panel size %d", name, a.MinPanelResponses, len(a.Panel))
	}
	if a.Judge.Model == "" || a.Synthesis.Model == "" {
		return fmt.Errorf("config: fusion alias %q needs both judge and synthesis models", name)
	}
	if err := checkRefs(name, a.Judge.Backends, defined); err != nil {
		return err
	}
	return checkRefs(name, a.Synthesis.Backends, defined)
}

func validateURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("must be an absolute http(s) URL, got %q", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("must include a host, got %q", raw)
	}
	return nil
}

func checkRefs(alias string, backends []string, defined map[string]bool) error {
	if len(backends) == 0 {
		return fmt.Errorf("config: alias %q references an empty backend list", alias)
	}
	for _, b := range backends {
		if !defined[b] {
			return fmt.Errorf("config: alias %q references undefined backend %q", alias, b)
		}
	}
	return nil
}

// ----- scalar helper types -----

// Duration is a time.Duration that unmarshals from a YAML string like "10s".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"10s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the standard time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// ByteSize is a byte count that unmarshals from a YAML string ("256MB") or a
// plain integer number of bytes.
type ByteSize int64

func (b *ByteSize) UnmarshalYAML(value *yaml.Node) error {
	// Accept a bare integer (bytes).
	var n int64
	if err := value.Decode(&n); err == nil {
		*b = ByteSize(n)
		return nil
	}
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("max_body_size must be a number or string like \"256MB\": %w", err)
	}
	parsed, err := parseByteSize(s)
	if err != nil {
		return err
	}
	*b = ByteSize(parsed)
	return nil
}

func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	upper := strings.ToUpper(s)
	multipliers := []struct {
		suffix string
		mult   int64
	}{
		{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10},
		{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10},
		{"B", 1},
	}
	for _, m := range multipliers {
		if strings.HasSuffix(upper, m.suffix) {
			num := strings.TrimSpace(upper[:len(upper)-len(m.suffix)])
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid byte size %q: %w", s, err)
			}
			return int64(f * float64(m.mult)), nil
		}
	}
	n, err := strconv.ParseInt(upper, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte size %q", s)
	}
	return n, nil
}
