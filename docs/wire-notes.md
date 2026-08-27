# 线上记录：opencode.ai/zen/go 实证 API 侦察

仅记录观察行为。下面的每一项都有从 2026-08-27 对
`https://opencode.ai/zen/go/v1/...` 线上端点的真实请求中抓到的原始字节作为依据。
这里没有取材于供应商文档的内容。

测试中的端点：

- OpenAI 协议：    `POST https://opencode.ai/zen/go/v1/chat/completions`，模型 `mimo-v2.5`
- Anthropic 协议：`POST https://opencode.ai/zen/go/v1/messages`，模型 `qwen3.7-plus`

从更早的探测中已知（此处未重新验证）：

- OpenAI：`finish_reason:"tool_calls"`、`message.content: null`、一个 `reasoning_content` 字段、
  `usage.prompt_tokens_details.cached_tokens`，以及单个 Assistant 消息中的多个 `tool_calls`。
- Anthropic：`stop_reason:"tool_use"`、一个 `signature` 为空的 `thinking` 块、
  `cache_creation_input_tokens` / `cache_read_input_tokens`。
- 两者：一个非标准的顶层 `"cost"` 字段。

---
## 该端点偏离协议规范的地方

惊喜索引，每一项都在同名的小节中证明。其他一切的表现都符合规范阅读的预期。

| # | 偏差 | 小节 |
|---|---|---|
| 1 | 被截断的 OpenAI 工具调用返回 `tool_calls: []` 并将原始 `<tool_call><function=…>` 宿主标记倒入 `message.content` —— `arguments` 从不部分返回 | A2 |
| 2 | 被截断的 Anthropic `tool_use` 将 `input` 替换为非规范的 `{"raw_arguments": "<invalid JSON>"}` —— 并且 `stop_reason` 仍然说 `"tool_use"` | A3c |
| 3 | Anthropic `max_tokens` 仅限制可见文本；thinking 在其外生成并计费（`max_tokens:10` 返回 `output_tokens:4403`） | A3a |
| 4 | 网关泄露了一个裸 `</think>` 闭合标签，作为用户可见的 `text` 内容块 | A3b, B6 |
| 5 | OpenAI SSE 流在 `data: [DONE]` **之后**发出一帧 —— `cost` 帧，每个符合规范的客户端都会丢弃 | B4 |
| 6 | OpenAI 流式 `usage` 默认存在；`stream_options.include_usage` 被接受并完全是空操作 | B5 |
| 7 | Anthropic `ping` 事件在 `message_start` 之前和 `message_stop` 之后到达，将流括在中间 | B6 |
| 8 | `message_start.usage.input_tokens` 与 `message_delta.usage.input_tokens` 不一致（56 vs 291） —— 只有 `message_delta` 是正确的 | B6 |
| 9 | `cost` 被走私到尾随的 Anthropic `ping` 事件上作为额外的键 | B6 |
| 10 | thinking 块上的 `signature` 总是空字符串，包括 `signature_delta` | B7, A3b |
| 11 | 顶层 `cost` 是 JSON **字符串**，从不是数字；这里总是 `"0"` | C10 |
| 12 | 未知的模型 id 返回 **401 Unauthorized**，不是 404/400 | D11 |
| 13 | 两个协议都返回 Anthropic 错误信封；OpenAI 表面没有 `code`/`param`，而且 `error.type` 是 PascalCase（`ModelError`、`AuthError`） | D11 |
| 14 | 错误体是 JSON 但以 `Content-Type: text/plain;charset=UTF-8` 提供 | D11 |
| 15 | 格式错误的请求 JSON 返回 **500**，不是 400 —— 一个客户端 bug 伪装成可重试的服务器故障 | D11 |
| 16 | 缺少必需字段的 400 根本不返回错误信封，只是 `{"model":"qwen3.7-plus"}` | D11 |
| 17 | `anthropic-version` 不是必需的；调用在没有它的情况下成功 | D11 |
| 18 | `parallel_tool_calls:false` 被接受并忽略；任何虚构的参数也一样。没有任何东西被验证 | D12 |

已确认按文档正常工作：文本截断时的 `finish_reason:"length"` / `stop_reason:"max_tokens"`（A1、A3a）、Anthropic 显式 prompt 缓存（C8）、OpenAI 隐式 prompt 缓存
（C9）、`input_json_delta` 累积（B6），以及两边的推理流式传输（B7）。

---
## A1. OpenAI：在强制长答案的 prompt 上使用 `max_tokens: 10`

请求：

```json
{
  "model": "mimo-v2.5",
  "max_tokens": 10,
  "messages": [
    {"role": "user", "content": "Write a detailed 500-word essay about the history of the Dutch tulip trade. Begin immediately."}
  ]
}
```

原始响应（HTTP 200，逐字，整个响应体）：

```json
{"id":"c275372c-035e-4c22-aa6f-c82cb9b0a1b6_b283ee14d9b7400f8f8618963089641a","object":"chat.completion","created":1787768399,"model":"mimo-v2.5","choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant","content":null,"reasoning_content":"The user wants a detailed 500","tool_calls":null}}],"usage":{"prompt_tokens":269,"completion_tokens":10,"total_tokens":279,"prompt_tokens_details":{"cached_tokens":192},"completion_tokens_details":{"reasoning_tokens":0}},"cost":"0"}
```

**精确的 `finish_reason` 字符串：`"length"`。**

要点：截断得到 `finish_reason:"length"`，在推理模型上预算首先被 `reasoning_content` 消耗 —— 所以截断的回合送到时，`content` 可能是 `null`，
而且*根本没有*用户可见的文本。这里还能看到三个更进一步的怪现象：`cost` 是**字符串** `"0"`（不是数字）、`completion_tokens_details.reasoning_tokens`
是 `0` 而 `reasoning_content` 明显非空，以及 `prompt_tokens` 对于大约 20-token 的用户消息竟是 269（说明网关自己在前面塞了点什么）。

---
## A2. OpenAI：**在工具调用的中间**截断

工具给定：`bash`、对象 schema、必需的字符串属性 `command`。`tool_choice:"required"`。
Prompt 要求一条长的单个 shell 命令。扫过 `max_tokens`。

### 基线：未截断的调用（max_tokens 800）

```json
"finish_reason":"tool_calls",
"message":{"role":"assistant","content":null,"reasoning_content":"...","tool_calls":[
  {"id":"call_9f1de7facb7d47ddb515efb9","type":"function","function":{"name":"bash",
   "arguments":"{\"command\": \"find /srv/app -type f -name '*.go' -mtime -14 -not -path '*/vendor/*' -not -path '*/testdata/*' -exec grep -Hn 'TODO(security)' {} + | sort > /tmp/audit.txt\"}"}}]}
```

`arguments` 是一个包含 JSON 的 JSON **字符串** —— 标准 OpenAI 双编码。

### 被截断的：扫过（`reasoning_effort:"none"` 以便在工具调用上花费预算）

精确的 `message` 对象，逐字：

```
max_tokens=5   "content":"<tool_call>\n<function=b",                       "tool_calls":[]
max_tokens=10  "content":"<tool_call>\n<function=bash>\n<parameter=",       "tool_calls":[]
max_tokens=20  "content":"<tool_call>\n<function=bash>\n<parameter=command>find /srv/app -name \"*.go", "tool_calls":[]
max_tokens=30  "content":"<tool_call>\n<function=bash>\n<parameter=command>find /srv/app -name \"*.go\" -type f -mtime -14 -", "tool_calls":[]
max_tokens=45  "content":"<tool_call>\n<function=bash>\n<parameter=command>find /srv/app -type f -name '*.go' -not -path '*/vendor/*' -not -path '*/testdata/*' -mtime -", "tool_calls":[]
max_tokens=60  "content":"<tool_call>\n<function=bash>\n<parameter=command>find /srv/app -type f -name '*.go' -not -path '*/vendor/*' -not -path '*/testdata/*' -mtime -14 -exec grep -Hn 'TODO(security)' {} +", "tool_calls":[]
max_tokens=70  "content":"<tool_call>\n<function=bash>\n<parameter=command>find /srv/app -type f -name '*.go' -not -path '*/vendor/*' -not -path '*/testdata/*' -mtime -14 -exec grep -Hn 'TODO(security)' {} + 2>/dev/null | sort > /tmp", "tool_calls":[]
```

这些都携带了 `"finish_reason":"length"`、`"reasoning_content":null`、`"tool_calls":[]`。

### 推理留在默认值时效果相同

不是 `reasoning_effort:"none"` 的假象。启用推理，不同的 prompt，一个完整的响应体：

```json
{"choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant",
"content":"<tool_call>\n<function=bash>\n<parameter=command>echo alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango uniform victor whiskey xray yankee z",
"reasoning_content":"The user wants me to call the bash tool with a specific echo command that lists all the letters of the alphabet in the NATO phonetic alphabet format (with some repetitions).\n\nLet me do this exactly as requested.",
"tool_calls":[]}}],
"usage":{"prompt_tokens":550,"completion_tokens":100,"total_tokens":650,...},"cost":"0"}
```

### **关键问题的答案**

**不。`tool_calls[].function.arguments` 从不被截断返回，因为在截断的工具调用上 `tool_calls` 根本没被填充。**它以空数组 `[]` 的形式返回。

实际发生的事情：模型在线上不发出 JSON。它发出一个 XML 式的宿主语法 —— `<tool_call>\n<function=NAME>\n<parameter=NAME>VALUE` —— 网关在服务器端解析为 OpenAI 形状的 `tool_calls`。当生成在语法中间停止时，解析失败，网关**回退到在
`message.content` 中把原始未解析的宿主标记交给你**。截断可以在这段标记的任意位置将其切开：在函数名中间
（5 tokens 时的 `<function=b`）、在参数关键字（10 tokens 时的 `<parameter=`），或在参数值内的任何地方。

另外注意 `tool_calls` 在提供了工具时是 `[]`（空数组），未提供时则是 `null` —— 同一个概念的两个不同空值。

要点：在这个端点上你永远不必修复半写的参数 JSON —— 但你必须处理一个远为棘手的情况。`finish_reason:"length"` 配合 `tool_calls` 为空和
`content` 持有内部 `<tool_call>` 标记意味着将 `content` 渲染给用户的 Agent 会打印网关内部，而只检查 `tool_calls` 的 Agent 会悄无声息地看到一个什么都没做的回合。在查看任一字段**之前**，在 `finish_reason == "length"` 上分支。

---
## A3. Anthropic：同样的两个截断实验

### A3a. 文本截断 —— `max_tokens: 10`

请求：模型 `qwen3.7-plus`、`max_tokens: 10`、相同的 500 字文章 prompt。

响应 HTTP 200。修剪（`thinking` 字符串是约 4000 tokens 的规划散文，此处省略为 `[...]`；它在线上被完整返回）：

```json
{"id":"msg_7b7253e9-8836-45da-9aa8-1fb5d1080acb","type":"message","role":"assistant",
 "stop_reason":"max_tokens","model":"qwen3.7-plus",
 "content":[
   {"type":"thinking","thinking":"Thinking Process:\n\n1.  **Analyze the Request:** [...] Ready.","signature":""},
   {"type":"text","text":"Originating in the rugged mountains of Central Asia, the tul"}],
 "usage":{"input_tokens":32,"output_tokens":4403,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},
 "cost":"0"}
```

**精确的 `stop_reason` 字符串：`"max_tokens"`。** 可见的 `text` 块被切割在单词中间
（`the tul`）。

**`max_tokens: 10` 没有被遵守：`output_tokens` 返回为 4403。** 限制仅应用于可见文本块；整个 `thinking` 块在预算之外生成并计费。这是一个真实的成本陷阱 —— 调用者如果把 `max_tokens: 10` 当作廉价探针来设置，就会被收费大约 4400 个输出 tokens。

响应信封中也没有：`stop_sequence`（标准 Anthropic 总是包括它，
未使用时为 `null`）。`usage` 也没有 `service_tier`。

### A3b. 工具调用基线（未截断，`max_tokens: 700`、`tool_choice:{"type":"any"}`）

```json
{"id":"msg_f5739b8c-9584-4727-81b7-e19585c1b30d","type":"message","role":"assistant",
 "stop_reason":"tool_use","model":"qwen3.7-plus",
 "content":[
   {"type":"thinking","thinking":"","signature":""},
   {"type":"text","text":"\n</think>\n\n"},
   {"type":"tool_use","id":"toolu_2102ceb5b6af4d43a4fa1361","name":"bash","input":{"command":"find /srv/app -name '*.go' -mtime -14 -not -path '*/vendor/*' -not -path '*/testdata/*' -exec grep -n 'TODO(security)' {} + | sort > /tmp/audit.txt"}},
   {"type":"tool_use","id":"toolu_5ae0ccdc34f44d30a2217c5e","name":"bash","input":{"command":"find /srv/app -name '*.go' -not -path '*/vendor/*' -not -path '*/testdata/*' -mtime -14 -exec grep -Hn 'TODO(security)' {} \; | sort > /tmp/audit.txt"}}],
 "usage":{"input_tokens":343,"output_tokens":157,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},
 "cost":"0"}
```

有两件事值得为这一章记一笔。首先，**网关泄露了一个原始 `</think>` 闭合标签作为 `text` 块** —— 一个空的 `thinking` 块，然后 `{"type":"text","text":"\n</think>\n\n"}`。
thinking 提取失败，闭合标签掉进了用户可见的内容中。
其次，**并行 `tool_use` 块也在 Anthropic 端出现**，这里模型
发出了两个几乎相同的 `bash` 调用写入同一文件。

要点：`stop_reason:"max_tokens"`，`max_tokens` 仅限制可见文本 —— thinking
无论如何都生成并计费。永远不要将小 `max_tokens` 视为成本上限。并且永远不要将 `text` 块渲染给用户而不检查它是否不是网关宿主残留。

### A3c. Anthropic：**被截断的** `tool_use` 块的 `input` 看起来像什么

与 A3b 相同的工具/prompt，扫过 `max_tokens`。逐字 `content` 数组：

`max_tokens=15` —— 宿主标记登陆在 `thinking` 中，工具调用被切：

```json
"stop_reason":"tool_use",
"content":[
 {"type":"thinking","thinking":"<tool_call>\n<function=bash>\n<parameter=command>\nfind /srv/app -type f -name '*.go' ! -path '*/vendor/*' ! -path '*/testdata/*' -mtime -14 -exec grep -Hn 'TODO(security)' {} \; | sort > /tmp/audit.txt\n</parameter>\n</function>\n</tool_call>","signature":""},
 {"type":"tool_use","id":"toolu_00752d0dd1854ab0a3d14879","name":"bash","input":{"raw_arguments":"{\"command\": \"find"}}],
"usage":{"input_tokens":343,"output_tokens":100,...}
```

`max_tokens=30` —— 第一次调用完成，第二次被切：

```json
"stop_reason":"tool_use",
"content":[
 {"type":"thinking","thinking":"","signature":""},
 {"type":"text","text":"\n</think>\n\n"},
 {"type":"tool_use","id":"toolu_35fc149f7bc84adca314665c","name":"bash","input":{"command":"find /srv/app -name vendor -prune -o -name testdata -prune -o -name '*.go' -type f -mtime -14 -print | xargs grep -Hn 'TODO(security)' | sort > /tmp/audit.txt"}},
 {"type":"tool_use","id":"toolu_c22da64de987480f802f8618","name":"bash","input":{"raw_arguments":"{\"command\": \"find /srv/app -name '*.go' -not -path '*/vendor"}}]
```

`max_tokens=60` —— 相同的形状，切割得更晚：

```json
 {"type":"tool_use","id":"toolu_bd8b76810dd64528af4daa9a","name":"bash","input":{"raw_arguments":"{\"command\": \"find /srv/app -type f -name '*.go' -not -path '*/vendor/*' -not -path '*/testdata/*' -mtime -14 -exec grep -Hn 'TODO(security)'"}}
```

**答案：在被截断的 `tool_use` 上，`input` 被替换为一个合成的单键对象
`{"raw_arguments": "<the truncated JSON text>"}`。** `raw_arguments` 键不是 Anthropic Messages 规范的一部分。声明的 schema 属性 `command` 根本不存在，尽管 schema 将其标记为必需。截断的 JSON 文本内部是真正无效的 —— 它以未终止的引号结束（`{"command": "find`）。

**并且 `stop_reason` 仍然是 `"tool_use"`，不是 `"max_tokens"`。** 没有信封级别的信号表明任何东西被切。唯一可检测的证据是 `input` 本身的形状。

`max_tokens` 再次不是硬限制。观察到的 `max_tokens` → `output_tokens`：
`5 → 86`（以及一个*完整的*工具调用）、`15 → 100`、`30 → 113`、`60 → 140`、`100 → 157`。
输出持续超出请求的限制大约 57–95 tokens。

要点（stop-reason 章节的重要一条）：在 Anthropic 端，截断的工具调用**不会**通过 `stop_reason` 发出信号。你必须在分派前根据自己的 schema 验证每个 `tool_use.input` —— 特别是，检查 `raw_arguments` 键和缺少的必需属性，并将任一种情况都视为需要重试的截断回合，而不是可执行的工具调用。此处盲目执行 `input["command"]` 会运行一条空或半写的 shell 命令。

---
## B4. OpenAI `"stream": true` —— 原始 SSE 成帧和工具调用分割

响应头：

```
HTTP/1.1 200 OK
Content-Type: text/event-stream
Transfer-Encoding: chunked
Cache-Control: no-cache
Server: cloudflare
```

帧结构，用 `cat -A` 显示以便行尾可见（`$` = LF）。每个帧都是一个
`data:` 行后跟一个空行 —— 标准 SSE：

```
data: {...}$
$
data: {...}$
$
```

**没有 `event:` 行。仅 `data:`。**（由 `grep -c '^event:'` = 0 在整个流中确认。）**有一个 `[DONE]` 哨兵。**

### 一个工具调用的完整流，按顺序

请求：`bash` 工具、`tool_choice:"required"`、`reasoning_effort:"none"`、
prompt"用以下命令调用 bash 工具一次：ls -la /srv/app"。

1. 角色开启 —— 注意 `content` 是 `""`，不是 null：

```json
{"choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"","reasoning_content":null,"tool_calls":null}}]}
```

2. 工具调用开启 —— 这是**唯一**携带 `id` 和 `function.name` 的块：

```json
"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_8d4f0377bc594026a4765cfc","type":"function","function":{"name":"bash","arguments":""}}]}
```

3.–9. 参数片段。`id` 和 `function.name` 现在是 `null`，`index` 保持 `0`，以及
`type` 保持 `"function"`（它*不是*null）。`arguments` 片段按顺序：

```
"{\"command\": "
"\""
"ls"
" -la /srv"
"/app"
"\""
"}"
```

串联：`{"command": "ls -la /srv/app"}`。片段在 token 中间和路径中间分割
（`/srv` + `/app`）—— 它们不是 JSON 对齐的，必须作为原始字节字符串累积。

10. 完成块 —— 空 delta，`finish_reason` 设置：

```json
{"choices":[{"index":0,"finish_reason":"tool_calls","delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null}}]}
```

11. 使用块 —— `choices` 是一个空数组：

```json
{"id":"...","object":"chat.completion.chunk","created":1787768844,"model":"mimo-v2.5","choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":192},"completion_tokens_details":{"reasoning_tokens":0}}}
```

12. `data: [DONE]`

13. **`[DONE]` 之后的帧：**

```
data: {"choices":[],"cost":"0"}
```

要点：按 `index` 累积 `arguments`，并仅从第一个工具调用块锁定 `id`/`name` —— 它们精确出现一次，之后是 `null`。每个字段都显式给出为 `null`，而不是被直接省略，所以"键存在"这件事什么都说明不了；要检查值。并且**流不在 `[DONE]` 结束** —— 一个尾随的 `cost` 帧跟在它后面，每个符合规范的客户端（它在哨兵处停止读取）都会丢弃。

---

## B5. **关键**：OpenAI 流默认包括 `usage` 吗？

**是的。默认情况下存在 `usage`，根本没有发送 `stream_options`。**

没有 `stream_options`（见上面的块 11）：

```json
{"choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":192},"completion_tokens_details":{"reasoning_tokens":0}}}
```

有 `"stream_options": {"include_usage": true}` —— 否则相同的请求：

```json
{"choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":448},"completion_tokens_details":{"reasoning_tokens":0}}}
```

**区别是：没有。** 相同的帧数（两次 13 条 `data:` 行），流中的相同位置，相同的字段。唯一不同的数字是 `cached_tokens`（192 vs 448），它会随每次运行的缓存状态不同而变化，与参数无关。`stream_options` 被接受而不出错（HTTP 200）并且是一个**空操作**。

要点：这里免费送你流式 `usage`，但不要依赖这一点。发送
`stream_options:{"include_usage":true}` 花费为零，是真正 OpenAI 兼容端点需要的，所以无论如何都发送它 —— 并从一个 `choices` 数组**为空**的块中读取 `usage`，因为该帧不携带任何 delta，而且会让假设 `choices[0]` 的解析器崩溃。

---
## B6. Anthropic `"stream": true` —— 每种事件类型，按顺序

头：`Content-Type: text/event-stream`、`Transfer-Encoding: chunked`、`Cache-Control: no-cache`。
成帧是 `event: <name>` + `data: {...}` + 空行 —— 所以这一端**确实**使用 `event:` 行。
**没有 `[DONE]` 哨兵**（`grep -c DONE` = 0）；流在连接关闭时结束。

### 不同的 `event:` 类型，按首次出现的顺序

```
ping
message_start
content_block_start
content_block_delta
content_block_stop
message_delta
message_stop
```

两个工具调用响应的完整事件序列：

```
ping message_start
content_block_start content_block_delta x6 content_block_stop    (index 0, tool_use)
content_block_start content_block_delta   content_block_stop     (index 1, text)
content_block_start content_block_delta x6 content_block_stop    (index 2, tool_use)
message_delta message_stop ping
```

**`ping` 首先到达，在 `message_start` 之前，最后再次到达，在 `message_stop` 之后。** 在
Anthropic 规范中 `message_start` 总是第一个事件，`message_stop` 是最后一个；pings 仅在间隙中作为保活出现。这里它们将整个流括在中间。

### `usage` 出现的位置 —— 两个报告不一致

`message_start`（无缓存字段，信封没有 `stop_reason`/`stop_sequence`）：

```json
{"type":"message_start","message":{"id":"msg_e3f9307e-2dc9-41f0-a70e-cca934593aa0","type":"message","role":"assistant","model":"qwen3.7-plus","content":[],"usage":{"input_tokens":56,"output_tokens":0}}}
```

`message_delta`（携带 `stop_reason` 加一个包括缓存字段的完整使用块）：

```json
{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":291,"output_tokens":63,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}
```

**对于相同请求，`input_tokens` 在 `message_start` 中是 56，在 `message_delta` 中是 291。**
`message_start` 数字是错误的（使用相同 prompt 的非流式调用报告了 291-ish）。
规范将权威 `input_tokens` 放在 `message_start` 中；这里只有 `message_delta` 是
可信的，它也是唯一出现缓存计数器的地方。

### `cost` 藏在哪里

```json
{"type":"ping","cost":"0"}
```

`message_stop` 之后尾随的 `ping` 在 `ping` 事件上作为额外的键携带非标准 `cost` 字段。

### `input_json_delta` 如何携带工具参数

`content_block_start` 以**空的** `input` 对象和真实的 `id`/`name` 宣布块：

```json
{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_ff07c814f3f34014aa526469","name":"bash","input":{}}}
```

然后 `partial_json` 片段，按顺序（注意第一个是空字符串）：

```
""
"{\"command\": \"ls"
" -la /srv"
"/app"
"\""
"}"
```

串联：`{"command": "ls -la /srv/app"}`。然后是带着索引的 `content_block_stop`。
与 OpenAI 端相同的非 JSON 对齐分割，以及相同的 `/srv` + `/app` 分割点 —— 强有力的证据表明两个协议表面都从一个共享的内部 token 流呈现。

注意本流中内容块 `index: 1` 再次是 `</think>` 泄露，作为一个 `text_delta`：

```json
{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"\n</think>\n\n"}}
```

要点：从 `message_delta` 读取 `stop_reason` 和所有 `usage`，永远不要从 `message_start`。
在任何位置（包括在 `message_start` 之前和 `message_stop` 之后）容忍 `ping`，如果你想要 `cost` 字段就不在 `message_stop` 处终止。没有 `[DONE]`。

---
## B7. 任一方是否会流式传输推理/thinking，通过什么字段/事件？

**两者都是。是的。**

### OpenAI 端：`delta.reasoning_content`

Prompt"17 * 23 是多少？仔细思考，然后回答。"、`stream:true`、推理留在
默认值。连续帧，逐字 `delta` 对象：

```json
"delta":{"role":"assistant","content":"","reasoning_content":null,"tool_calls":null}
"delta":{"role":null,"content":null,"reasoning_content":"Okay","tool_calls":null}
"delta":{"role":null,"content":null,"reasoning_content":", the","tool_calls":null}
"delta":{"role":null,"content":null,"reasoning_content":" user is asking for","tool_calls":null}
"delta":{"role":null,"content":null,"reasoning_content":" the product of ","tool_calls":null}
"delta":{"role":null,"content":null,"reasoning_content":"17 and ","tool_calls":null}
```

没有单独的事件或块类型 —— 推理和 `content` 共享**同一个** `delta`
对象，只是位于同级字段 `reasoning_content` 中，仅由两者中哪个非 null 来区分。在这次运行中：44 帧携带了 `reasoning_content`，1 帧携带了 `content`。

### Anthropic 端：`thinking_delta` 在 `thinking` 内容块中

在流中观察到的不同 delta 类型：

```
thinking_delta
signature_delta
text_delta
```

观察到的不同 `content_block` 类型：`thinking`、`text`。

```json
{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}
{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let"}}
...
{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":""}}
{"type":"content_block_stop","index":0}
{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}
{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"To calculate"}}
{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":" 17 ×"}}
```

Thinking 是拥有自己 `index` 的一等公民内容块：先由 `content_block_stop` 关闭，
`text` 块才在下一个索引处开启。

**`signature_delta` 被发出但其 `signature` 是空字符串** —— 帧存在以
满足形状，不携带任何东西。这与非流式
响应中看到的空 `signature` 相匹配。没有加密签名可以往返。

要点：两个协议对推理建模完全不同 —— 同一 delta 上的同级字段（OpenAI）vs 一个有着自己开始/停止和 delta
类型的独立索引内容块（Anthropic）。如果 Anthropic 端代码认为索引 0 处的 `content_block_delta` 是文本，
就会把模型的私有推理渲染给用户。并且因为 `signature` 总是空，
你不能用它来验证或重放这个端点上的 thinking 块。

---
## C8. Anthropic prompt 缓存使用 `cache_control: {"type":"ephemeral"}`

设置：一个系统块，约 9,800 tokens，内容是各种逼真的散文（一个虚构的 Go 工程
手册 —— 472 个不同的句子、7,633 个单词、49,277 个字符，由组合模板生成，所以它是真正各种各样的英文，不是重复的字符）。作为
`system: [{"type":"text","text":"<handbook>","cache_control":{"type":"ephemeral"}}]` 发送，带有平凡的用户回合 `"Reply with exactly the word: ACK"` 和 `max_tokens: 32`。
相同的体被逐个连续 POST 三次。

### 观察到的 `usage`，逐字

```
call 1: "usage":{"input_tokens":18,"output_tokens":249,"cache_creation_input_tokens":9775,"cache_read_input_tokens":0,   "cache_creation":{"ephemeral_5m_input_tokens":9775}}
call 2: "usage":{"input_tokens":18,"output_tokens":236,"cache_creation_input_tokens":0,   "cache_read_input_tokens":9775,"cache_creation":{"ephemeral_5m_input_tokens":0}}
call 3: "usage":{"input_tokens":18,"output_tokens":264,"cache_creation_input_tokens":0,   "cache_read_input_tokens":9775,"cache_creation":{"ephemeral_5m_input_tokens":0}}
```

**缓存真实有效。这些计数器不总是 0。** 在第一次调用时写入，在每个后续调用时读取，精确相同的数字（9,775）双向。

两个结构性细节：有一个额外的嵌套 `cache_creation` 对象，带
`ephemeral_5m_input_tokens`（一个 5 分钟 TTL 桶），并且**`input_tokens` 排除缓存的
前缀** —— 它只报告 18，即仅用户回合。可计费的总输入是
`input_tokens + cache_read_input_tokens`。

注意 `max_tokens: 32` 产生了 249/236/264 输出 tokens，确认 A3a：thinking 块
不受 `max_tokens` 限制。

### 控制：**相同的** prompt，`cache_control` **被移除**

```
no-cache_control call 1: {"input_tokens":1089,"output_tokens":250,"cache_creation_input_tokens":0,"cache_read_input_tokens":8704}
no-cache_control call 2: {"input_tokens":1345,"output_tokens":308,"cache_creation_input_tokens":0,"cache_read_input_tokens":8448}
```

**即使没有 `cache_control` 也会发生缓存** —— 这个端点隐式缓存。但
行为降级：嵌套 `cache_creation` 对象消失，命中是部分的并且
**在其他相同的调用之间变化**（8704，然后 8448），其余的作为未缓存的 `input_tokens` 计费（1089，然后 1345）。两个隐式数字都是 64 的精确倍数
（136x64 和 132x64），而显式 `cache_control` 命中 9,775 不是 —— 所以隐式
缓存在 64-token 块边界上匹配，而 `cache_control` 固定了精确的前缀。

（这些控制调用在显式调用之后运行，所以前缀已经热；那是为什么
控制的第 1 次调用已经显示读取而不是写入。）

要点：缓存是真实的，值得在这里教授。发送 `cache_control` 仍然值得 ——
它把那种部分的、每次运行都会变的 64 块命中，转换成完整、稳定、精确前缀的
命中。把你的缓存命中率算作 `cache_read / (input_tokens + cache_read)`，永远不要
只用 `input_tokens` 来算，它在温暖调用中会把真实输入低报 500 倍。

---

## C9. OpenAI 端：`usage.prompt_tokens_details.cached_tokens` 是否变为非零？

**是的。** 相同的约 9,800-token 手册，作为 `system` 消息发送，相同的体三次：

```
call 1: "usage":{"prompt_tokens":9815,"completion_tokens":2,"total_tokens":9817,"prompt_tokens_details":{"cached_tokens":0},   "completion_tokens_details":{"reasoning_tokens":0}}
call 2: "usage":{"prompt_tokens":9815,"completion_tokens":2,"total_tokens":9817,"prompt_tokens_details":{"cached_tokens":9792},"completion_tokens_details":{"reasoning_tokens":0}}
call 3: "usage":{"prompt_tokens":9815,"completion_tokens":2,"total_tokens":9817,"prompt_tokens_details":{"cached_tokens":9792},"completion_tokens_details":{"reasoning_tokens":0}}
```

所有三个都返回了 `finish_reason:"stop"` 和 `content:"ACK"`。隐式缓存，不需要参数 —— 第一次调用时冷，然后 9,815 中的 9,792 个 prompt tokens 从缓存提供。

**两个协议按相反方向说明缓存的 tokens。** OpenAI：`prompt_tokens`
保持在完整的 9,815，`cached_tokens` 是它的*子集*。Anthropic：`input_tokens`
下降到 18，`cache_read_input_tokens` 是*额外的*添加到它。相同的缓存命中因此
在一个协议上看起来像"输入没有变化"，在另一个上看起来像"输入下降 99.8%"。

`cached_tokens` 在本文档中每次观察都是 64-token 块对齐的：
9792 = 153x64、512 = 8x64、448 = 7x64、192 = 3x64。

要点：隐式缓存默认在两个表面都启用；OpenAI 端根本不需要选择加入。
一个从 OpenAI 端的 `prompt_tokens` 计算成本的仪表板会在每次温暖调用时高估支出，因为它永远不会减去 `cached_tokens`。

---
## C10. `cost` 是否曾非零？它是什么 JSON 类型？

**JSON 类型：`string`。** 在两个协议上通过 `jq '.cost|type'` 确认：

```
OpenAI  large-prompt call: {"cost":"0","cost_type":"string","prompt_tokens":9815}
Anthropic large-prompt call: {"cost":"0","cost_type":"string","out":235,"cread":9775}
```

**从未观察到非零。** 本文档中的每个响应都携带了 `"cost":"0"`，包括
最昂贵的调用：

- 9,815 prompt tokens（OpenAI）-> `"cost":"0"`
- 4,403 输出 tokens（Anthropic，A3a）-> `"cost":"0"`
- 2,000 completion tokens，`finish_reason:"length"` -> `"cost":"0"`
- 9,775-token 缓存写入 -> `"cost":"0"`

跨所有捕获的流式和非流式体去重每个 `cost` 出现
产生精确一个不同的值：`"cost":"0"`。

它是否在计费（非免费）键上非零是**未验证的** —— 这个测试使用了一个
单一的临时键，似乎不被计量，所以这里的零不是字段被硬编码的证据。

要点：`cost` 是一个**字符串**，不是数字。`json.Unmarshal` 到一个 `float64` 字段会
失败出现 `cannot unmarshal string into Go struct field`。如果你根本解码它，解码到
`string`（或 `json.Number`）并解析。在这个键上它总是 `"0"`，所以不要在它上面构建一个预算
守卫 —— 从 token 计数改为推导支出。

---
## D11. 错误的模型 id 和错误的 API 键 —— 精确状态和错误体，两个协议

### 四个必需的情况

```
OpenAI    /v1/chat/completions  bad model  -> HTTP/1.1 401 Unauthorized
{"type":"error","error":{"type":"ModelError","message":"Model gpt-does-not-exist-9000 is not supported"}}

OpenAI    /v1/chat/completions  bad key    -> HTTP/1.1 401 Unauthorized
{"type":"error","error":{"type":"AuthError","message":"Invalid API key."}}

Anthropic /v1/messages          bad model  -> HTTP/1.1 401 Unauthorized
{"type":"error","error":{"type":"ModelError","message":"Model claude-does-not-exist-9000 is not supported"}}

Anthropic /v1/messages          bad key    -> HTTP/1.1 401 Unauthorized
{"type":"error","error":{"type":"AuthError","message":"Invalid API key."}}
```

**未知模型返回 401 Unauthorized**，不是 404 也不是 400。仅靠状态无法
区分"你的键是错的"和"你的模型名字是错的" —— 你必须读 `error.type`。

**两个协议都返回相同的 Anthropic 形状的信封** `{"type":"error","error":{"type","message"}}`。
OpenAI 表面*不*返回 OpenAI 的错误形状：没有 `param` 也没有 `code`
字段，并且 `error.type` 是 PascalCase（`ModelError`、`AuthError`）而非 OpenAI 的
snake_case `invalid_request_error` 或 Anthropic 的 `authentication_error` / `not_found_error`。
一个官方 OpenAI SDK 解码这个会发现其 `code` 和 `param` 字段为空。

### 错误响应头

```
HTTP/1.1 401 Unauthorized
Content-Type: text/plain;charset=UTF-8
Content-Length: 105
Server: cloudflare
```

**`Content-Type: text/plain;charset=UTF-8` 在 JSON 体上。** 一个在解析前在
content-type 上分支的客户端会将错误体视为不透明文本。

### 探测过的其他错误类

```
no auth header at all      -> 401  {"type":"error","error":{"type":"AuthError","message":"Missing API key."}}
malformed JSON body        -> 500  {"type":"error","error":{"type":"error","message":"Internal server error"}}
OpenAI body POSTed to /v1/messages -> 500  {"type":"error","error":{"type":"error","message":"Internal server error"}}
Anthropic call with no `anthropic-version` header -> 200, works normally (header is not required)
Anthropic call with `max_tokens` omitted -> 400, Content-Type: application/json, body is:
    {"model":"qwen3.7-plus"}
```

那里还有两个陷阱。**客户端错误（格式错误的 JSON）被报告为 500**，一个
以"5xx = 瞬态，带回退重试"为键的重试策略会永远重试 —— 它永不成功。
以及对缺失必需字段的 400 返回**根本没错误信封**：响应体是一个
24 字节的回显 `{"model":"qwen3.7-plus"}`，`type`/`error` 缺失，所以
尝试做 `resp.Error.Message` 的错误解析代码得到一个空字符串，无事可记。

要点：永不按 HTTP 状态对这些错误分类。在 429 和连接失败上重试；
将 401 视为致命但模糊的并记录 `error.type`；以及将 5xx 视为*可能*永久的，
限制重试。总是防护一个没有 `error` 字段的错误体。

---

## D12. OpenAI 端是否接受 `"parallel_tool_calls": false`？

**它接受它（HTTP 200）并忽略它。**

请求：`parallel_tool_calls:false`、一个 `bash` 工具、prompt"使用 bash 工具做三
个单独的事情：列出 /a、列出 /b 和列出 /c。"

```json
"finish_reason":"tool_calls",
"tool_calls":[
 {"id":"call_137b32c3c32c4339ab5749f6","type":"function","function":{"name":"bash","arguments":"{\"command\": \"ls /a\"}"}},
 {"id":"call_c5e114b4f267439ea8ee2b7e","type":"function","function":{"name":"bash","arguments":"{\"command\": \"ls /b\"}"}},
 {"id":"call_17775982f2994613a690341f","type":"function","function":{"name":"bash","arguments":"{\"command\": \"ls /c\"}"}}]
```

三个并行工具调用，带 `parallel_tool_calls` 显式 `false`。控制运行，使用
`parallel_tool_calls: true` 产生了相同的结果 —— 3 次调用，相同的参数。

相关探测：一个完全虚构的参数，`"totally_made_up_param_xyz":{"a":1}`，也
返回**HTTP 200**。未知请求参数被静默丢弃，永不被拒绝。

要点：参数被接受但是空操作，所以**你的 Agent 循环必须能够执行一批工具调用，不管你请求什么**。更一般地，这里的 200 不意味着参数起效 —— 这个网关
永不验证请求参数，所以唯一知道是否有效的方式是观察响应，这正是
这些笔记的整个前提。

---
## 出处

上述所有响应体都通过 `curl` 在 2026-08-27 对线上端点捕获并逐字粘贴，有三个刻意的例外，每个在其出现的地方标记：

1. 约 4,000-token `thinking` 字符串在 A3a 中省略为 `[...]`；它在线上被完整返回。
2. 在 A3b/A3c `find -exec` 例子中，shell 终止符显示为 `\;`；在线上它
   是 JSON 转义的 `\;`。没有发现依赖于它。
3. 长的 SSE 捕获显示为它们的 `data:` 有效载荷，其中重复的 `id`/`object`/
   `created`/`model` 信封键在第一次出现后被丢弃。

通过重建所示的请求体并重新 POST 它来重现任何小节。使用的键是
临时的，预计会被撤销。
