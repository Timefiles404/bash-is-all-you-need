# Stage 03 — Babel

Two protocols, one agent. Give it a URL, a key, a protocol name and a model, and
it works — a local Ollama and a frontier API are the same four fields.

The interesting part is not that it can be done. It is **what you have to decide
in order to do it**, because the two protocols disagree about nearly everything,
and every disagreement forces you to pick a neutral form that belongs to
neither.

---

## The rule

> **The agent loop must never contain a vendor's word.**

No `tool_calls`, no `stop_reason`, no `input_tokens`, no `chat/completions`. If
one leaks into the loop, the second protocol stops being an adapter and becomes
an `if` statement — and then a hundred of them.

The test of the abstraction is not that it compiles. It is: **did adding the
second protocol change the agent loop?** Compare `stages/03-babel`'s loop with
stage 02's. It is the same loop with its vocabulary replaced.

```
main.go            speaks Msg, Block, StopReason, Usage — and nothing else
  │
  ├─ provider.go   the neutral language + the Provider interface
  ├─ sse.go        SSE framing. Knows nothing about tokens or tools
  ├─ openai.go     one vendor's opinions, quarantined
  └─ anthropic.go  the other vendor's opinions, quarantined
```

`sse.go` is the piece worth noticing. It was carved out of stage 02, and the cut
is exactly where mechanism ends and opinion begins: **framing is shared, payload
is not.** One protocol sends only `data:` lines with a `[DONE]` sentinel; the
other sends `event:` + `data:` with no sentinel at all. Same reader.

---

## What actually differs

Every row is observed, not read off a spec. Evidence in
[wire-notes.md](wire-notes.md).

| | OpenAI protocol | Anthropic protocol |
|---|---|---|
| **System prompt** | `messages[0]`, `role:"system"` | top-level `system` field |
| **Tool definitions** | nested under `{"type":"function","function":{…,"parameters":…}}` | flat `{"name","description","input_schema"}` |
| **Tool call arguments** | a JSON **string** | a JSON **object** |
| **Tool results** | one separate `role:"tool"` message **per call** | **all** results as blocks inside **one `user` message** |
| **Stop reason** | `finish_reason`: `tool_calls`/`stop`/`length` | `stop_reason`: `tool_use`/`end_turn`/`max_tokens` |
| **Reasoning** | `reasoning_content`, a sibling field in the same delta | a separate indexed content block with `thinking_delta` |
| **SSE framing** | `data:` only, `[DONE]` sentinel, **plus a frame after it** | `event:` + `data:`, no sentinel, `ping` **before** `message_start` and **after** `message_stop` |
| **Where usage lives** | a chunk whose `choices` array is **empty** | `message_delta` only — `message_start`'s figure is **wrong** (56 vs 291 for the same request) |
| **Token accounting** | `prompt_tokens` is the total; `cached_tokens` is nested **inside** it | `input_tokens` is the **uncached remainder**; cache counters sit **beside** it |
| **Cache control** | implicit only, 64-token block aligned, varies run to run | explicit `cache_control` pins the exact prefix |

Two of these rows are where the design gets made.

### Tool results, and why there is no `RoleTool`

One protocol answers a tool call with its own message, one per call. The other
gathers every result into a single `user` message. There is no neutral role that
means both, so **the neutral form has neither**: a tool result is a *Block*, and
each adapter decides what message shape carries it.

That is the entire reason `Msg` holds blocks rather than a flat string. Picking
either vendor's shape as "neutral" would have smuggled one of them into the core
— and the leak would only become visible when the second protocol arrived, which
is exactly when it is most expensive to fix.

### Tool arguments stay raw bytes

`Block.Args` is a `string` holding raw JSON, not a decoded `map[string]any`. One
protocol wants a JSON string, the other a JSON object; raw bytes are the only
form that reaches both without re-serialising.

And re-serialising is not free: Go's map iteration order is not stable, so a
decode-then-encode round trip can produce different bytes for the same value —
which changes the prompt prefix, which **invalidates the cache** the next chapter
is entirely about. A "harmless" normalisation in the wrong place costs real
money two chapters later.

### Normalise the stop reason, but keep the original

```go
type CallResult struct {
    Stop    StopReason  // normalised: end_turn / tool_use / max_tokens / filtered / unknown
    RawStop string      // the provider's literal string
}
```

This is not redundancy. On this gateway, a tool call truncated at `max_tokens`
comes back with `stop_reason: "tool_use"` and an unusable body — **the envelope
lies** (wire-notes §A3c). When a session goes wrong, `Stop` tells you what the
agent believed and `RawStop` tells you what it was told, and the gap between them
is the bug.

**Never normalise away your only evidence.** And note the other half of this
rule: unknown strings map to `StopUnknown`, not to `StopEndTurn`. A state machine
that maps anything unrecognised to "probably fine" will eventually map a refusal,
a quota event, or a new safety stop to "probably fine".

---

## Configuration

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

Three deliberate choices:

- **JSON, not TOML.** TOML would be a dependency, or a hundred lines of parser
  that teach nothing about agents. Ugly and free beats elegant and costly in a
  repo whose claim is that you can read all of it.
- **`api_key_env`, never `api_key`.** A config file gets committed eventually —
  every one of them does. The only reliable defence is for the secret to have
  nowhere to sit in the file at all.
- **The env-var path still works.** `AGENT_BASE_URL` / `AGENT_API_KEY` /
  `AGENT_MODEL` run without any config file. Config formats are for when you
  have several endpoints; making the simple case require one is how tools become
  annoying.

Mapping a protocol name onto an implementation is thirteen lines in `config.go`,
and it is the only place in the repo that does it.

---

## What this buys, beyond portability

- **A trace records events, not wire format.** A session captured against one
  protocol replays identically — the renderer never knew which one it was.
- **Reasoning renders the same either way.** Two entirely different wire
  representations, one `KindReasoningDelta`.
- **The instrument panel keeps working**, because `Usage` was already neutral —
  which is why it has no field called `prompt_tokens`.

That last point is worth dwelling on. `Usage` was designed in stage 02, one
chapter before the second protocol existed, and it needed no change here. Not
foresight: it came from writing down *what the number means* (`Input` = "billed
at full price") instead of *what the API called it*. Names that describe meaning
survive a second implementation; names copied off a vendor's JSON do not.

---

## The accounting reversal, one more time

The two protocols count in opposite directions, and both adapters have to
normalise into the same struct:

```go
// OpenAI: prompt_tokens is the TOTAL, cached_tokens is nested inside it
Input     = prompt_tokens - cached_tokens
CacheRead = cached_tokens

// Anthropic: input_tokens is ALREADY the uncached remainder
Input      = input_tokens
CacheRead  = cache_read_input_tokens
CacheWrite = cache_creation_input_tokens
```

Get the OpenAI side wrong — copy `prompt_tokens` straight across — and
`Prompt()` reports 698 for a 506-token prompt. Notice when that bug is
invisible: **the error is exactly the size of the cache hit.** Zero on a cold
request. Looks perfect in testing. Gets steadily worse the better your caching
works.

---

## From a real run

The same task, the same binary, the same loop, on both protocols:

```sh
echo "count the .py files here" | agent --provider oai --trace oai.jsonl   # openai / mimo-v2.5
echo "count the .py files here" | agent --provider ant --trace ant.jsonl   # anthropic / qwen3.7-plus
```

Collapse the delta runs in each trace and compare the sequence of event kinds:

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

**Identical, item for item.** Now compare the bytes those events came from:

```
oai  request keys: max_tokens, messages, model, stream, stream_options, tools
ant  request keys: max_tokens, messages, model, stream, system, tools
```

Different envelopes, different framing, different accounting, different tool
shapes — and one agent loop that cannot tell. That equality is the deliverable;
everything else in this chapter is what it cost.

The Anthropic trace also replays with no key and no provider configured, because
what was recorded was events, not wire format.

### One difference the panels do show

Look at the two instrument readouts from those runs:

```
openai    / mimo-v2.5     in 579   full 131 · write 0 · read 448
anthropic / qwen3.7-plus  in 592   full 592 · write 0 · read 0
```

Same task, same size of conversation — and one bar is mostly green while the
other is entirely red. The second protocol cached **nothing**.

That is not a bug in the adapter, and it is not the model being different. It is
the missing half of this chapter: one protocol caches implicitly and the other
expects to be *asked*. Fixing it is one field in one request, and it is worth an
entire chapter, because the discipline around that field is worth far more than
the field.

→ that is stage 04.

---

## The HTML-escaping trap

Found while reconciling the two adapters, and worth knowing whatever you are
building.

Go's `json.Marshal` escapes `<`, `>` and `&` into `\u003c`, `\u003e` and
`\u0026`. It is a browser-safety default, and it is actively hostile to a shell
agent, where those three characters are `2>&1`, `>/tmp/out` and `<<EOF`:

```
json.Marshal        : {"command":"grep -rn 'x' . 2\u003e\u00261 | head -5 \u003e/tmp/out"}
SetEscapeHTML(false): {"command":"grep -rn 'x' . 2>&1 | head -5 >/tmp/out"}
```

The server decodes it, so the model reads the same string either way. Two things
still make it worth four lines of `json.Encoder`. The request inspector exists to
show you what you sent, and the first version is not readable. And whether the
escaping shifts a provider's cache key depends on whether it hashes raw bytes or
decoded content — which we do not know, and which is a reason to be *consistent*
rather than a reason to guess.

Consistency is the real argument: the two adapters originally disagreed, and two
adapters emitting different bytes for the same conversation is a wart in a
chapter about normalising away exactly that kind of difference.

---

## Exercises

1. **Run the same task on both protocols** and diff the traces. The events
   should be nearly identical; the `request` events will share nothing.
2. **Point it at a local Ollama.** Small models emit malformed tool calls — that
   is not your code failing, and `parseBashArgs` will tell you so.
3. **Try to leak a vendor word into the loop.** Add a special case in `main.go`
   for one protocol and notice how quickly it wants a second.
4. **Add a third protocol.** Google's is a reasonable exercise. Count how many
   files you have to touch; the answer should be one, plus thirteen lines of
   `config.go`.
5. **Break the raw-bytes rule.** Decode `Args` into a map and re-encode it, then
   watch the cache-read column in stage 04 collapse.

→ Next: [Stage 04 — The Cache](04-the-cache.md)

→ Reference: [Wire notes](wire-notes.md)
