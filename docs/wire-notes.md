# Wire notes: opencode.ai/zen/go empirical API reconnaissance

Observed behaviour only. Every claim below is backed by raw bytes captured from
live requests against `https://opencode.ai/zen/go/v1/...` on 2026-08-27.
Nothing here is taken from vendor documentation.

Endpoints under test:

- OpenAI protocol:    `POST https://opencode.ai/zen/go/v1/chat/completions`, model `mimo-v2.5`
- Anthropic protocol: `POST https://opencode.ai/zen/go/v1/messages`, model `qwen3.7-plus`

Given from earlier probes (not re-verified here):

- OpenAI: `finish_reason:"tool_calls"`, `message.content: null`, a `reasoning_content` field,
  `usage.prompt_tokens_details.cached_tokens`, and multiple `tool_calls` in one assistant message.
- Anthropic: `stop_reason:"tool_use"`, a `thinking` block with an empty `signature`,
  `cache_creation_input_tokens` / `cache_read_input_tokens`.
- Both: a non-standard top-level `"cost"` field.

---
## Where this endpoint deviates from the protocol specs

Index of the surprises, each proven in the section named. Everything else behaved as a
spec-reading would predict.

| # | Deviation | Section |
|---|---|---|
| 1 | Truncated OpenAI tool call returns `tool_calls: []` and dumps raw `<tool_call><function=…>` harness markup into `message.content` — `arguments` is never partial | A2 |
| 2 | Truncated Anthropic `tool_use` replaces `input` with a non-spec `{"raw_arguments": "<invalid JSON>"}` — and `stop_reason` still says `"tool_use"` | A3c |
| 3 | Anthropic `max_tokens` bounds only visible text; thinking is generated and billed outside it (`max_tokens:10` returned `output_tokens:4403`) | A3a |
| 4 | The gateway leaks a bare `</think>` closing tag as a user-visible `text` content block | A3b, B6 |
| 5 | The OpenAI SSE stream emits a frame **after** `data: [DONE]` — the `cost` frame, which every conforming client discards | B4 |
| 6 | OpenAI streaming `usage` is present by default; `stream_options.include_usage` is accepted and is a complete no-op | B5 |
| 7 | Anthropic `ping` events arrive before `message_start` and after `message_stop`, bracketing the stream | B6 |
| 8 | `message_start.usage.input_tokens` disagrees with `message_delta.usage.input_tokens` (56 vs 291) — only `message_delta` is correct | B6 |
| 9 | `cost` is smuggled onto the trailing Anthropic `ping` event as an extra key | B6 |
| 10 | `signature` on thinking blocks is always the empty string, including `signature_delta` | B7, A3b |
| 11 | Top-level `cost` is a JSON **string**, never a number; always `"0"` here | C10 |
| 12 | An unknown model id returns **401 Unauthorized**, not 404/400 | D11 |
| 13 | Both protocols return the Anthropic error envelope; the OpenAI surface has no `code`/`param`, and `error.type` is PascalCase (`ModelError`, `AuthError`) | D11 |
| 14 | Error bodies are JSON but served as `Content-Type: text/plain;charset=UTF-8` | D11 |
| 15 | Malformed request JSON returns **500**, not 400 — a client bug disguised as a retryable server fault | D11 |
| 16 | A 400 for a missing required field returns no error envelope at all, just `{"model":"qwen3.7-plus"}` | D11 |
| 17 | `anthropic-version` is not required; the call succeeds without it | D11 |
| 18 | `parallel_tool_calls:false` is accepted and ignored; so is any invented parameter. Nothing is validated | D12 |

Confirmed working as documented: `finish_reason:"length"` / `stop_reason:"max_tokens"` on text
truncation (A1, A3a), Anthropic explicit prompt caching (C8), OpenAI implicit prompt caching
(C9), `input_json_delta` accumulation (B6), and reasoning streaming on both sides (B7).

---
## A1. OpenAI: `max_tokens: 10` on a prompt forcing a long answer

Request:

```json
{
  "model": "mimo-v2.5",
  "max_tokens": 10,
  "messages": [
    {"role": "user", "content": "Write a detailed 500-word essay about the history of the Dutch tulip trade. Begin immediately."}
  ]
}
```

Raw response (HTTP 200, verbatim, whole body):

```json
{"id":"c275372c-035e-4c22-aa6f-c82cb9b0a1b6_b283ee14d9b7400f8f8618963089641a","object":"chat.completion","created":1787768399,"model":"mimo-v2.5","choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant","content":null,"reasoning_content":"The user wants a detailed 500","tool_calls":null}}],"usage":{"prompt_tokens":269,"completion_tokens":10,"total_tokens":279,"prompt_tokens_details":{"cached_tokens":192},"completion_tokens_details":{"reasoning_tokens":0}},"cost":"0"}
```

**Exact `finish_reason` string: `"length"`.**

Takeaway: truncation yields `finish_reason:"length"`, and on a reasoning model the
budget is consumed by `reasoning_content` first — so a truncated turn can arrive with
`content: null` and *no* user-visible text at all. Note three further oddities visible
here: `cost` is the **string** `"0"` (not a number), `completion_tokens_details.reasoning_tokens`
is `0` while `reasoning_content` is plainly non-empty, and `prompt_tokens` is 269 for a
~20-token user message (the gateway prepends something of its own).

---
## A2. OpenAI: truncation IN THE MIDDLE OF A TOOL CALL

Tool given: `bash`, object schema, required string property `command`. `tool_choice:"required"`.
Prompt asks for a long single shell command. Swept `max_tokens`.

### Baseline: the untruncated call (max_tokens 800)

```json
"finish_reason":"tool_calls",
"message":{"role":"assistant","content":null,"reasoning_content":"...","tool_calls":[
  {"id":"call_9f1de7facb7d47ddb515efb9","type":"function","function":{"name":"bash",
   "arguments":"{\"command\": \"find /srv/app -type f -name '*.go' -mtime -14 -not -path '*/vendor/*' -not -path '*/testdata/*' -exec grep -Hn 'TODO(security)' {} + | sort > /tmp/audit.txt\"}"}}]}
```

`arguments` is a JSON **string** containing JSON — the standard OpenAI double encoding.

### Truncated: the sweep (`reasoning_effort:"none"` to spend the budget on the tool call)

Exact `message` objects, verbatim:

```
max_tokens=5   "content":"<tool_call>\n<function=b",                       "tool_calls":[]
max_tokens=10  "content":"<tool_call>\n<function=bash>\n<parameter=",       "tool_calls":[]
max_tokens=20  "content":"<tool_call>\n<function=bash>\n<parameter=command>find /srv/app -name \"*.go", "tool_calls":[]
max_tokens=30  "content":"<tool_call>\n<function=bash>\n<parameter=command>find /srv/app -name \"*.go\" -type f -mtime -14 -", "tool_calls":[]
max_tokens=45  "content":"<tool_call>\n<function=bash>\n<parameter=command>find /srv/app -type f -name '*.go' -not -path '*/vendor/*' -not -path '*/testdata/*' -mtime -", "tool_calls":[]
max_tokens=60  "content":"<tool_call>\n<function=bash>\n<parameter=command>find /srv/app -type f -name '*.go' -not -path '*/vendor/*' -not -path '*/testdata/*' -mtime -14 -exec grep -Hn 'TODO(security)' {} +", "tool_calls":[]
max_tokens=70  "content":"<tool_call>\n<function=bash>\n<parameter=command>find /srv/app -type f -name '*.go' -not -path '*/vendor/*' -not -path '*/testdata/*' -mtime -14 -exec grep -Hn 'TODO(security)' {} + 2>/dev/null | sort > /tmp", "tool_calls":[]
```

Every one of these carried `"finish_reason":"length"`, `"reasoning_content":null`, `"tool_calls":[]`.

### The same effect with reasoning left at its default

Not an artifact of `reasoning_effort:"none"`. With reasoning on, a different prompt, one full body:

```json
{"choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant",
"content":"<tool_call>\n<function=bash>\n<parameter=command>echo alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango uniform victor whiskey xray yankee z",
"reasoning_content":"The user wants me to call the bash tool with a specific echo command that lists all the letters of the alphabet in the NATO phonetic alphabet format (with some repetitions).\n\nLet me do this exactly as requested.",
"tool_calls":[]}}],
"usage":{"prompt_tokens":550,"completion_tokens":100,"total_tokens":650,...},"cost":"0"}
```

### ANSWER TO THE KEY QUESTION

**No. `tool_calls[].function.arguments` is never returned truncated, because on a truncated
tool call `tool_calls` is not populated at all.** It comes back as the empty array `[]`.

What actually happens: the model does not emit JSON on the wire. It emits an XML-ish harness
syntax — `<tool_call>\n<function=NAME>\n<parameter=NAME>VALUE` — and the gateway parses that
server-side into OpenAI-shaped `tool_calls`. When the generation stops mid-syntax the parse
fails, and the gateway **falls back to handing you the raw un-parsed harness markup in
`message.content`**. Truncation can bisect the markup anywhere: mid function name
(`<function=b` at 5 tokens), at the parameter keyword (`<parameter=` at 10), or anywhere
inside the argument value.

Also note `tool_calls` is `[]` (empty array) when tools were supplied and `null` when they
were not — two different empty values for the same idea.

Takeaway: on this endpoint you will never have to repair half-written argument JSON — but you
must handle a far nastier case. `finish_reason:"length"` with `tool_calls` empty and
`content` holding internal `<tool_call>` markup means an agent that renders `content` to the
user will print gateway internals, and an agent that only checks `tool_calls` will silently
see a turn that did nothing. Branch on `finish_reason == "length"` **before** you look at
either field.

---
## A3. Anthropic: the same two truncation experiments

### A3a. Text truncation — `max_tokens: 10`

Request: model `qwen3.7-plus`, `max_tokens: 10`, same 500-word-essay prompt.

Response HTTP 200. Trimmed (the `thinking` string is ~4000 tokens of planning prose, elided
here as `[...]`; it was returned in full on the wire):

```json
{"id":"msg_7b7253e9-8836-45da-9aa8-1fb5d1080acb","type":"message","role":"assistant",
 "stop_reason":"max_tokens","model":"qwen3.7-plus",
 "content":[
   {"type":"thinking","thinking":"Thinking Process:\n\n1.  **Analyze the Request:** [...] Ready.","signature":""},
   {"type":"text","text":"Originating in the rugged mountains of Central Asia, the tul"}],
 "usage":{"input_tokens":32,"output_tokens":4403,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},
 "cost":"0"}
```

**Exact `stop_reason` string: `"max_tokens"`.** The visible `text` block is cut mid-word
(`the tul`).

**`max_tokens: 10` was not honoured: `output_tokens` came back as 4403.** The limit was applied
only to the visible text block; the entire `thinking` block was generated and billed outside
the budget. This is a real cost trap — a caller who sets `max_tokens: 10` as a cheap probe
gets charged for ~4400 output tokens.

Also absent from the response envelope: `stop_sequence` (standard Anthropic always includes it,
`null` when unused). `usage` has no `service_tier` either.

### A3b. Tool-call baseline (untruncated, `max_tokens: 700`, `tool_choice:{"type":"any"}`)

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

Two things worth pinning up in the chapter. First, **the gateway leaks a raw `</think>` closing
tag as a `text` block** — an empty `thinking` block, then `{"type":"text","text":"\n</think>\n\n"}`.
The thinking extraction failed and the close tag fell through into user-visible content.
Second, **parallel `tool_use` blocks appear on the Anthropic side too**, and here the model
emitted two near-duplicate `bash` calls writing to the same file.

Takeaway: `stop_reason:"max_tokens"`, and `max_tokens` bounds only the visible text — thinking
is generated and billed regardless. Never treat a small `max_tokens` as a cost cap here. And
never render a `text` block to a user without checking it is not gateway harness residue.

### A3c. Anthropic: what a TRUNCATED `tool_use` block's `input` looks like

Same tool/prompt as A3b, sweeping `max_tokens`. Verbatim `content` arrays:

`max_tokens=15` — the harness markup landed in `thinking`, and the tool call is cut:

```json
"stop_reason":"tool_use",
"content":[
 {"type":"thinking","thinking":"<tool_call>\n<function=bash>\n<parameter=command>\nfind /srv/app -type f -name '*.go' ! -path '*/vendor/*' ! -path '*/testdata/*' -mtime -14 -exec grep -Hn 'TODO(security)' {} \; | sort > /tmp/audit.txt\n</parameter>\n</function>\n</tool_call>","signature":""},
 {"type":"tool_use","id":"toolu_00752d0dd1854ab0a3d14879","name":"bash","input":{"raw_arguments":"{\"command\": \"find"}}],
"usage":{"input_tokens":343,"output_tokens":100,...}
```

`max_tokens=30` — first call complete, second one cut:

```json
"stop_reason":"tool_use",
"content":[
 {"type":"thinking","thinking":"","signature":""},
 {"type":"text","text":"\n</think>\n\n"},
 {"type":"tool_use","id":"toolu_35fc149f7bc84adca314665c","name":"bash","input":{"command":"find /srv/app -name vendor -prune -o -name testdata -prune -o -name '*.go' -type f -mtime -14 -print | xargs grep -Hn 'TODO(security)' | sort > /tmp/audit.txt"}},
 {"type":"tool_use","id":"toolu_c22da64de987480f802f8618","name":"bash","input":{"raw_arguments":"{\"command\": \"find /srv/app -name '*.go' -not -path '*/vendor"}}]
```

`max_tokens=60` — same shape, cut later in the string:

```json
 {"type":"tool_use","id":"toolu_bd8b76810dd64528af4daa9a","name":"bash","input":{"raw_arguments":"{\"command\": \"find /srv/app -type f -name '*.go' -not -path '*/vendor/*' -not -path '*/testdata/*' -mtime -14 -exec grep -Hn 'TODO(security)'"}}
```

**ANSWER: on a truncated `tool_use`, `input` is replaced by a synthetic single-key object
`{"raw_arguments": "<the truncated JSON text>"}`.** The `raw_arguments` key is not part of the
Anthropic Messages spec. The declared schema property `command` is simply absent, even though
the schema marks it required. The truncated JSON text inside is genuinely invalid — it ends
mid-string with an unterminated quote (`{"command": "find`).

**And `stop_reason` is still `"tool_use"`, not `"max_tokens"`.** There is no envelope-level
signal that anything was cut. The only detectable evidence is the shape of `input` itself.

`max_tokens` is again not a hard bound. Observed `max_tokens` → `output_tokens`:
`5 → 86` (and a *complete* tool call), `15 → 100`, `30 → 113`, `60 → 140`, `100 → 157`.
Output consistently overruns the requested limit by roughly 57–95 tokens.

Takeaway (the important one for the stop-reason chapter): on the Anthropic side a truncated
tool call is **not** signalled by `stop_reason`. You must validate every `tool_use.input`
against your own schema before dispatch — specifically, check for the `raw_arguments` key and
for missing required properties, and treat either as a truncated turn to retry, not as a tool
call to execute. Executing `input["command"]` blindly here would run an empty or half-written
shell command.

---
## B4. OpenAI `"stream": true` — raw SSE framing and tool-call splitting

Response headers:

```
HTTP/1.1 200 OK
Content-Type: text/event-stream
Transfer-Encoding: chunked
Cache-Control: no-cache
Server: cloudflare
```

Frame structure, shown with `cat -A` so line ends are visible (`$` = LF). Every frame is a
`data:` line followed by one blank line — standard SSE:

```
data: {...}$
$
data: {...}$
$
```

**There are no `event:` lines. Only `data:`.** (Confirmed by `grep -c '^event:'` = 0 across the
whole stream.) **There is a `[DONE]` sentinel.**

### The full stream for one tool call, in order

Request: `bash` tool, `tool_choice:"required"`, `reasoning_effort:"none"`,
prompt "Call the bash tool once with command set to: ls -la /srv/app".

1. Role opener — note `content` is `""`, not null:

```json
{"choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"","reasoning_content":null,"tool_calls":null}}]}
```

2. Tool-call opener — this is the **only** chunk carrying `id` and `function.name`:

```json
"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_8d4f0377bc594026a4765cfc","type":"function","function":{"name":"bash","arguments":""}}]}
```

3.–9. Argument fragments. `id` and `function.name` are now `null`, `index` stays `0`, and
`type` stays `"function"` (it is *not* nulled). The `arguments` fragments in order:

```
"{\"command\": "
"\""
"ls"
" -la /srv"
"/app"
"\""
"}"
```

Concatenated: `{"command": "ls -la /srv/app"}`. Fragments split mid-token and mid-path
(`/srv` + `/app`) — they are not JSON-aligned and must be accumulated as a raw byte string.

10. Finish chunk — empty delta, `finish_reason` set:

```json
{"choices":[{"index":0,"finish_reason":"tool_calls","delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null}}]}
```

11. Usage chunk — `choices` is an empty array:

```json
{"id":"...","object":"chat.completion.chunk","created":1787768844,"model":"mimo-v2.5","choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":192},"completion_tokens_details":{"reasoning_tokens":0}}}
```

12. `data: [DONE]`

13. **A frame AFTER `[DONE]`:**

```
data: {"choices":[],"cost":"0"}
```

Takeaways: accumulate `arguments` by `index`, and latch `id`/`name` from the first tool-call
chunk only — they arrive exactly once and are `null` thereafter. Every field is emitted
explicitly as `null` rather than omitted, so "key present" tells you nothing; test the value.
And **the stream does not end at `[DONE]`** — a trailing `cost` frame follows it, which every
spec-conforming client (which stops reading at the sentinel) will discard.

---

## B5. CRITICAL: does the OpenAI stream include `usage` by default?

**Yes. `usage` is present by default, with no `stream_options` sent at all.**

Without `stream_options` (see chunk 11 above):

```json
{"choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":192},"completion_tokens_details":{"reasoning_tokens":0}}}
```

With `"stream_options": {"include_usage": true}` — identical request otherwise:

```json
{"choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":448},"completion_tokens_details":{"reasoning_tokens":0}}}
```

**The difference is: none.** Same frame count (13 `data:` lines both times), same position in
the stream, same fields. The only differing number is `cached_tokens` (192 vs 448), which
varies run to run with cache state and is unrelated to the parameter. `stream_options` is
accepted without error (HTTP 200) and is a **no-op**.

Takeaway: you get streamed usage for free here, but do not build on that. Sending
`stream_options:{"include_usage":true}` costs nothing and is what a real OpenAI-compatible
endpoint requires, so send it anyway — and read usage from a chunk whose `choices` array is
**empty**, since that frame carries no delta and will crash a parser that assumes `choices[0]`.

---
## B6. Anthropic `"stream": true` — every event type, in order

Headers: `Content-Type: text/event-stream`, `Transfer-Encoding: chunked`, `Cache-Control: no-cache`.
Framing is `event: <name>` + `data: {...}` + blank line — so this side **does** use `event:` lines.
**There is no `[DONE]` sentinel** (`grep -c DONE` = 0); the stream ends when the connection closes.

### Distinct `event:` types, in order of first appearance

```
ping
message_start
content_block_start
content_block_delta
content_block_stop
message_delta
message_stop
```

Full event sequence for a two-tool-call response:

```
ping message_start
content_block_start content_block_delta x6 content_block_stop    (index 0, tool_use)
content_block_start content_block_delta   content_block_stop     (index 1, text)
content_block_start content_block_delta x6 content_block_stop    (index 2, tool_use)
message_delta message_stop ping
```

**`ping` arrives FIRST, before `message_start`, and again LAST, after `message_stop`.** In the
Anthropic spec `message_start` is always the first event and `message_stop` the last; pings only
appear as keepalives in between. Here they bracket the whole stream.

### Where usage appears — and the two reports disagree

`message_start` (no cache fields, and the envelope has no `stop_reason`/`stop_sequence`):

```json
{"type":"message_start","message":{"id":"msg_e3f9307e-2dc9-41f0-a70e-cca934593aa0","type":"message","role":"assistant","model":"qwen3.7-plus","content":[],"usage":{"input_tokens":56,"output_tokens":0}}}
```

`message_delta` (carries `stop_reason` plus a full usage block including cache fields):

```json
{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":291,"output_tokens":63,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}
```

**`input_tokens` is 56 in `message_start` and 291 in `message_delta` — for the same request.**
The `message_start` figure is wrong (the non-streaming call with the same prompt reported 291-ish).
The spec puts authoritative `input_tokens` in `message_start`; here only `message_delta` is
trustworthy, and it is also the only place the cache counters appear.

### Where `cost` hides

```json
{"type":"ping","cost":"0"}
```

The trailing post-`message_stop` `ping` carries the non-standard `cost` field as an extra key on
a `ping` event.

### How `input_json_delta` carries tool arguments

`content_block_start` announces the block with an **empty** `input` object and the real `id`/`name`:

```json
{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_ff07c814f3f34014aa526469","name":"bash","input":{}}}
```

Then `partial_json` fragments, in order (note the first is the empty string):

```
""
"{\"command\": \"ls"
" -la /srv"
"/app"
"\""
"}"
```

Concatenated: `{"command": "ls -la /srv/app"}`. Then `content_block_stop` with the index.
Same non-JSON-aligned splitting as the OpenAI side, and the same `/srv` + `/app` split point —
strong evidence both protocol surfaces are rendered from one shared internal token stream.

Note content block `index: 1` in this stream is the `</think>` leak again, as a `text_delta`:

```json
{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"\n</think>\n\n"}}
```

Takeaway: read `stop_reason` and all usage from `message_delta`, never from `message_start`.
Tolerate `ping` in any position including before `message_start` and after `message_stop`, and
do not terminate on `message_stop` if you want the `cost` field. There is no `[DONE]`.

---
## B7. Does either side stream reasoning/thinking, and via what field/event?

**Both do. Yes.**

### OpenAI side: `delta.reasoning_content`

Prompt "What is 17 * 23? Think it through, then answer.", `stream:true`, reasoning left at
default. Consecutive frames, verbatim `delta` objects:

```json
"delta":{"role":"assistant","content":"","reasoning_content":null,"tool_calls":null}
"delta":{"role":null,"content":null,"reasoning_content":"Okay","tool_calls":null}
"delta":{"role":null,"content":null,"reasoning_content":", the","tool_calls":null}
"delta":{"role":null,"content":null,"reasoning_content":" user is asking for","tool_calls":null}
"delta":{"role":null,"content":null,"reasoning_content":" the product of ","tool_calls":null}
"delta":{"role":null,"content":null,"reasoning_content":"17 and ","tool_calls":null}
```

There is no separate event or block type — reasoning rides in the **same** `delta` object as
`content`, in a sibling field `reasoning_content`, distinguished only by which of the two is
non-null. In this run: 44 frames carried `reasoning_content`, 1 carried `content`.

### Anthropic side: `thinking_delta` inside a `thinking` content block

Distinct delta types observed across the stream:

```
thinking_delta
signature_delta
text_delta
```

Distinct `content_block` types observed: `thinking`, `text`.

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

Thinking is a first-class content block at its own `index`, closed by `content_block_stop`
before the `text` block opens at the next index.

**`signature_delta` is emitted but its `signature` is the empty string** — the frame exists to
satisfy the shape, carrying nothing. This matches the empty `signature` seen in non-streaming
responses. There is no cryptographic signature to round-trip.

Takeaway: the two protocols model reasoning completely differently — a sibling field on the
same delta (OpenAI) versus a separate indexed content block with its own start/stop and delta
type (Anthropic). Anthropic-side code that assumes `content_block_delta` at index 0 is text
will render the model's private reasoning to the user. And because `signature` is always empty,
you cannot use it to verify or replay thinking blocks on this endpoint.

---
## C8. Anthropic prompt caching with `cache_control: {"type":"ephemeral"}`

Setup: a system block of ~9,800 tokens of varied realistic prose (a fabricated Go engineering
handbook — 472 distinct sentences, 7,633 words, 49,277 chars, generated from a combinatorial
template so it is real varied English, not a repeated character). Sent as
`system: [{"type":"text","text":"<handbook>","cache_control":{"type":"ephemeral"}}]` with the
trivial user turn `"Reply with exactly the word: ACK"` and `max_tokens: 32`.
The identical body was POSTed three times back to back.

### Observed `usage`, verbatim

```
call 1: "usage":{"input_tokens":18,"output_tokens":249,"cache_creation_input_tokens":9775,"cache_read_input_tokens":0,   "cache_creation":{"ephemeral_5m_input_tokens":9775}}
call 2: "usage":{"input_tokens":18,"output_tokens":236,"cache_creation_input_tokens":0,   "cache_read_input_tokens":9775,"cache_creation":{"ephemeral_5m_input_tokens":0}}
call 3: "usage":{"input_tokens":18,"output_tokens":264,"cache_creation_input_tokens":0,   "cache_read_input_tokens":9775,"cache_creation":{"ephemeral_5m_input_tokens":0}}
```

**Caching genuinely works. These counters are not always 0.** Write on the first call, read on
every subsequent call, exact same figure (9,775) both directions.

Two structural details: there is an extra nested `cache_creation` object with
`ephemeral_5m_input_tokens` (a 5-minute TTL bucket), and **`input_tokens` excludes the cached
prefix** — it reports 18, the user turn only. Total billable input is
`input_tokens + cache_read_input_tokens`.

Note `max_tokens: 32` produced 249/236/264 output tokens, confirming A3a: the thinking block
is not bounded by `max_tokens`.

### Control: the SAME prompt with `cache_control` REMOVED

```
no-cache_control call 1: {"input_tokens":1089,"output_tokens":250,"cache_creation_input_tokens":0,"cache_read_input_tokens":8704}
no-cache_control call 2: {"input_tokens":1345,"output_tokens":308,"cache_creation_input_tokens":0,"cache_read_input_tokens":8448}
```

**Caching still happens without `cache_control`** — this endpoint caches implicitly. But the
behaviour degrades: the nested `cache_creation` object disappears, the hit is partial and
**varies between otherwise identical calls** (8704, then 8448), and the remainder is billed as
uncached `input_tokens` (1089, then 1345). Both implicit figures are exact multiples of 64
(136x64 and 132x64), while the explicit `cache_control` hit of 9,775 is not — so the implicit
cache matches on 64-token block boundaries whereas `cache_control` pins the exact prefix.

(These control calls ran after the explicit ones, so the prefix was already warm; that is why
call 1 of the control already shows a read rather than a write.)

Takeaway: caching is real and worth teaching here. Sending `cache_control` is still worth it —
it converts a partial, run-to-run-variable 64-block hit into a complete, stable, exact-prefix
hit. Compute your cache hit rate as `cache_read / (input_tokens + cache_read)`, never from
`input_tokens` alone, which under-reports true input by 500x on a warm call.

---

## C9. OpenAI side: does `usage.prompt_tokens_details.cached_tokens` become non-zero?

**Yes.** Same ~9,800-token handbook, sent as a `system` message, identical body three times:

```
call 1: "usage":{"prompt_tokens":9815,"completion_tokens":2,"total_tokens":9817,"prompt_tokens_details":{"cached_tokens":0},   "completion_tokens_details":{"reasoning_tokens":0}}
call 2: "usage":{"prompt_tokens":9815,"completion_tokens":2,"total_tokens":9817,"prompt_tokens_details":{"cached_tokens":9792},"completion_tokens_details":{"reasoning_tokens":0}}
call 3: "usage":{"prompt_tokens":9815,"completion_tokens":2,"total_tokens":9817,"prompt_tokens_details":{"cached_tokens":9792},"completion_tokens_details":{"reasoning_tokens":0}}
```

All three returned `finish_reason:"stop"` and `content:"ACK"`. Implicit caching, no parameter
required — cold on the first call, then 9,792 of 9,815 prompt tokens served from cache.

**The two protocols account for cached tokens in opposite directions.** OpenAI: `prompt_tokens`
stays at the full 9,815 and `cached_tokens` is a *subset* of it. Anthropic: `input_tokens`
drops to 18 and `cache_read_input_tokens` is *additional* to it. The same cache hit therefore
looks like "no change in input" on one protocol and "input collapsed by 99.8%" on the other.

`cached_tokens` is 64-token block aligned across every observation in this document:
9792 = 153x64, 512 = 8x64, 448 = 7x64, 192 = 3x64.

Takeaway: implicit caching is on by default on both surfaces; the OpenAI side needs no opt-in
at all. A dashboard that computes cost from `prompt_tokens` on the OpenAI side will overstate
spend on every warm call, because it never subtracts `cached_tokens`.

---
## C10. Is `cost` ever non-zero? What JSON type is it?

**JSON type: `string`.** Confirmed with `jq '.cost|type'` on both protocols:

```
OpenAI  large-prompt call: {"cost":"0","cost_type":"string","prompt_tokens":9815}
Anthropic large-prompt call: {"cost":"0","cost_type":"string","out":235,"cread":9775}
```

**Never observed non-zero.** Every response in this document carried `"cost":"0"`, including
the most expensive calls made:

- 9,815 prompt tokens (OpenAI) -> `"cost":"0"`
- 4,403 output tokens (Anthropic, A3a) -> `"cost":"0"`
- 2,000 completion tokens, `finish_reason:"length"` -> `"cost":"0"`
- 9,775-token cache write -> `"cost":"0"`

Deduplicating every `cost` occurrence across all captured streaming and non-streaming bodies
yields exactly one distinct value: `"cost":"0"`.

Whether it is non-zero on a billed (non-complimentary) key is **unverified** — this test used a
single temporary key that appears not to be metered, so a zero here is not evidence that the
field is hardcoded.

Takeaway: `cost` is a **string**, not a number. `json.Unmarshal` into a `float64` field will
fail with `cannot unmarshal string into Go struct field`. If you decode it at all, decode into
`string` (or `json.Number`) and parse. On this key it is always `"0"`, so do not build a budget
guard on it — derive spend from the token counts instead.

---
## D11. Bad model id and bad API key — exact status and error body, both protocols

### The four required cases

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

**An unknown model returns 401 Unauthorized**, not 404 and not 400. Status alone cannot
distinguish "your key is wrong" from "your model name is wrong" — you must read `error.type`.

**Both protocols return the same Anthropic-shaped envelope** `{"type":"error","error":{"type","message"}}`.
The OpenAI surface does *not* return OpenAI's error shape: there is no `param` and no `code`
field, and `error.type` is PascalCase (`ModelError`, `AuthError`) rather than OpenAI's
snake_case `invalid_request_error` or Anthropic's `authentication_error` / `not_found_error`.
An official OpenAI SDK decoding this will find its `code` and `param` fields empty.

### Error response headers

```
HTTP/1.1 401 Unauthorized
Content-Type: text/plain;charset=UTF-8
Content-Length: 105
Server: cloudflare
```

**`Content-Type: text/plain;charset=UTF-8` on a JSON body.** A client that branches on
content-type before parsing will treat the error body as opaque text.

### Other error classes probed

```
no auth header at all      -> 401  {"type":"error","error":{"type":"AuthError","message":"Missing API key."}}
malformed JSON body        -> 500  {"type":"error","error":{"type":"error","message":"Internal server error"}}
OpenAI body POSTed to /v1/messages -> 500  {"type":"error","error":{"type":"error","message":"Internal server error"}}
Anthropic call with no `anthropic-version` header -> 200, works normally (header is not required)
Anthropic call with `max_tokens` omitted -> 400, Content-Type: application/json, body is:
    {"model":"qwen3.7-plus"}
```

Two more traps there. **A client mistake (malformed JSON) is reported as 500**, which a retry
policy keyed on "5xx = transient, retry with backoff" will retry forever — it can never succeed.
And the 400 for a missing required field returns **no error envelope at all**: the body is a
24-byte echo `{"model":"qwen3.7-plus"}` with `type`/`error` absent, so error-parsing code that
does `resp.Error.Message` gets an empty string with nothing to log.

Takeaway: never classify these errors by HTTP status. Retry on 429 and on connection failures;
treat 401 as fatal-but-ambiguous and log `error.type`; and treat 5xx as *possibly* permanent,
capping retries. Always guard against an error body that has no `error` field.

---

## D12. Does the OpenAI side accept `"parallel_tool_calls": false`?

**It accepts it (HTTP 200) and ignores it.**

Request: `parallel_tool_calls:false`, one `bash` tool, prompt "Use the bash tool to do three
separate things: list /a, list /b, and list /c."

```json
"finish_reason":"tool_calls",
"tool_calls":[
 {"id":"call_137b32c3c32c4339ab5749f6","type":"function","function":{"name":"bash","arguments":"{\"command\": \"ls /a\"}"}},
 {"id":"call_c5e114b4f267439ea8ee2b7e","type":"function","function":{"name":"bash","arguments":"{\"command\": \"ls /b\"}"}},
 {"id":"call_17775982f2994613a690341f","type":"function","function":{"name":"bash","arguments":"{\"command\": \"ls /c\"}"}}]
```

Three parallel tool calls, with `parallel_tool_calls` explicitly `false`. The control run with
`parallel_tool_calls: true` produced an identical result — 3 calls, same arguments.

Related probe: an entirely invented parameter, `"totally_made_up_param_xyz":{"a":1}`, also
returns **HTTP 200**. Unknown request parameters are silently dropped, never rejected.

Takeaway: the parameter is accepted but is a no-op, so **your agent loop must be able to execute
a batch of tool calls no matter what you request**. More generally, a 200 here does not mean a
parameter took effect — this gateway never validates request parameters, so the only way to know
whether something works is to observe the response, which is the whole premise of these notes.

---
## Provenance

All bodies above were captured with `curl` against the live endpoint on 2026-08-27 and pasted
verbatim, with three deliberate exceptions, each marked where it occurs:

1. The ~4,000-token `thinking` string in A3a is elided as `[...]`; it was returned in full.
2. In the A3b/A3c `find -exec` examples the shell terminator is shown as `\;`; on the wire it
   was the JSON-escaped `\;`. No finding depends on it.
3. Long SSE captures are shown as their `data:` payloads with the repeated `id`/`object`/
   `created`/`model` envelope keys dropped after the first occurrence.

Reproduce any section by rebuilding the request body shown and re-POSTing it. The key used was
temporary and is expected to be revoked.
