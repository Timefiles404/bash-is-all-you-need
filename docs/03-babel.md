# 第 03 阶段 — Babel

两个协议，一个 Agent。给它一个 URL、一个 key、一个协议名和一个模型，它就能工作——本地 Ollama 和前沿 API 都是同样的四个字段。

有趣的地方不在于这能做到。而在于**为了做到这一点你必须做什么决定**，因为这两个协议在几乎每一件事上都不同意，每一处分歧都迫使你选一种中立的形式，它既不属于这个协议，也不属于那个协议。

---

## 规则

> **Agent 主循环里绝对不能出现某个供应商的词。**

没有 `tool_calls`，没有 `stop_reason`，没有 `input_tokens`，没有 `chat/completions`。如果一个泄漏进循环，第二个协议就不再是适配器，而是变成了一个 `if` 语句——然后是一百个。

抽象的考验不在于它能不能编译。而在于：**加入第二个协议改变了 Agent 主循环吗？** 对比 `stages/03-babel` 的循环和第 02 阶段的。循环是同一个，词汇被替换了。

```
main.go            speaks Msg, Block, StopReason, Usage — and nothing else
  │
  ├─ provider.go   the neutral language + the Provider interface
  ├─ sse.go        SSE framing. Knows nothing about tokens or tools
  ├─ openai.go     one vendor's opinions, quarantined
  └─ anthropic.go  the other vendor's opinions, quarantined
```

`sse.go` 是值得注意的部分。它是从第 02 阶段切出来的，切点恰好是机制结束、观点开始的地方：**框架是共享的，负载不是。** 一个协议只发 `data:` 行加一个 `[DONE]` 哨兵；另一个发 `event:` + `data:` 没有哨兵。同一个 reader。

---

## 实际上有什么区别

每一行都是观察的结果，不是从规范里念出来的。证据在 [wire-notes.md](wire-notes.md)。

| | OpenAI 协议 | Anthropic 协议 |
|---|---|---|
| **系统提示词** | `messages[0]`, `role:"system"` | 顶层 `system` 字段 |
| **工具定义** | 嵌套在 `{"type":"function","function":{…,"parameters":…}}` | 平铺的 `{"name","description","input_schema"}` |
| **工具调用参数** | 一个 JSON **字符串** | 一个 JSON **对象** |
| **工具结果** | 一个单独的 `role:"tool"` 消息**每个调用一个** | **所有**结果作为块在**一个 `user` 消息**里 |
| **停止理由** | `finish_reason`: `tool_calls`/`stop`/`length` | `stop_reason`: `tool_use`/`end_turn`/`max_tokens` |
| **推理** | `reasoning_content`，同一个 delta 里的兄弟字段 | 一个单独的索引化内容块，带 `thinking_delta` |
| **SSE 框架** | `data:` 只有，`[DONE]` 哨兵，**加上后面的一个帧** | `event:` + `data:`，没有哨兵，`ping` **在** `message_start` **之前**和 `message_stop` **之后** |
| **usage 在哪儿** | 一个 `choices` 数组**为空**的块 | `message_delta` 只有——`message_start` 的数字**是错的**（同一个请求 56 vs 291） |
| **token 记账** | `prompt_tokens` 是总数；`cached_tokens` 嵌套**在它里面** | `input_tokens` 是**已经去掉缓存的剩余部分**；缓存计数器**并排**坐着 |
| **缓存控制** | 仅隐式，64-token 块对齐，每次运行都变 | 显式 `cache_control` 钉住确切的前缀 |

其中两行是设计被做出来的地方。

### 工具结果，以及为什么没有 `RoleTool`

一个协议用自己的消息回答一个工具调用，每个调用一个。另一个把每个结果都收集进一个 `user` 消息。没有中立的角色能表示两个，所以**中立的形式两个都没有**：一个工具结果是一个 *Block*，每个适配器决定什么消息形状运载它。

那是 `Msg` 持有块而不是平坦字符串的完整理由。把任何一个供应商的形状选作"中立"会把其中一个偷偷运进核心——而泄漏只会在第二个协议到达时才可见，那正是最贵的修复时机。

### 工具参数保持原始字节

`Block.Args` 是一个持有原始 JSON 的 `string`，不是一个解码的 `map[string]any`。一个协议想要 JSON 字符串，另一个要 JSON 对象；原始字节是唯一能到达两边而不需要重新序列化的形式。

重新序列化不是免费的：Go 的 map 迭代顺序不稳定，所以一个解码-再编码往返可以为同一个值产生不同的字节——这改变了 prompt 前缀，**使缓存失效**，下一章完全是关于这个的。一个在错误地方的"无害"规范化会在两章后花真实的钱。

### 规范化停止理由，但保留原始的

```go
type CallResult struct {
    Stop    StopReason  // 规范化的：end_turn / tool_use / max_tokens / filtered / unknown
    RawStop string      // 供应商的逐字字符串
}
```

这不是冗余。在这个网关上，一个在 `max_tokens` 处截断的工具调用带着 `stop_reason: "tool_use"` 和一个无用的 body 回来——**信封说谎了**（wire-notes §A3c）。当一个会话出问题了，`Stop` 告诉你 Agent 相信了什么，`RawStop` 告诉你它被告知了什么，两者之间的间隙就是 bug。

**永远不要规范化掉你唯一的证据。** 并注意这个规则的另一半：未知字符串映射到 `StopUnknown`，不是 `StopEndTurn`。一个把任何未认出的东西映射到"可能没问题"的状态机最终会把一个拒绝、一个配额事件或一个新的安全停止映射到"可能没问题"。

---

## 配置

```json
{
  "default": "opencode-oai",
  "providers": {
    "opencode-oai": {
      "protocol": "openai",
      "base_url": "https://opencode.ai/zen/go/v1",
      "api_key_env": "AGENT_API_KEY",
      "model": "mimo-v2.5",
      "window": 131072,
      "prices": { "in": 0.30, "out": 1.20, "cache_read": 0.03 }
    },
    "ollama": {
      "protocol": "openai",
      "base_url": "http://localhost:11434/v1",
      "api_key_env": "OLLAMA_KEY",
      "model": "qwen2.5-coder:7b"
    }
  }
}
```

三个刻意的选择：

- **JSON，不是 TOML。** TOML 要么是一个依赖，要么是一百行教不了关于 Agent 的解析器。丑陋且免费打败优雅且昂贵，在一个声称你能读完所有东西的仓库里。
- **`api_key_env`，永不 `api_key`。** 一个配置文件最终会被提交——每一个都会。唯一可靠的防御是密钥在文件里没有地方坐。
- **env-var 路径仍然有效。** `AGENT_BASE_URL` / `AGENT_API_KEY` / `AGENT_MODEL` 在没有配置文件的情况下运行。配置格式是给你有多个端点的时候用的；让简单情况也必须用上一个，工具就是这样变烦人的。

映射一个协议名到一个实现是 `config.go` 里的十三行，它是仓库里唯一做这个的地方。

---

## 这买来了什么，超越便携性

- **一个 trace 记录事件，不是线上格式。** 一个针对一个协议捕获的会话完全相同地重放——渲染器从来不知道是哪一个。
- **推理的呈现方式两个协议都一样。** 两个完全不同的线上表示，一个 `KindReasoningDelta`。
- **仪表板保持工作**，因为 `Usage` 已经是中立的——这就是为什么它没有一个叫 `prompt_tokens` 的字段。

最后一点值得思量。`Usage` 在第 02 阶段被设计，比第二个协议的出现还早一章，这里不需要任何改变。不是先见之明：它来自于写下*数字的含义*（`Input` = "按完整价格计费"）而不是*API 叫它什么*。描述意义的名字能在第二个实现中存活；从供应商的 JSON 里复制的名字不存活。

---

## 记账反转，再一次

两个协议向相反方向计算，两个适配器都必须规范化进同一个 struct：

```go
// OpenAI: prompt_tokens 是总数，cached_tokens 嵌套在它里面
Input     = prompt_tokens - cached_tokens
CacheRead = cached_tokens

// Anthropic: input_tokens 已经是去掉缓存的剩余部分
Input      = input_tokens
CacheRead  = cache_read_input_tokens
CacheWrite = cache_creation_input_tokens
```

把 OpenAI 这一边弄错——直接复制 `prompt_tokens`——`Prompt()` 会为一个 506-token 提示词报告 698。注意那个 bug 什么时候是看不见的：**错误恰好是缓存命中的大小。** 在一个冷请求上是零。在测试里看起来完美。随着缓存工作得越来越好而逐渐恶化。

---

## 来自真实运行

同一个任务，同一个二进制，同一个循环，在两个协议上：

```sh
echo "count the .py files here" | agent --provider oai --trace oai.jsonl   # openai / mimo-v2.5
echo "count the .py files here" | agent --provider ant --trace ant.jsonl   # anthropic / qwen3.7-plus
```

折叠每个 trace 里的 delta 运行并比较事件种类的序列：

```
oai:  user_message turn_start request first_token reasoning_delta tool_call_start
      tool_args_delta usage response_end tool_call_ready gate_verdict command_start
      command_end tool_result turn_start request first_token reasoning_delta
      text_delta usage response_end turn_end

ant:  user_message turn_start request first_token reasoning_delta tool_call_start
      tool_args_delta usage response_end tool_call_ready gate_verdict command_start
      command_end tool_result turn_start request first_token reasoning_delta
      text_delta usage response_end turn_end
```

**完全一样，逐项。** 现在比较这些事件来自的字节：

```
oai  request keys: max_tokens, messages, model, stream, stream_options, tools
ant  request keys: max_tokens, messages, model, stream, system, tools
```

不同的信封，不同的框架，不同的记账，不同的工具形状——和一个无法分辨的 Agent 循环。那种平等是可交付物；这章里的其他一切，都是为它付出的代价。

Anthropic trace 也可以在没有 key 也没有配置的供应商的情况下重放，因为记录的是事件，不是线上格式。

### 仪表板展示的一个区别

看看从这些运行来的两个仪表板读数：

```
openai    / mimo-v2.5     in 579   full 131 · write 0 · read 448
anthropic / qwen3.7-plus  in 592   full 592 · write 0 · read 0
```

同一个任务，同样大小的对话——一个条形图大多是绿色而另一个完全是红色。第二个协议缓存了**什么都没有**。

那不是适配器里的一个 bug，也不是模型不同。那是这一章缺失的一半：一个协议隐式地缓存，另一个期望被*问到*。修复它是一个请求里的一个字段，它值得一整章，因为围绕那个字段的纪律远比那个字段本身值钱。

→ 那是第 04 阶段。

---

## HTML-转义陷阱

在协调两个适配器时找到的，无论你在构建什么都值得知道。

Go 的 `json.Marshal` 把 `<`、`>` 和 `&` 转义成 `\u003c`、`\u003e` 和 `\u0026`。这是一个浏览器安全默认，它对一个 shell Agent 是实打实的敌意——这三个字符正是 `2>&1`、`>/tmp/out` 和 `<<EOF`：

```
json.Marshal        : {"command":"grep -rn 'x' . 2\u003e\u00261 | head -5 \u003e/tmp/out"}
SetEscapeHTML(false): {"command":"grep -rn 'x' . 2>&1 | head -5 >/tmp/out"}
```

服务器解码它，所以不管哪种写法，模型读到的字符串都一样。两件事仍然让四行 `json.Encoder` 是值得的。请求检查器存在来展示你发送了什么，第一个版本不可读。而转义是否移动一个供应商的缓存 key 取决于它是否散列原始字节或解码内容——我们不知道，那是一个成为*一致*而不是猜测的理由。

一致性是真实的论点：两个适配器最初不同意，两个适配器为同一个对话发出不同的字节——这本身就是个瑕疵，因为这一章讲的正是把这种差异规范化掉。

---

## 练习

1. **在两个协议上运行同一个任务** 并 diff traces。事件应该几乎相同；`request` 事件不会共享任何东西。
2. **指向一个本地 Ollama。** 小模型发出格式不规范的工具调用——那不是你的代码失败，`parseBashArgs` 会告诉你。
3. **尝试把一个供应商词泄漏进循环。** 在 `main.go` 中为一个协议加一个特殊情况，并注意它多快就想要第二个。
4. **加第三个协议。** Google 的是一个合理的练习。数一下你必须接触多少个文件；答案应该是一个，加上 `config.go` 里的十三行。
5. **破坏原始字节规则。** 解码 `Args` 进一个 map 再编码它，然后看第 04 阶段的缓存读列如何崩溃。

→ 下一步：第 04 阶段 — 缓存

→ 参考：[线上注释](wire-notes.md)
