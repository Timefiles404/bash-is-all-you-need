// Stage 03 — provider configuration.
//
// The promise this file keeps: give the agent a URL, a key, a protocol and a
// model, and it works. No vendor SDK, no account with anyone in particular, no
// code change. A local Ollama and a frontier API are the same four fields.
//
// The format is JSON rather than TOML for one reason: TOML would be a
// dependency, or a hundred lines of parser that teach nothing about agents.
// Ugly and free beats elegant and costly in a repo whose whole claim is that
// you can read all of it.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// providerConfig is one endpoint.
type providerConfig struct {
	Protocol string `json:"protocol"` // "openai" | "anthropic"
	BaseURL  string `json:"base_url"`

	// APIKeyEnv names an environment variable; the key itself is deliberately
	// not a field. A config file gets committed eventually — every one of them
	// does — and the only reliable defence is for the secret to have nowhere to
	// sit in the file at all.
	APIKeyEnv string `json:"api_key_env"`

	Model  string      `json:"model"`
	Window int         `json:"window,omitempty"` // context window, for the watermark
	Prices priceConfig `json:"prices,omitempty"`
}

// priceConfig is dollars per million tokens. Absent means unknown, and unknown
// prints as a dash rather than as zero — see render.go.
type priceConfig struct {
	In         float64 `json:"in,omitempty"`
	Out        float64 `json:"out,omitempty"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

type providersFile struct {
	Default   string                    `json:"default"`
	Providers map[string]providerConfig `json:"providers"`
}

// loadProviders reads a providers file. A missing file is not an error: the
// env-var path below still works, so stages 00–02 muscle memory keeps working.
func loadProviders(path string) (*providersFile, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &providersFile{Providers: map[string]providerConfig{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var pf providersFile
	if err := json.Unmarshal(raw, &pf); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for name, p := range pf.Providers {
		if p.Protocol != "openai" && p.Protocol != "anthropic" {
			return nil, fmt.Errorf("%s: provider %q has unknown protocol %q (want openai or anthropic)", path, name, p.Protocol)
		}
	}
	return &pf, nil
}

// resolve picks a provider by name, falling back to the file's default and then
// to environment variables.
//
// The env-var path exists so the four-variable form from earlier stages still
// runs unchanged. Config formats are for when you have several endpoints;
// making the simple case require one is how tools become annoying.
func (pf *providersFile) resolve(name string) (providerConfig, string, error) {
	if name == "" {
		name = pf.Default
	}
	if name != "" {
		p, ok := pf.Providers[name]
		if !ok {
			return providerConfig{}, "", fmt.Errorf("no provider named %q (have: %s)", name, strings.Join(providerNames(pf), ", "))
		}
		return p, name, nil
	}

	p := providerConfig{
		Protocol:  envOr("AGENT_PROTOCOL", "openai"),
		BaseURL:   strings.TrimSuffix(os.Getenv("AGENT_BASE_URL"), "/"),
		APIKeyEnv: "AGENT_API_KEY",
		Model:     os.Getenv("AGENT_MODEL"),
	}
	if p.BaseURL == "" || p.Model == "" || os.Getenv("AGENT_API_KEY") == "" {
		return providerConfig{}, "", fmt.Errorf("no provider configured: pass --provider, or set AGENT_BASE_URL, AGENT_API_KEY and AGENT_MODEL")
	}
	return p, "env", nil
}

func providerNames(pf *providersFile) []string {
	out := make([]string, 0, len(pf.Providers))
	for n := range pf.Providers {
		out = append(out, n)
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// build turns a config into a live Provider. This is the only place in the repo
// that maps a protocol name onto an implementation — thirteen lines, and the
// entire reason the agent loop never needs to know which vendor it is talking
// to.
func (c providerConfig) build() (Provider, error) {
	key := os.Getenv(c.APIKeyEnv)
	if key == "" {
		return nil, fmt.Errorf("environment variable %s is empty", c.APIKeyEnv)
	}
	base := strings.TrimSuffix(c.BaseURL, "/")
	switch c.Protocol {
	case "openai":
		return newOpenAIProvider(base, key, c.Model), nil
	case "anthropic":
		return newAnthropicProvider(base, key, c.Model), nil
	default:
		return nil, fmt.Errorf("unknown protocol %q", c.Protocol)
	}
}
