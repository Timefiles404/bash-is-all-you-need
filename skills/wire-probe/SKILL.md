---
name: wire-probe
description: Probe the LLM gateway with curl and record what it actually sends in docs/wire-notes.md
---

# Probing the gateway

Every teaching claim in this repo rests on `docs/wire-notes.md`, which records
what one real gateway actually sends — byte by byte, with the raw evidence
attached. Protocol documentation and observed behaviour do not agree, and where
they differ the observation wins.

## Setup

Credentials live in the gitignored `.env` at the repo root. The shell in this
harness does not persist between calls, so source it in every command:

```sh
set -a && . ./.env && set +a
```

Two endpoints on the same key: `$AGENT_BASE_URL/chat/completions` (OpenAI
protocol) and `$AGENT_BASE_URL/messages` (Anthropic protocol).

## Method

- Use `curl`, not the agent. The point is to see the wire, not a rendering of it.
- Capture the response to a file first, then inspect it. A pipeline that greps
  as it goes discards the evidence you will want ten minutes later.
- For streaming, capture with `--no-buffer` and keep every SSE frame, including
  the ones that look like noise. `ping` frames before `message_start` and after
  `message_stop` are a real finding.
- Probe the failure paths deliberately: a bad model id, a bad key, a malformed
  body, a missing required field, a `max_tokens` low enough to truncate a tool
  call mid-argument. The failure shapes are where the two protocols differ most
  and where documentation is least reliable.

## Recording

Add a numbered section to `docs/wire-notes.md` with:

- The exact `curl` invocation, with the key redacted.
- The raw response body or SSE frames, verbatim, elided only where noted.
- One sentence saying what the finding is.
- A note if it contradicts the protocol documentation, with what the
  documentation says.

Never write a claim from memory of a spec. If it is not in wire-notes with
evidence, it does not go in a chapter.
