# 线上记录：opencode.ai/zen/go 的 API 实测摸底

只写观察到的行为。下面每一条结论背后都压着原始字节——2026-08-27 对
`https://opencode.ai/zen/go/v1/...` 发真实请求抓下来的。这里没有一句话
取自供应商文档。

被测端点：

- OpenAI 协议：`POST https://opencode.ai/zen/go/v1/chat/completions`，
  模型 `mimo-v2.5`
- Anthropic 协议：`POST https://opencode.ai/zen/go/v1/messages`，
  模型 `qwen3.7-plus`

更早的探测已经确定，这一轮没有重测：

- OpenAI：`finish_reason:"tool_calls"`、`message.content: null`、
  `reasoning_content` 字段、`usage.prompt_tokens_details.cached_tokens`，
  以及同一条 assistant 消息里塞进多个 `tool_calls`。
- Anthropic：`stop_reason:"tool_use"`、`signature` 为空的 `thinking` 块、
  `cache_creation_input_tokens` / `cache_read_input_tokens`。
- 两边都有：顶层挂着协议里没有的 `"cost"` 字段。

---
## 这个端点在哪些地方偏离了协议规范

意外清单，每一条都在指名的小节里给出证据。清单以外的部分，照着规范去读
会怎么想，实际就是怎么跑的。

| # | 偏差 | 小节 |
|---|---|---|
| 1 | OpenAI 工具调用被截断时返回 `tool_calls: []`，再把原始的 `<tool_call><function=…>` 宿主标记倒进 `message.content`——`arguments` 永远不会是半截的 | A2 |
| 2 | Anthropic `tool_use` 被截断时，`input` 换成协议里没有的 `{"raw_arguments": "<invalid JSON>"}`——而 `stop_reason` 照旧写 `"tool_use"` | A3c |
| 3 | Anthropic 的 `max_tokens` 只管可见文本；thinking 在预算之外生成，也在预算之外计费（`max_tokens:10` 返回了 `output_tokens:4403`） | A3a |
| 4 | 网关把裸的 `</think>` 闭合标签当成用户可见的 `text` 内容块漏了出来 | A3b, B6 |
| 5 | OpenAI 的 SSE 流在 `data: [DONE]` **之后**还发一帧——那是 `cost` 帧，守规范的客户端全会丢掉它 | B4 |
| 6 | OpenAI 流式 `usage` 默认就有；`stream_options.include_usage` 收下了，然后一点用都没有 | B5 |
| 7 | Anthropic 的 `ping` 事件出现在 `message_start` 之前和 `message_stop` 之后，把整条流夹在中间 | B6 |
| 8 | `message_start.usage.input_tokens` 和 `message_delta.usage.input_tokens` 对不上（56 对 291）——只有 `message_delta` 是对的 | B6 |
| 9 | `cost` 被塞进收尾那个 Anthropic `ping` 事件，充当多出来的键 | B6 |
| 10 | thinking 块上的 `signature` 永远是空字符串，`signature_delta` 也一样 | B7, A3b |
| 11 | 顶层 `cost` 是 JSON **字符串**，从来不是数字；在这里永远是 `"0"` | C10 |
| 12 | 认不出的模型 id 回的是 **401 Unauthorized**，不是 404/400 | D11 |
| 13 | 两个协议都返回 Anthropic 那套错误信封；OpenAI 这一面没有 `code`/`param`，`error.type` 还是 PascalCase（`ModelError`、`AuthError`） | D11 |
| 14 | 错误体是 JSON，却按 `Content-Type: text/plain;charset=UTF-8` 发出来 | D11 |
| 15 | 请求 JSON 写坏了回 **500**，不是 400——客户端的 bug 伪装成可重试的服务端故障 | D11 |
| 16 | 少了必填字段的那个 400 根本没有错误信封，只回一句 `{"model":"qwen3.7-plus"}` | D11 |
| 17 | `anthropic-version` 不是必需的；不带它调用照样成功 | D11 |
| 18 | `parallel_tool_calls:false` 收下了，然后忽略；随便编个参数也一样。什么都不校验 | D12 |

确认和文档一致的部分：文本截断时的 `finish_reason:"length"` /
`stop_reason:"max_tokens"`（A1、A3a）、Anthropic 的显式 prompt 缓存（C8）、
OpenAI 的隐式 prompt 缓存（C9）、`input_json_delta` 的累积（B6），以及两边
的推理流式（B7）。

---
## A1. OpenAI：`max_tokens: 10` 碰上非要长答案的 prompt

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

**`finish_reason` 的确切字符串：`"length"`。**

要点：截断给出的是 `finish_reason:"length"`，而在推理模型上，预算先被
`reasoning_content` 吃掉——所以被截断的回合到手时，`content: null`，用户
能看见的文本*根本没有*。这一帧上还能顺手看出三处怪事：`cost` 是**字符串**
`"0"`，不是数字；`completion_tokens_details.reasoning_tokens` 是 `0`，而
`reasoning_content` 明明不空；`prompt_tokens` 对一条约 20 token 的用户消息
报了 269——网关自己在前面塞了东西。

---
## A2. OpenAI：**截断正好落在工具调用中间**

给的工具：`bash`，object schema，必填的字符串属性 `command`。
`tool_choice:"required"`。Prompt 要的是单独一条很长的 shell 命令。
`max_tokens` 从小到大扫了一遍。

### 基线：没被截断的那次调用（max_tokens 800）

```json
"finish_reason":"tool_calls",
"message":{"role":"assistant","content":null,"reasoning_content":"...","tool_calls":[
  {"id":"call_9f1de7facb7d47ddb515efb9","type":"function","function":{"name":"bash",
   "arguments":"{\"command\": \"find /srv/app -type f -name '*.go' -mtime -14 -not -path '*/vendor/*' -not -path '*/testdata/*' -exec grep -Hn 'TODO(security)' {} + | sort > /tmp/audit.txt\"}"}}]}
```

`arguments` 是装着 JSON 的 JSON **字符串**——OpenAI 标准的双层编码。

### 截断的那批：扫一遍（`reasoning_effort:"none"`，预算全花在工具调用上）

逐字抄下来的 `message` 对象：

```
max_tokens=5   "content":"<tool_call>\n<function=b",                       "tool_calls":[]
max_tokens=10  "content":"<tool_call>\n<function=bash>\n<parameter=",       "tool_calls":[]
max_tokens=20  "content":"<tool_call>\n<function=bash>\n<parameter=command>find /srv/app -name \"*.go", "tool_calls":[]
max_tokens=30  "content":"<tool_call>\n<function=bash>\n<parameter=command>find /srv/app -name \"*.go\" -type f -mtime -14 -", "tool_calls":[]
max_tokens=45  "content":"<tool_call>\n<function=bash>\n<parameter=command>find /srv/app -type f -name '*.go' -not -path '*/vendor/*' -not -path '*/testdata/*' -mtime -", "tool_calls":[]
max_tokens=60  "content":"<tool_call>\n<function=bash>\n<parameter=command>find /srv/app -type f -name '*.go' -not -path '*/vendor/*' -not -path '*/testdata/*' -mtime -14 -exec grep -Hn 'TODO(security)' {} +", "tool_calls":[]
max_tokens=70  "content":"<tool_call>\n<function=bash>\n<parameter=command>find /srv/app -type f -name '*.go' -not -path '*/vendor/*' -not -path '*/testdata/*' -mtime -14 -exec grep -Hn 'TODO(security)' {} + 2>/dev/null | sort > /tmp", "tool_calls":[]
```

每一条都带着 `"finish_reason":"length"`、`"reasoning_content":null`、
`"tool_calls":[]`。

### 推理留在默认值，效果一样

这不是 `reasoning_effort:"none"` 造出来的假象。推理打开、换个 prompt，下面
是完整的响应体：

```json
{"choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant",
"content":"<tool_call>\n<function=bash>\n<parameter=command>echo alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango uniform victor whiskey xray yankee z",
"reasoning_content":"The user wants me to call the bash tool with a specific echo command that lists all the letters of the alphabet in the NATO phonetic alphabet format (with some repetitions).\n\nLet me do this exactly as requested.",
"tool_calls":[]}}],
"usage":{"prompt_tokens":550,"completion_tokens":100,"total_tokens":650,...},"cost":"0"}
```

### **关键问题的答案**

**不会。`tool_calls[].function.arguments` 永远不会以被截断的形态返回，因为
工具调用一旦被截断，`tool_calls` 根本就没填。**它回来的是空数组 `[]`。

真正发生的事情是这样：模型在线上根本不发 JSON。它发的是一套 XML 味的宿主
语法——`<tool_call>\n<function=NAME>\n<parameter=NAME>VALUE`——网关在
服务端把它解析成 OpenAI 形状的 `tool_calls`。生成在语法中间断掉，解析就
失败，于是网关**索性把没解析的原始宿主标记直接塞给你，就放在
`message.content` 里**。截断可以从这段标记的任何位置切下去：函数名中间
（5 token 时的 `<function=b`）、参数关键字上（10 token 时的
`<parameter=`），或者参数值内部的任意一处。

还要注意：给了工具时 `tool_calls` 是 `[]`（空数组），没给工具时是 `null`
——同一件事有两种空值。

要点：在这个端点上你永远不用去修补写了一半的参数 JSON——但你得应付更难看
的情况。`finish_reason:"length"` 加上 `tool_calls` 为空、`content` 里装着
内部的 `<tool_call>` 标记，意味着：把 `content` 直接渲染给用户的 Agent 会
把网关内部打到屏幕上，而只看 `tool_calls` 的 Agent 会以为这一回合什么也
没干，一声不响。要在看这两个字段**之前**，先按
`finish_reason == "length"` 分支。

---
## A3. Anthropic：同样的两个截断实验

### A3a. 文本截断——`max_tokens: 10`

请求：模型 `qwen3.7-plus`、`max_tokens: 10`，还是那个 500 词文章的 prompt。

响应 HTTP 200。下面这段裁过（`thinking` 字符串是约 4000 token 的规划散文，
这里省略成 `[...]`；线上是完整返回的）：

```json
{"id":"msg_7b7253e9-8836-45da-9aa8-1fb5d1080acb","type":"message","role":"assistant",
 "stop_reason":"max_tokens","model":"qwen3.7-plus",
 "content":[
   {"type":"thinking","thinking":"Thinking Process:\n\n1.  **Analyze the Request:** [...] Ready.","signature":""},
   {"type":"text","text":"Originating in the rugged mountains of Central Asia, the tul"}],
 "usage":{"input_tokens":32,"output_tokens":4403,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},
 "cost":"0"}
```

**`stop_reason` 的确切字符串：`"max_tokens"`。** 可见的 `text` 块被切在
单词中间（`the tul`）。

**`max_tokens: 10` 没被遵守：`output_tokens` 回来是 4403。** 这个上限只
作用在可见的文本块上；整个 `thinking` 块在预算之外生成，也在预算之外计费。
这是实实在在的成本陷阱——有人把 `max_tokens: 10` 当便宜的探针来用，结果
被按大约 4400 个输出 token 收了钱。

响应信封里还少了东西：`stop_sequence`（标准 Anthropic 一定带它，没用到时
是 `null`）。`usage` 里也没有 `service_tier`。

### A3b. 工具调用基线：未截断，`max_tokens: 700`、`tool_choice:{"type":"any"}`

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

有两件事值得钉在这一章的墙上。第一，**网关把原始的 `</think>` 闭合标签
当成 `text` 块漏了出来**——先是空的 `thinking` 块，接着是
`{"type":"text","text":"\n</think>\n\n"}`。thinking 抽取失败，闭合标签就
一路掉进了用户可见的内容里。第二，**并行的 `tool_use` 块在 Anthropic 这边
也有**，而这里模型发出的是两条几乎一样的 `bash` 调用，写同一个文件。

要点：`stop_reason:"max_tokens"`，而 `max_tokens` 只管住可见文本——
thinking 照生成、照计费。在这里千万不要把小的 `max_tokens` 当成成本上限。
也千万不要把 `text` 块直接渲染给用户，先确认它不是网关的宿主残渣。

### A3c. Anthropic：**被截断的** `tool_use` 块，`input` 长什么样

工具和 prompt 跟 A3b 一样，扫 `max_tokens`。逐字抄下来的 `content` 数组：

`max_tokens=15`——宿主标记落进了 `thinking`，工具调用被切断：

```json
"stop_reason":"tool_use",
"content":[
 {"type":"thinking","thinking":"<tool_call>\n<function=bash>\n<parameter=command>\nfind /srv/app -type f -name '*.go' ! -path '*/vendor/*' ! -path '*/testdata/*' -mtime -14 -exec grep -Hn 'TODO(security)' {} \; | sort > /tmp/audit.txt\n</parameter>\n</function>\n</tool_call>","signature":""},
 {"type":"tool_use","id":"toolu_00752d0dd1854ab0a3d14879","name":"bash","input":{"raw_arguments":"{\"command\": \"find"}}],
"usage":{"input_tokens":343,"output_tokens":100,...}
```

`max_tokens=30`——第一条调用完整，第二条被切：

```json
"stop_reason":"tool_use",
"content":[
 {"type":"thinking","thinking":"","signature":""},
 {"type":"text","text":"\n</think>\n\n"},
 {"type":"tool_use","id":"toolu_35fc149f7bc84adca314665c","name":"bash","input":{"command":"find /srv/app -name vendor -prune -o -name testdata -prune -o -name '*.go' -type f -mtime -14 -print | xargs grep -Hn 'TODO(security)' | sort > /tmp/audit.txt"}},
 {"type":"tool_use","id":"toolu_c22da64de987480f802f8618","name":"bash","input":{"raw_arguments":"{\"command\": \"find /srv/app -name '*.go' -not -path '*/vendor"}}]
```

`max_tokens=60`——形状一样，只是切在字符串里更后面的位置：

```json
 {"type":"tool_use","id":"toolu_bd8b76810dd64528af4daa9a","name":"bash","input":{"raw_arguments":"{\"command\": \"find /srv/app -type f -name '*.go' -not -path '*/vendor/*' -not -path '*/testdata/*' -mtime -14 -exec grep -Hn 'TODO(security)'"}}
```

**答案：`tool_use` 被截断时，`input` 会被换成合成出来的单键对象
`{"raw_arguments": "<the truncated JSON text>"}`。** `raw_arguments` 这个键
不在 Anthropic Messages 规范里。声明过的 schema 属性 `command` 干脆就没了，
尽管 schema 把它标成必填。里面那段被截断的 JSON 文本是真的不合法——它停在
字符串中间，引号没闭合（`{"command": "find`）。

**而 `stop_reason` 依然是 `"tool_use"`，不是 `"max_tokens"`。** 信封层面
没有任何信号告诉你东西被切掉了。唯一查得到的证据，就是 `input` 自己的形状。

`max_tokens` 在这里同样不是硬上限。实测的 `max_tokens` → `output_tokens`：
`5 → 86`（而且工具调用是*完整的*）、`15 → 100`、`30 → 113`、`60 → 140`、
`100 → 157`。输出稳定地超出请求的上限，大约多 57–95 个 token。

要点（这一条对讲 stop-reason 的那一章最要紧）：在 Anthropic 这边，工具调用
被截断**不会**由 `stop_reason` 告诉你。派发之前，你必须拿自己的 schema 去
校验每一个 `tool_use.input`——具体就是查有没有 `raw_arguments` 键、有没有缺
必填属性，中了任何一条，都按"这一回合被截断、要重试"处理，而不是当成一条
能执行的工具调用。在这里闭着眼执行 `input["command"]`，跑掉的会是空命令，
或者写了一半的 shell 命令。

---
## B4. OpenAI `"stream": true`——原始 SSE 成帧，以及工具调用怎么被切开

响应头：

```
HTTP/1.1 200 OK
Content-Type: text/event-stream
Transfer-Encoding: chunked
Cache-Control: no-cache
Server: cloudflare
```

帧结构，用 `cat -A` 打出来，行尾就看得见了（`$` 就是 LF）。每一帧都是一行
`data:` 后面跟一个空行——标准 SSE：

```
data: {...}$
$
data: {...}$
$
```

**没有 `event:` 行。只有 `data:`。**（整条流上 `grep -c '^event:'` = 0，
确认过了。）**有 `[DONE]` 哨兵。**

### 一次工具调用的完整流，按顺序

请求：`bash` 工具、`tool_choice:"required"`、`reasoning_effort:"none"`，
prompt 是 "Call the bash tool once with command set to: ls -la /srv/app"。

1. 角色开场——注意 `content` 是 `""`，不是 null：

```json
{"choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"","reasoning_content":null,"tool_calls":null}}]}
```

2. 工具调用开场——这是**唯一**带着 `id` 和 `function.name` 的块：

```json
"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_8d4f0377bc594026a4765cfc","type":"function","function":{"name":"bash","arguments":""}}]}
```

3.–9. 参数片段。`id` 和 `function.name` 这时是 `null`，`index` 保持 `0`，
`type` 保持 `"function"`——它*没有*被置空。`arguments` 的片段按顺序是：

```
"{\"command\": "
"\""
"ls"
" -la /srv"
"/app"
"\""
"}"
```

接起来就是 `{"command": "ls -la /srv/app"}`。片段切在 token 中间，也切在
路径中间（`/srv` + `/app`）——它们跟 JSON 边界毫无关系，只能当成原始字节串
一段段攒起来。

10. 收尾块——delta 是空的，`finish_reason` 有值了：

```json
{"choices":[{"index":0,"finish_reason":"tool_calls","delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null}}]}
```

11. usage 块——`choices` 是空数组：

```json
{"id":"...","object":"chat.completion.chunk","created":1787768844,"model":"mimo-v2.5","choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":192},"completion_tokens_details":{"reasoning_tokens":0}}}
```

12. `data: [DONE]`

13. **`[DONE]` 之后还有一帧：**

```
data: {"choices":[],"cost":"0"}
```

要点几条：`arguments` 按 `index` 攒；`id`/`name` 只从第一个工具调用块上
锁下来——它们只出现一次，之后全是 `null`。每个字段都是显式给 `null`，不是
省略，所以"键存在"什么都说明不了，要看值。还有，**这条流并不在 `[DONE]`
处结束**——后面还跟着一帧 `cost`，而守规范的客户端读到哨兵就停，正好把它
丢掉。

---

## B5. **关键**：OpenAI 流默认带 `usage` 吗？

**带。默认就有 `usage`，`stream_options` 一个字都没发。**

不带 `stream_options`（看上面第 11 块）：

```json
{"choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":192},"completion_tokens_details":{"reasoning_tokens":0}}}
```

带上 `"stream_options": {"include_usage": true}`——其余部分完全相同的请求：

```json
{"choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":448},"completion_tokens_details":{"reasoning_tokens":0}}}
```

**区别是：没有区别。** 帧数一样（两次都是 13 行 `data:`），在流里的位置
一样，字段一样。唯一不同的数字是 `cached_tokens`（192 对 448），它随每次
运行的缓存状态变，跟这个参数无关。`stream_options` 照收不误、不报错
（HTTP 200），而它是彻底的**空操作**。

要点：这里的流式 usage 是白送的，但别把代码建在这上面。发
`stream_options:{"include_usage":true}` 不花一分钱，真正 OpenAI 兼容的端点
又确实要它，所以照发——然后从 `choices` 数组**为空**的那个块里读 usage，
因为那一帧不带 delta，会让任何假定 `choices[0]` 存在的解析器直接崩掉。

---
## B6. Anthropic `"stream": true`——每种事件类型，按顺序

头：`Content-Type: text/event-stream`、`Transfer-Encoding: chunked`、
`Cache-Control: no-cache`。成帧方式是 `event: <name>` + `data: {...}` +
空行——所以这一边**确实**用 `event:` 行。**没有 `[DONE]` 哨兵**
（`grep -c DONE` = 0），流靠连接关闭来结束。

### 不同的 `event:` 类型，按第一次出现的顺序

```
ping
message_start
content_block_start
content_block_delta
content_block_stop
message_delta
message_stop
```

一次含两个工具调用的响应，完整的事件序列：

```
ping message_start
content_block_start content_block_delta x6 content_block_stop    (index 0, tool_use)
content_block_start content_block_delta   content_block_stop     (index 1, text)
content_block_start content_block_delta x6 content_block_stop    (index 2, tool_use)
message_delta message_stop ping
```

**`ping` 第一个到，排在 `message_start` 前面；最后又来一个，排在
`message_stop` 后面。** 在 Anthropic 规范里，`message_start` 永远是第一个
事件，`message_stop` 是最后一个；ping 只作为保活出现在中间。这里它们把
整条流夹在了当中。

### usage 出现在哪里——两份报告对不上

`message_start`（没有缓存字段，信封里也没有
`stop_reason`/`stop_sequence`）：

```json
{"type":"message_start","message":{"id":"msg_e3f9307e-2dc9-41f0-a70e-cca934593aa0","type":"message","role":"assistant","model":"qwen3.7-plus","content":[],"usage":{"input_tokens":56,"output_tokens":0}}}
```

`message_delta`（带着 `stop_reason`，加一整块 usage，缓存字段也在里面）：

```json
{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":291,"output_tokens":63,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}
```

**同一个请求，`input_tokens` 在 `message_start` 里是 56，在
`message_delta` 里是 291。** `message_start` 那个数是错的（同样 prompt 的
非流式调用报的是 291 上下）。规范把权威的 `input_tokens` 放在
`message_start`；在这里只有 `message_delta` 可信，而且缓存计数器也只在
它那儿出现。

### `cost` 藏在哪里

```json
{"type":"ping","cost":"0"}
```

`message_stop` 之后那个收尾的 `ping`，把非标准的 `cost` 字段当成 `ping`
事件上多出来的一个键，一并带了过来。

### `input_json_delta` 怎么送工具参数

`content_block_start` 宣布这个块的时候，`input` 对象是**空的**，而
`id`/`name` 是真的：

```json
{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_ff07c814f3f34014aa526469","name":"bash","input":{}}}
```

接着是 `partial_json` 片段，按顺序（注意第一个是空字符串）：

```
""
"{\"command\": \"ls"
" -la /srv"
"/app"
"\""
"}"
```

接起来还是 `{"command": "ls -la /srv/app"}`。然后是带索引的
`content_block_stop`。跟 OpenAI 那边一样不按 JSON 边界切，连 `/srv` +
`/app` 这个切点都一样——这是很硬的证据：两个协议表面是从同一条内部 token
流渲染出来的。

注意这条流里的内容块 `index: 1` 又是那个 `</think>` 泄露，这次以
`text_delta` 的形式出现：

```json
{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"\n</think>\n\n"}}
```

要点：`stop_reason` 和所有 usage 都从 `message_delta` 读，永远不要从
`message_start`。`ping` 出现在任何位置都要容忍，包括 `message_start` 之前
和 `message_stop` 之后；如果你要 `cost` 字段，就别在 `message_stop` 处
收工。这里没有 `[DONE]`。

---
## B7. 两边会不会流式送推理/thinking，走的是哪个字段、哪个事件？

**都会。会送。**

### OpenAI 这边：`delta.reasoning_content`

Prompt 是 "What is 17 * 23? Think it through, then answer."，`stream:true`，
推理留在默认值。连续几帧，逐字抄下来的 `delta` 对象：

```json
"delta":{"role":"assistant","content":"","reasoning_content":null,"tool_calls":null}
"delta":{"role":null,"content":null,"reasoning_content":"Okay","tool_calls":null}
"delta":{"role":null,"content":null,"reasoning_content":", the","tool_calls":null}
"delta":{"role":null,"content":null,"reasoning_content":" user is asking for","tool_calls":null}
"delta":{"role":null,"content":null,"reasoning_content":" the product of ","tool_calls":null}
"delta":{"role":null,"content":null,"reasoning_content":"17 and ","tool_calls":null}
```

没有单独的事件类型，也没有单独的块类型——推理和 `content` 坐在**同一个**
`delta` 对象里，只是换到同级的 `reasoning_content` 字段，区分它们唯一的
办法就是看两者里哪个非 null。这一次运行：44 帧带了 `reasoning_content`，
1 帧带了 `content`。

### Anthropic 这边：`thinking` 内容块里的 `thinking_delta`

整条流上观察到的不同 delta 类型：

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

thinking 是一等公民的内容块，有自己的 `index`，先由 `content_block_stop`
关掉，`text` 块才在下一个索引上开场。

**`signature_delta` 照发，但它的 `signature` 是空字符串**——这一帧存在
只是为了把形状凑齐，里面什么都没有。这和非流式响应里看到的空 `signature`
是一致的。这个端点上没有任何可以往返的加密签名。

要点：两个协议给推理建的模型截然不同——一边是同一个 delta 上的同级字段
（OpenAI），一边是自带 start/stop 和 delta 类型、占一个独立索引的内容块
（Anthropic）。Anthropic 这边的代码要是假定索引 0 上的
`content_block_delta` 是文本，就会把模型的私密推理直接渲染给用户。而因为
`signature` 永远是空的，你在这个端点上没法用它来校验或者重放 thinking 块。

---
## C8. Anthropic 的 prompt 缓存，带 `cache_control: {"type":"ephemeral"}`

准备：约 9,800 token 的 system 块，内容是各式各样、像真的一样的散文（一本
编造出来的 Go 工程手册——472 个不同的句子、7,633 个单词、49,277 个字符，
由组合模板生成，所以它是真正富于变化的英文，不是拿同一个字符堆出来的）。
发送形式是
`system: [{"type":"text","text":"<handbook>","cache_control":{"type":"ephemeral"}}]`
配上无关紧要的用户回合 `"Reply with exactly the word: ACK"`，以及
`max_tokens: 32`。完全相同的请求体连着 POST 了三次。

### 观察到的 `usage`，逐字

```
call 1: "usage":{"input_tokens":18,"output_tokens":249,"cache_creation_input_tokens":9775,"cache_read_input_tokens":0,   "cache_creation":{"ephemeral_5m_input_tokens":9775}}
call 2: "usage":{"input_tokens":18,"output_tokens":236,"cache_creation_input_tokens":0,   "cache_read_input_tokens":9775,"cache_creation":{"ephemeral_5m_input_tokens":0}}
call 3: "usage":{"input_tokens":18,"output_tokens":264,"cache_creation_input_tokens":0,   "cache_read_input_tokens":9775,"cache_creation":{"ephemeral_5m_input_tokens":0}}
```

**缓存确实在工作。这些计数器不总是 0。** 第一次调用写入，之后每次调用
读取，两个方向上的数字一模一样，都是 9,775。

两个结构上的细节：多出一层嵌套的 `cache_creation` 对象，里面装着
`ephemeral_5m_input_tokens`（5 分钟 TTL 的那一档）；而且**`input_tokens`
不含被缓存的前缀**——它报的是 18，只算用户回合。真正要计费的输入总量是
`input_tokens + cache_read_input_tokens`。

注意 `max_tokens: 32` 产出了 249/236/264 个输出 token，正好印证 A3a：
thinking 块不受 `max_tokens` 约束。

### 对照：**同样的** prompt，把 `cache_control` **去掉**

```
no-cache_control call 1: {"input_tokens":1089,"output_tokens":250,"cache_creation_input_tokens":0,"cache_read_input_tokens":8704}
no-cache_control call 2: {"input_tokens":1345,"output_tokens":308,"cache_creation_input_tokens":0,"cache_read_input_tokens":8448}
```

**不带 `cache_control` 也照样缓存**——这个端点是隐式缓存。但行为会退化：
嵌套的 `cache_creation` 对象消失了；命中是部分的，而且**在两次本该完全
相同的调用之间还会变**（先 8704，后 8448）；剩下的部分按未缓存的
`input_tokens` 计费（先 1089，后 1345）。两个隐式数字都是 64 的整倍数
（136x64 和 132x64），而显式 `cache_control` 那次命中的 9,775 不是——所以
隐式缓存是按 64 token 的块边界去匹配的，而 `cache_control` 钉住的是精确
前缀。

（这几次对照调用跑在显式调用之后，前缀已经是热的；所以对照的第 1 次调用
一上来就显示读取，不是写入。）

要点：缓存是真的，值得在这里讲。`cache_control` 依然值得发——它把那种
部分的、每跑一次就变一次的 64 块命中，换成完整、稳定、精确到前缀的命中。
缓存命中率要算成 `cache_read / (input_tokens + cache_read)`，永远不要只拿
`input_tokens` 去算，热调用时它会把真实输入低报 500 倍。

---

## C9. OpenAI 这边：`usage.prompt_tokens_details.cached_tokens` 会非零吗？

**会。** 同一本约 9,800 token 的手册，作为 `system` 消息发送，相同的请求体
发三次：

```
call 1: "usage":{"prompt_tokens":9815,"completion_tokens":2,"total_tokens":9817,"prompt_tokens_details":{"cached_tokens":0},   "completion_tokens_details":{"reasoning_tokens":0}}
call 2: "usage":{"prompt_tokens":9815,"completion_tokens":2,"total_tokens":9817,"prompt_tokens_details":{"cached_tokens":9792},"completion_tokens_details":{"reasoning_tokens":0}}
call 3: "usage":{"prompt_tokens":9815,"completion_tokens":2,"total_tokens":9817,"prompt_tokens_details":{"cached_tokens":9792},"completion_tokens_details":{"reasoning_tokens":0}}
```

三次都返回了 `finish_reason:"stop"` 和 `content:"ACK"`。隐式缓存，不需要
任何参数——第一次调用是冷的，之后 9,815 个 prompt token 里有 9,792 个从
缓存里来。

**两个协议记缓存 token 的方向正好相反。** OpenAI：`prompt_tokens` 保持在
完整的 9,815，`cached_tokens` 是它的*子集*。Anthropic：`input_tokens` 掉到
18，`cache_read_input_tokens` 是*加在它之外*的。于是同一次缓存命中，在一个
协议上看着像"输入毫无变化"，在另一个协议上看着像"输入塌掉了 99.8%"。

本文档里每一次观察，`cached_tokens` 都对齐在 64 token 的块上：
9792 = 153x64、512 = 8x64、448 = 7x64、192 = 3x64。

要点：两个表面默认都开着隐式缓存，OpenAI 这边根本不用主动打开。仪表盘要是
拿 OpenAI 这边的 `prompt_tokens` 去算成本，那它在每一次热调用上都会高估
支出，因为它从来不减掉 `cached_tokens`。

---
## C10. `cost` 有没有非零过？它是什么 JSON 类型？

**JSON 类型：`string`。** 在两个协议上都用 `jq '.cost|type'` 确认过：

```
OpenAI  large-prompt call: {"cost":"0","cost_type":"string","prompt_tokens":9815}
Anthropic large-prompt call: {"cost":"0","cost_type":"string","out":235,"cread":9775}
```

**从没观察到非零。** 本文档里每一个响应都带着 `"cost":"0"`，包括跑过的
最贵的那几次：

- 9,815 个 prompt token（OpenAI）-> `"cost":"0"`
- 4,403 个输出 token（Anthropic，A3a）-> `"cost":"0"`
- 2,000 个 completion token、`finish_reason:"length"` -> `"cost":"0"`
- 9,775 token 的缓存写入 -> `"cost":"0"`

把抓到的所有流式和非流式响应体里每一处 `cost` 拿出来去重，得到的不同取值
恰好只有一个：`"cost":"0"`。

换成真计费（非赠送）的 key 会不会非零，**没有验证过**——这次测试用的只是
一个临时 key，看起来根本没被计量，所以这里的零不能当成"这个字段被写死了"
的证据。

要点：`cost` 是**字符串**，不是数字。`json.Unmarshal` 到 `float64` 字段会
失败，报 `cannot unmarshal string into Go struct field`。真要解码它，就解到
`string`（或者 `json.Number`）再自己 parse。在这个 key 上它永远是 `"0"`，
所以别拿它去搭预算保险——支出从 token 计数里推。

---
## D11. 错的模型 id 和错的 API key——两个协议上的确切状态码和错误体

### 四个必查的情况

```
OpenAI    /v1/chat/completions  模型名错   -> HTTP/1.1 401 Unauthorized
{"type":"error","error":{"type":"ModelError","message":"Model gpt-does-not-exist-9000 is not supported"}}

OpenAI    /v1/chat/completions  key 不对   -> HTTP/1.1 401 Unauthorized
{"type":"error","error":{"type":"AuthError","message":"Invalid API key."}}

Anthropic /v1/messages          模型名错   -> HTTP/1.1 401 Unauthorized
{"type":"error","error":{"type":"ModelError","message":"Model claude-does-not-exist-9000 is not supported"}}

Anthropic /v1/messages          key 不对   -> HTTP/1.1 401 Unauthorized
{"type":"error","error":{"type":"AuthError","message":"Invalid API key."}}
```

**认不出的模型返回 401 Unauthorized**，不是 404，也不是 400。光看状态码
分不清"你的 key 错了"和"你的模型名写错了"——你必须去读 `error.type`。

**两个协议返回的是同一套 Anthropic 形状的信封**
`{"type":"error","error":{"type","message"}}`。OpenAI 这一面*并不*返回
OpenAI 自己的错误形状：没有 `param`，也没有 `code` 字段，而 `error.type`
是 PascalCase（`ModelError`、`AuthError`），既不是 OpenAI 的 snake_case
`invalid_request_error`，也不是 Anthropic 的 `authentication_error` /
`not_found_error`。官方 OpenAI SDK 来解这个东西，会发现自己的 `code` 和
`param` 字段是空的。

### 错误响应头

```
HTTP/1.1 401 Unauthorized
Content-Type: text/plain;charset=UTF-8
Content-Length: 105
Server: cloudflare
```

**JSON 响应体上挂着 `Content-Type: text/plain;charset=UTF-8`。** 解析之前
先按 content-type 分支的客户端，会把这个错误体当成看不懂的纯文本。

### 探过的其他错误类别

```
完全不带鉴权头             -> 401  {"type":"error","error":{"type":"AuthError","message":"Missing API key."}}
请求体是坏的 JSON          -> 500  {"type":"error","error":{"type":"error","message":"Internal server error"}}
把 OpenAI 的请求体 POST 到 /v1/messages -> 500  {"type":"error","error":{"type":"error","message":"Internal server error"}}
Anthropic 调用不带 `anthropic-version` 头 -> 200，一切正常（这个头不是必需的）
Anthropic 调用省掉 `max_tokens` -> 400，Content-Type: application/json，响应体是：
    {"model":"qwen3.7-plus"}
```

这里还有两个陷阱。**客户端自己犯的错（JSON 写坏了）被报成 500**，而任何
按"5xx = 瞬时故障，退避重试"设定的重试策略都会永远重试下去——它根本不可能
成功。另一个是：缺必填字段的那个 400 **完全没有错误信封**，响应体是
24 字节的回显 `{"model":"qwen3.7-plus"}`，`type`/`error` 都不在，于是照着
`resp.Error.Message` 去取的错误解析代码拿到的是空字符串，什么都记不下来。

要点：永远不要按 HTTP 状态码给这些错误分类。只在 429 和连接失败上重试；
401 当成"致命但意思不明"，把 `error.type` 记下来；5xx 当成*可能*是永久
性的，重试次数要设上限。永远要防住那种连 `error` 字段都没有的错误体。

---

## D12. OpenAI 这边收不收 `"parallel_tool_calls": false`？

**它收（HTTP 200），然后无视。**

请求：`parallel_tool_calls:false`，工具只有 `bash` 一个，prompt 是 "Use the
bash tool to do three separate things: list /a, list /b, and list /c."

```json
"finish_reason":"tool_calls",
"tool_calls":[
 {"id":"call_137b32c3c32c4339ab5749f6","type":"function","function":{"name":"bash","arguments":"{\"command\": \"ls /a\"}"}},
 {"id":"call_c5e114b4f267439ea8ee2b7e","type":"function","function":{"name":"bash","arguments":"{\"command\": \"ls /b\"}"}},
 {"id":"call_17775982f2994613a690341f","type":"function","function":{"name":"bash","arguments":"{\"command\": \"ls /c\"}"}}]
```

三个并行的工具调用，而 `parallel_tool_calls` 明确写的是 `false`。把它换成
`parallel_tool_calls: true` 做对照，结果一模一样——3 次调用，参数相同。

顺手探了一下：完全编出来的参数 `"totally_made_up_param_xyz":{"a":1}` 同样
返回 **HTTP 200**。认不出的请求参数会被悄悄丢掉，从来不会被拒绝。

要点：这个参数收下了，但它是空操作，所以**不管你请求什么，你的 Agent 循环
都必须能执行一整批工具调用**。往大一点说：这里的 200 并不代表某个参数生效
了——这个网关从不校验请求参数，所以想知道某样东西到底有没有用，唯一的办法
就是去看响应，而这正是这份记录存在的全部理由。

---
## 出处

上面所有响应体都是 2026-08-27 用 `curl` 打线上端点抓下来、原样粘贴的，只有
三处是刻意的例外，每一处都在出现的地方标注了：

1. A3a 里那段约 4,000 token 的 `thinking` 字符串省略成了 `[...]`；线上是
   完整返回的。
2. A3b/A3c 的 `find -exec` 例子里，shell 终止符写成 `\;`；线上它是 JSON
   转义过的 `\;`。没有任何结论依赖这一点。
3. 长的 SSE 抓包只给出它们的 `data:` 载荷，重复的
   `id`/`object`/`created`/`model` 信封键在第一次出现之后就删掉了。

想复现任何一节，把文中给出的请求体重建出来、再 POST 一次就行。用的那个
key 是临时的，预计已经被吊销。
