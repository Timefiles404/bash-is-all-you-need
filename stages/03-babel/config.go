// 阶段 03——供应商配置。
//
// 这个文件保持的承诺：给 Agent 一个 URL、一个密钥、
// 一个协议和一个模型，它就能工作。没有厂商 SDK、
// 不需要在任何特定厂商那里开户、没有代码改变。一个本地 Ollama
// 和前沿 API 是相同的四个字段。
//
// 格式选 JSON 而不是 TOML，理由只有一个：TOML 要么是个依赖，
// 要么是一百行教不会你任何 Agent 知识的解析器代码。在这个
// repo 里，丑陋但免费胜过优雅但昂贵——因为这个 repo 唯一的
// 承诺就是你能读完它所有的代码。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// providerConfig 是一个端点。
type providerConfig struct {
	Protocol string `json:"protocol"` // "openai" | "anthropic"
	BaseURL  string `json:"base_url"`

	// APIKeyEnv 命名环境变量；密钥本身故意不是字段。
	// 配置文件最终会被提交——它们都会——唯一可靠的防御
	// 是让密钥在文件里根本无处容身。
	APIKeyEnv string `json:"api_key_env"`

	Model  string      `json:"model"`
	Window int         `json:"window,omitempty"` // 上下文窗口，用于水位线
	Prices priceConfig `json:"prices,omitempty"`
}

// priceConfig 是每百万 token 的美元。缺少意味着未知，
// 未知打印为破折号而不是零——见 render.go。
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

// loadProviders 读取供应商文件。缺少文件不是错误：
// 下面的 env-var 路径仍然有效，所以阶段 00–02 肌肉记忆保持工作。
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

// resolve 按名称挑选供应商，回退到文件的默认值，
// 然后是环境变量。
//
// env-var 路径存在，所以早期阶段的四变量形式仍然
// 原封不动地运行。配置格式是给有多个端点的场景用的；
// 如果连最简单的情况也要用上配置文件，工具就是这样
// 变得让人讨厌的。
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

// build 把配置变成活 Provider。这是 repo 中唯一的地方
// 映射协议名称到实现——十三行，以及 agent 主循环
// 永远不需要知道它在与哪个厂商通话的整个原因。
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
