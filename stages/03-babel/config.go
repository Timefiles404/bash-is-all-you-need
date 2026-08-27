// 阶段 03——供应商配置。
//
// 这个文件许下的承诺：把 URL、key、协议和模型交给 Agent，它就能跑。不用厂
// 商 SDK，不用在谁那儿开户，也不用改代码。本地的 Ollama 和前沿 API，就是同
// 样这四个字段。
//
// 格式用 JSON 而不是 TOML，只有一个理由：TOML 要么是一份依赖，要么是一百行
// 关于 Agent 什么也教不了你的解析器。这个仓库的全部主张就是"你能把它整个读
// 完"，那么难看而免费，就胜过优雅而昂贵。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// providerConfig 就是一个端点。
type providerConfig struct {
	Protocol string `json:"protocol"` // "openai" | "anthropic"
	BaseURL  string `json:"base_url"`

	// APIKeyEnv 写的是环境变量名；key 本身故意不做成字段。配置文件迟早会被提交
	// 上去——没有哪份逃得掉——唯一靠得住的防线，就是让密钥在这个文件里压根没
	// 地方待。
	APIKeyEnv string `json:"api_key_env"`

	Model  string      `json:"model"`
	Window int         `json:"window,omitempty"` // 上下文窗口，给水位线用
	Prices priceConfig `json:"prices,omitempty"`
}

// priceConfig 的单位是每百万 token 多少美元。没写就是未知，而未知打印成一
// 条短横，不是 0——见 render.go。
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

// loadProviders 读一份 providers 文件。文件不在不算错：下面那条环境变量的
// 路子照样能走，阶段 00–02 练出来的肌肉记忆也就还管用。
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

// resolve 按名字挑供应商，挑不到就退回文件里的默认项，再退回环境变量。
//
// 留着环境变量这条路，是为了让前面几个阶段那套四个变量的写法原样还能跑。配
// 置格式是给你有好几个端点的时候用的；让最简单的情况也非得配一份，工具就是
// 这么变烦人的。
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

// build 把配置变成活的 Provider。整个仓库里，只有这里把协议名映射到实现
// 上——十三行代码，而 Agent 主循环从来不需要知道自己在跟哪家供应商说话，
// 全靠它。
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
