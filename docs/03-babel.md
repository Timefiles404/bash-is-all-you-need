# 阶段 03：巴别塔

两套协议，一个 Agent。给它 URL、key、协议名、模型这四样，它就跑起来了
——本地的 Ollama 和最前沿的 API，填的是同样四个字段。

有意思的地方不是"这能做到"，而是**为了做到它，你必须替它拿定哪些主意**。
两套协议几乎在每件事上都不一致，而每一处不一致都逼你选一种中立形式，那种
形式两边都不属于。

---

## 规则

> **Agent 主循环里绝对不能出现供应商的词。**

不许有 `tool_calls`，不许有 `stop_reason`，不许有 `input_tokens`，不许有
`chat/completions`。只要漏进去一个，第二套协议就不再是适配器，而是变成一条
`if`——接着就是一百条。

抽象好不好，不看它编不编得过。看这个：**加了第二套协议，Agent 主循环动了
没有？** 把 `stages/03-babel` 的循环和阶段 02 的摆在一起，是同一个循环，
只是换了一套词汇。

```
main.go            只会说 Msg、Block、StopReason、Usage，别的词一个都不会
  │
  ├─ provider.go   中立语言 + Provider 接口
  ├─ sse.go        SSE 分帧。不知道 token 是什么，也不知道工具是什么
  ├─ openai.go     一家供应商的主张，隔离在此
  └─ anthropic.go  另一家供应商的主张，隔离在此
```

真正值得看一眼的是 `sse.go`。它是从阶段 02 切出来的，切口正好落在机制结束、
主张开始的地方：**分帧共用，负载不共用。** 一套协议只发 `data:` 行，末尾
一个 `[DONE]` 哨兵；另一套发 `event:` + `data:`，根本没有哨兵。同一个
reader 全吃得下。

---

## 到底差在哪

下面每一行都是抓出来的，不是从规范上念的。证据在
[wire-notes.md](wire-notes.md)。

| | OpenAI 协议 | Anthropic 协议 |
|---|---|---|
| **系统提示词** | `messages[0]`，`role:"system"` | 顶层 `system` 字段 |
| **工具定义** | 嵌在 `{"type":"function","function":{…,"parameters":…}}` 里 | 平铺的 `{"name","description","input_schema"}` |
| **工具调用参数** | JSON **字符串** | JSON **对象** |
| **工具结果** | 每个调用**各发一条**单独的 `role:"tool"` 消息 | **所有**结果当成块，塞进**同一条 `user` 消息** |
| **停止原因** | `finish_reason`：`tool_calls`/`stop`/`length` | `stop_reason`：`tool_use`/`end_turn`/`max_tokens` |
| **推理** | `reasoning_content`，同一个 delta 里的兄弟字段 | 独立的带索引内容块，靠 `thinking_delta` 送 |
| **SSE 分帧** | 只有 `data:`，`[DONE]` 哨兵，**哨兵之后还有一帧** | `event:` + `data:`，没有哨兵，`ping` 在 `message_start` **之前**和 `message_stop` **之后**各来一个 |
| **usage 在哪** | 在 `choices` 数组**为空**的那个 chunk 里 | 只在 `message_delta` 里——`message_start` 报的数**是错的**（同一个请求，56 对 291） |
| **token 记账** | `prompt_tokens` 是总数，`cached_tokens` 嵌在**它里面** | `input_tokens` 是**扣掉缓存后的余量**，缓存计数器**并排**放在旁边 |
| **缓存控制** | 只有隐式，按 64 token 的块对齐，每次跑都变 | 显式 `cache_control` 把前缀钉死 |

这里面有两行是真正做设计决定的地方。

### 工具结果，以及为什么没有 `RoleTool`

一套协议用自己的消息回答工具调用，一个调用一条。另一套把所有结果收进同一条
`user` 消息。没有哪个中立角色能同时表示这两种，所以**中立形式干脆两个都不
要**：工具结果就是 *Block*，至于拿什么消息形状装它，各自的适配器自己决定。

`Msg` 里装的是块，不是扁平的字符串，理由全在这儿。随便挑一家的形状当
"中立"，等于把那一家偷偷运进了内核——而这条漏子要等第二套协议到场才看得
见，那正好是修起来最贵的时刻。

### 工具参数保持原始字节

`Block.Args` 是 `string`，里面装的是原始 JSON，不是解码好的
`map[string]any`。一套协议要 JSON 字符串，另一套要 JSON 对象；原始字节是
唯一能同时喂给两边、又不必重新序列化的形式。

重新序列化可不是白干的：Go 的 map 遍历顺序不稳定，同一个值走一趟"解码再
编码"，出来的字节就可能变了——prompt 前缀跟着变，**缓存就失效了**，而下一
整章讲的正是这个缓存。一次"无害"的规范化，只要地方放错，两章之后就要拿真
钱来付。

### 停止原因要规范化，但原始那份得留着

```go
type CallResult struct {
    Stop    StopReason  // 规范化之后：end_turn / tool_use / max_tokens / filtered / unknown
    RawStop string      // 供应商原封不动的那个字符串
}
```

这不是冗余。在这个网关上，工具调用要是在 `max_tokens` 处被截断，回来的是
`stop_reason: "tool_use"` 加一段根本没法用的 body——**信封在说谎**
（wire-notes §A3c）。会话出岔子的时候，`Stop` 告诉你 Agent 当时相信了什么，
`RawStop` 告诉你它实际被告知了什么，两者之间那道缝就是 bug。

**别把手上唯一的证据规范化掉。** 这条规则还有另一半，一并记住：认不出的
字符串一律映射到 `StopUnknown`，不是 `StopEndTurn`。一台状态机只要把认不出
的东西都归进"大概没事"，早晚有一天，它会把一次拒答、一次配额事件、或者一
种新出的安全停止，也归进"大概没事"。

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

- **用 JSON，不用 TOML。** TOML 要么带进来一个依赖，要么带进来一百行解析
  器，而那一百行关于 Agent 什么也教不了。这个仓库的招牌是"你能把它整个读
  完"，在这里，丑但免费胜过优雅但昂贵。
- **只写 `api_key_env`，绝不写 `api_key`。** 配置文件早晚会被提交上去——
  没有一个逃得掉。唯一靠得住的防线，是让密钥在这个文件里根本没地方可坐。
- **环境变量那条路照样能走。** `AGENT_BASE_URL` / `AGENT_API_KEY` /
  `AGENT_MODEL` 不用任何配置文件就能跑。配置格式是给手上有好几个端点的人
  用的；非逼着最简单的情况也先写个文件，工具就是这么变烦人的。

把协议名映射到具体实现，是 `config.go` 里的十三行，而全仓库只有这一处干这
件事。

---

## 除了可移植，这还换来了什么

- **trace 记的是事件，不是线上格式。** 在一套协议下抓的会话，重放出来一模
  一样——渲染器从头到尾不知道那是哪一套。
- **推理的呈现两边完全相同。** 两种截然不同的线上表示，进来都是一个
  `KindReasoningDelta`。
- **仪表板照样能用**，因为 `Usage` 本来就是中立的——所以它才没有哪个字段
  叫 `prompt_tokens`。

最后这条值得多待一会儿。`Usage` 是在阶段 02 定下来的，比第二套协议的出现
早整整一章，到这里一个字都不用改。这不是有远见：它只是把*这个数字是什么
意思*写了下来（`Input` = "按全价计费"），而没去抄*API 管它叫什么*。描述含义
的名字能活过第二种实现；从供应商 JSON 里照抄的名字活不过。

---

## 记账反过来了，再说一遍

两套协议是朝相反方向数的，而两个适配器都得规范化成同一个 struct：

```go
// OpenAI：prompt_tokens 是总数，cached_tokens 嵌在它里面
Input     = prompt_tokens - cached_tokens
CacheRead = cached_tokens

// Anthropic：input_tokens 本来就已经是扣掉缓存后的余量
Input      = input_tokens
CacheRead  = cache_read_input_tokens
CacheWrite = cache_creation_input_tokens
```

OpenAI 这边搞错——把 `prompt_tokens` 直接抄过去——`Prompt()` 就会把 506
token 的 prompt 报成 698。注意这个 bug 什么时候是隐形的：**误差的大小恰好
等于缓存命中量。** 冷请求上它是零。测试里它看着完美。而你的缓存做得越好，
它就错得越离谱。

---

## 来自一次真实运行

同一个任务，同一个二进制，同一个主循环，在两套协议上各跑一遍：

```sh
echo "count the .py files here" | agent --provider oai --trace oai.jsonl   # openai / mimo-v2.5
echo "count the .py files here" | agent --provider ant --trace ant.jsonl   # anthropic / qwen3.7-plus
```

把两份 trace 里连续的 delta 折叠掉，再比事件种类的顺序：

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

**一项一项对下来，完全一样。** 现在再比比这些事件是从什么字节里出来的：

```
oai  request keys: max_tokens, messages, model, stream, stream_options, tools
ant  request keys: max_tokens, messages, model, stream, system, tools
```

信封不同，分帧不同，记账不同，工具形状不同——而 Agent 主循环分辨不出来。
这份"分辨不出来"就是交付物；这一章剩下的全部内容，都是它的价钱。

那份 Anthropic 的 trace 还能在没有 key、也没配过任何供应商的机器上重放，
因为录下来的是事件，不是线上格式。

### 仪表板真照出来的一处差别

看看那两次运行的仪表板读数：

```
openai    / mimo-v2.5     in 579   full 131 · write 0 · read 448
anthropic / qwen3.7-plus  in 592   full 592 · write 0 · read 0
```

同一个任务，对话大小也一样——可一条进度条大半是绿的，另一条通体全红。第二
套协议**什么都没缓存**。

这不是适配器的 bug，也不是因为换了模型。这是这一章缺掉的那一半：一套协议
自己隐式地缓存，另一套等着你*开口要*。补上它只要在一个请求里加一个字段，
而这件事值一整章——因为围着这个字段立起来的纪律，远比字段本身值钱。

→ 那就是阶段 04。

---

## HTML 转义这个坑

这是在对齐两个适配器的时候撞上的，不管你在写什么，都值得知道。

Go 的 `json.Marshal` 会把 `<`、`>`、`&` 转义成 `\u003c`、`\u003e`、
`\u0026`。这是替浏览器安全考虑的默认行为，可对跑 shell 的 Agent 来说，
它是实打实的敌意——那三个字符正是 `2>&1`、`>/tmp/out` 和 `<<EOF`：

```
json.Marshal        : {"command":"grep -rn 'x' . 2\u003e\u00261 | head -5 \u003e/tmp/out"}
SetEscapeHTML(false): {"command":"grep -rn 'x' . 2>&1 | head -5 >/tmp/out"}
```

服务端会解码，所以两种写法模型读到的字符串是一样的。但还有两件事，让那四行
`json.Encoder` 值得写。请求检查器存在的意义就是让你看清自己发出去了什么，
而上面那一版没法看。另外，转义会不会挪动供应商的缓存 key，取决于它散列的是
原始字节还是解码后的内容——这一点我们不知道，而不知道恰恰是要求*一致*的
理由，不是拿来猜的理由。

一致性才是真正的论据：这两个适配器一开始就不一致，而同一段对话经两个适配器
发出的字节居然不同，放在一章专讲"把这类差异规范化掉"的文字里，这本身就是
一处刺眼的瑕疵。

---

## 练习

1. **在两套协议上跑同一个任务**，然后 diff 两份 trace。事件应该几乎一致；
   `request` 事件则毫无共同之处。
2. **把它指向本地的 Ollama。** 小模型会发出格式不合法的工具调用——那不是
   你的代码坏了，`parseBashArgs` 会明白地告诉你。
3. **试着往主循环里漏一个供应商的词。** 在 `main.go` 里为某一套协议开个
   特例，然后看它多快就想要第二个。
4. **加第三套协议。** Google 那套就很合适。数数你得动几个文件；答案应该是
   一个，外加 `config.go` 里的十三行。
5. **把原始字节这条规矩破掉。** 把 `Args` 解码成 map 再编码回去，然后看
   阶段 04 里缓存读那一列怎么塌下去。

→ 下一站：[阶段 04：缓存](04-the-cache.md)

→ 参考：[线上记录](wire-notes.md)
