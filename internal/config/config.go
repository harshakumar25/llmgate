// Package config loads gateway configuration from a YAML-ish file and the
// environment. To keep the scaffold dependency-free it parses a tiny subset
// of YAML by hand; swap for gopkg.in/yaml.v3 once you add dependencies.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the top-level gateway configuration.
type Config struct {
	Addr       string
	Providers  []Provider
	Timeout    time.Duration // per upstream call; slow local models need more
	Retry      Retry
	Cache      Cache
	RateLimit  RateLimit
	Budget     Budget
	Guardrails Guardrails
	Database   string // DSN, e.g. postgres://...; reserved for a database meter
	Redis      string // addr, e.g. localhost:6379 (used once you wire cache.NewRedis)
	APIKeys    []string

	// ModelFallback lets providers that do not serve a requested model
	// answer with their own, so failover works across vendors.
	ModelFallback bool

	// UsageLog is a JSON-lines file every metered call is appended to and
	// replayed from on start. Empty keeps usage in memory only.
	UsageLog string
}

// Provider describes one upstream model backend.
type Provider struct {
	Name    string // "openai" | "ollama" | "anthropic" | "gemini"
	BaseURL string
	APIKey  string // may reference ${ENV_VAR}
	Model   string
	// Models lists extra model names this provider accepts as-is, besides
	// Model. "*" passes any name through (useful for a local Ollama with
	// many models pulled).
	Models []string
	// Price in USD per million tokens, overriding the built-in table.
	PriceIn, PriceOut float64
}

// Provider returns the first configured provider with this name.
func (c *Config) Provider(name string) (Provider, bool) {
	for _, p := range c.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

// Serves returns every model name this provider accepts as-is.
func (p Provider) Serves() []string {
	return append([]string{p.Model}, p.Models...)
}

// Retry controls the router's failover behaviour.
type Retry struct {
	MaxAttempts int
	Backoff     time.Duration // base delay; doubles per attempt, with jitter
	MaxBackoff  time.Duration // cap on a single delay

	// A provider that fails BreakerFailures times in a row is skipped for
	// BreakerCooldown, then gets one trial call. 0 failures disables it.
	BreakerFailures int
	BreakerCooldown time.Duration
}

// Cache bounds the completion cache.
type Cache struct {
	MaxEntries int
	TTL        time.Duration // 0 disables caching
}

// RateLimit caps requests per API key (or per client IP when the gateway runs
// without keys). PerMinute 0 disables it.
type RateLimit struct {
	PerMinute int
	Burst     int // 0 means PerMinute
}

// Budget caps each API key's spend per UTC day. 0 disables a limit.
type Budget struct {
	USDDaily    float64
	TokensDaily int
}

// Guardrails toggles the inbound safety filters.
type Guardrails struct {
	BlockPromptInjection bool
	RedactPII            bool
	RedactOutput         bool // also scrub PII from non-streamed replies

	// Classifier: a model asked whether each new prompt is an injection
	// attempt, after the regex patterns pass. Off unless Provider is set.
	Classifier Classifier
}

// Classifier configures the model-based injection check.
type Classifier struct {
	Provider   string        // name of a configured provider, e.g. "ollama"
	Model      string        // defaults to that provider's model
	Timeout    time.Duration // default 5s
	FailClosed bool          // block when the classifier errors (default: allow)
	FlagOnly   bool          // only label (X-LLMGate-Guard: flagged), never block
}

// Default returns a config that runs out of the box against a local Ollama.
func Default() *Config {
	return &Config{
		Addr: ":8080",
		Providers: []Provider{
			{Name: "ollama", BaseURL: "http://localhost:11434", Model: "llama3.2", Models: []string{"*"}},
		},
		Timeout: 60 * time.Second,
		Retry: Retry{
			MaxAttempts:     2,
			Backoff:         200 * time.Millisecond,
			MaxBackoff:      2 * time.Second,
			BreakerFailures: 5,
			BreakerCooldown: 30 * time.Second,
		},
		Cache:         Cache{MaxEntries: 10_000, TTL: 5 * time.Minute},
		ModelFallback: true,
		Guardrails:    Guardrails{BlockPromptInjection: true, RedactPII: true},
	}
}

// Load reads config from path (if non-empty) and applies environment overrides.
// A missing path is not an error: defaults are used.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		if err := loadFile(cfg, path); err != nil {
			return nil, err
		}
	}
	applyEnv(cfg)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if len(c.Providers) == 0 {
		return fmt.Errorf("config: at least one provider is required")
	}
	for i, p := range c.Providers {
		if p.Name == "" || p.Model == "" {
			return fmt.Errorf("config: provider %d missing name or model", i)
		}
	}
	if g := c.Guardrails.Classifier.Provider; g != "" {
		if _, ok := c.Provider(g); !ok {
			return fmt.Errorf("config: guard_provider %q is not a configured provider", g)
		}
	}
	return nil
}

// applyEnv lets a few common settings be overridden without a config file,
// which keeps `docker run -e ...` and local dev frictionless.
func applyEnv(c *Config) {
	if v := os.Getenv("LLMGATE_ADDR"); v != "" {
		c.Addr = v
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		c.Database = v
	}
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		c.Redis = v
	}
	if v := os.Getenv("LLMGATE_UPSTREAM_TIMEOUT_S"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Timeout = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("LLMGATE_USAGE_LOG"); v != "" {
		c.UsageLog = v
	}
	if v := os.Getenv("LLMGATE_RATE_LIMIT_RPM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.RateLimit.PerMinute = n
		}
	}
	if v := os.Getenv("LLMGATE_API_KEYS"); v != "" {
		c.APIKeys = splitAndTrim(v, ",")
	}
	// Expand ${ENV} references in provider API keys.
	for i := range c.Providers {
		c.Providers[i].APIKey = os.ExpandEnv(c.Providers[i].APIKey)
	}
}

// loadFile parses a deliberately small subset of YAML: top-level scalars and a
// `providers:` list of `- name: ...` blocks. It is enough for the scaffold and
// has zero dependencies. Replace with a real YAML parser when you need nesting.
func loadFile(c *Config, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close()

	var providers []Provider
	var cur *Provider
	flush := func() {
		if cur != nil {
			providers = append(providers, *cur)
			cur = nil
		}
	}

	sc := bufio.NewScanner(f)
	inProviders := false
	for sc.Scan() {
		raw := sc.Text()
		line := strings.TrimRight(raw, " \t")
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		indented := strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t")
		trimmed := strings.TrimSpace(line)

		if trimmed == "providers:" {
			inProviders = true
			continue
		}
		if !indented && strings.Contains(trimmed, ":") {
			// A new top-level key ends the providers list.
			flush()
			inProviders = false
			key, val := splitKV(trimmed)
			applyTopLevel(c, key, val)
			continue
		}
		if inProviders {
			if strings.HasPrefix(trimmed, "- ") {
				flush()
				cur = &Provider{}
				trimmed = strings.TrimSpace(trimmed[2:])
			}
			if cur != nil && strings.Contains(trimmed, ":") {
				key, val := splitKV(trimmed)
				applyProvider(cur, key, val)
			}
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	if len(providers) > 0 {
		c.Providers = providers
	}
	return nil
}

func applyTopLevel(c *Config, key, val string) {
	switch key {
	case "addr":
		c.Addr = val
	case "database":
		c.Database = val
	case "redis":
		c.Redis = val
	case "api_keys":
		c.APIKeys = splitAndTrim(val, ",")
	case "upstream_timeout_s":
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			c.Timeout = time.Duration(n) * time.Second
		}
	case "max_attempts":
		if n, err := strconv.Atoi(val); err == nil {
			c.Retry.MaxAttempts = n
		}
	case "backoff_ms":
		if n, err := strconv.Atoi(val); err == nil {
			c.Retry.Backoff = time.Duration(n) * time.Millisecond
		}
	case "max_backoff_ms":
		if n, err := strconv.Atoi(val); err == nil {
			c.Retry.MaxBackoff = time.Duration(n) * time.Millisecond
		}
	case "breaker_failures":
		if n, err := strconv.Atoi(val); err == nil {
			c.Retry.BreakerFailures = n
		}
	case "breaker_cooldown_s":
		if n, err := strconv.Atoi(val); err == nil {
			c.Retry.BreakerCooldown = time.Duration(n) * time.Second
		}
	case "cache_max_entries":
		if n, err := strconv.Atoi(val); err == nil {
			c.Cache.MaxEntries = n
		}
	case "cache_ttl_s":
		if n, err := strconv.Atoi(val); err == nil {
			c.Cache.TTL = time.Duration(n) * time.Second
		}
	case "budget_usd_daily":
		c.Budget.USDDaily, _ = strconv.ParseFloat(val, 64)
	case "budget_tokens_daily":
		if n, err := strconv.Atoi(val); err == nil {
			c.Budget.TokensDaily = n
		}
	case "usage_log":
		c.UsageLog = val
	case "rate_limit_rpm":
		if n, err := strconv.Atoi(val); err == nil {
			c.RateLimit.PerMinute = n
		}
	case "rate_limit_burst":
		if n, err := strconv.Atoi(val); err == nil {
			c.RateLimit.Burst = n
		}
	case "model_fallback":
		c.ModelFallback = val == "true"
	case "block_prompt_injection":
		c.Guardrails.BlockPromptInjection = val == "true"
	case "redact_pii":
		c.Guardrails.RedactPII = val == "true"
	case "redact_output":
		c.Guardrails.RedactOutput = val == "true"
	case "guard_provider":
		c.Guardrails.Classifier.Provider = val
	case "guard_model":
		c.Guardrails.Classifier.Model = val
	case "guard_timeout_ms":
		if n, err := strconv.Atoi(val); err == nil {
			c.Guardrails.Classifier.Timeout = time.Duration(n) * time.Millisecond
		}
	case "guard_fail_closed":
		c.Guardrails.Classifier.FailClosed = val == "true"
	case "guard_mode":
		c.Guardrails.Classifier.FlagOnly = val == "flag"
	}
}

func applyProvider(p *Provider, key, val string) {
	switch key {
	case "name":
		p.Name = val
	case "base_url":
		p.BaseURL = val
	case "api_key":
		p.APIKey = val
	case "model":
		p.Model = val
	case "models":
		p.Models = splitAndTrim(val, ",")
	case "price_in":
		p.PriceIn, _ = strconv.ParseFloat(val, 64)
	case "price_out":
		p.PriceOut, _ = strconv.ParseFloat(val, 64)
	}
}

func splitKV(s string) (string, string) {
	i := strings.Index(s, ":")
	if i < 0 {
		return s, ""
	}
	val := stripComment(strings.TrimSpace(s[i+1:]))
	return strings.TrimSpace(s[:i]), strings.Trim(val, `"'`)
}

// stripComment removes a trailing YAML comment: a '#' that starts the value
// or follows whitespace, outside quotes. Without this, the example line
// `api_key: ${OPENAI_API_KEY}   # expanded from the environment` set the key
// to the variable plus the comment, and every OpenAI call failed with 401.
func stripComment(v string) string {
	var quote rune
	for i, c := range v {
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#' && (i == 0 || v[i-1] == ' ' || v[i-1] == '\t'):
			return strings.TrimSpace(v[:i])
		}
	}
	return v
}

func splitAndTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
