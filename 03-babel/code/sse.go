// Stage 03 — SSE framing, and nothing else.
//
// This file was carved out of stage 02's sse.go, and the cut is the point of
// the chapter. Everything here is about the *transport*: how bytes on a wire
// become discrete frames. Nothing here knows what a token is, what a tool call
// looks like, or which vendor is on the other end.
//
// That separation is what lets one reader serve two protocols that disagree
// about almost everything else: the OpenAI surface sends only `data:` lines
// with a `[DONE]` sentinel, the Anthropic surface sends `event:` + `data:` with
// no sentinel at all. Same framing, different payloads. See openai.go and
// anthropic.go for the halves that do know.
package main

import (
	"bufio"
	"io"
	"strings"
)

// ---------------------------------------------------------------------------
// Half one: SSE framing. Protocol-agnostic on purpose.
// ---------------------------------------------------------------------------

// sseFrame is one decoded SSE frame. Name is "" on streams that omit event:
// lines — which is every frame this stage will ever see, because the OpenAI side
// of this endpoint sends only `data:` (§B4: `grep -c '^event:'` = 0 across the
// whole stream). Name exists anyway because the Anthropic side in stage 03 does
// use `event:` lines, and a reader that has to be taught about them later is a
// reader that is wrong in between.
type sseFrame struct {
	Name string
	Data string
}

// readSSE calls fn for each frame until the stream ends. It must handle: frames
// with only `data:` lines, frames with `event:` + `data:`, multi-line data,
// blank-line separation, CRLF, and comment lines starting with ':'.
// Returning a non-nil error from fn stops the scan and returns that error.
//
// Note what it does *not* do: it has no idea what `[DONE]` means. A sentinel is
// a property of the payload protocol, not of the framing, and pushing that
// knowledge down here is how you end up unable to reuse the reader.
//
// Three details in the implementation are each worth a bug:
//
//  1. bufio.Reader, not bufio.Scanner. Scanner refuses tokens over 64KB by
//     default and reports that as an error at the worst possible moment — a
//     large tool result echoed back in one delta is exactly the frame that
//     trips it, and it will only ever happen in production.
//
//  2. The last line of the stream is processed *before* the EOF is acted on.
//     ReadString hands back the bytes it managed to read alongside io.EOF, so a
//     server that closes without a trailing blank line still has its final
//     frame sitting in `line`. Check the error first and you silently drop the
//     last frame of every such stream — usually the one carrying usage.
//
//  3. Line endings are stripped one at a time (`\n`, then `\r`) rather than
//     with a cutset, so data that legitimately ends in a carriage return keeps
//     it. A lone-CR terminator — permitted by the SSE spec, emitted by nobody,
//     and absent from §B4 — is out of scope; observation wins over the spec
//     here, as everywhere else in this file.
func readSSE(r io.Reader, fn func(sseFrame) error) error {
	br := bufio.NewReader(r)

	var (
		name    string
		data    []string // one entry per `data:` line; joined with "\n" on dispatch
		sawData bool     // whether *any* data line arrived, not whether it was non-empty
	)

	// dispatch delivers the frame built so far and resets the buffers.
	//
	// The spec says a frame with no data lines is not an event, and that is the
	// rule here: it makes runs of blank lines and bare keep-alive comments free,
	// rather than a stutter of empty frames. A frame with a data line that
	// happens to be empty *does* dispatch, which is a deliberate step past the
	// spec — this is a debugging tool, and a visibly empty frame teaches more
	// than a silently dropped one.
	dispatch := func() error {
		if !sawData {
			name = ""
			return nil
		}
		f := sseFrame{Name: name, Data: strings.Join(data, "\n")}
		name, data, sawData = "", data[:0], false
		return fn(f)
	}

	for {
		line, err := br.ReadString('\n')

		if line != "" {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")

			switch {
			case line == "":
				// Blank line: end of frame.
				if derr := dispatch(); derr != nil {
					return derr
				}

			case strings.HasPrefix(line, ":"):
				// Comment. Proxies and gateways send these as keep-alives so an
				// idle connection is not reaped mid-generation. They carry
				// nothing and must not end the current frame — and note this
				// case has to be tested before the field split below, or
				// `: ping` parses as a field with an empty name.

			default:
				// `field: value`, where only the FIRST colon separates and a
				// single leading space of the value is stripped. Both matter:
				// every payload here is JSON, so values are full of colons, and
				// getting the space rule wrong shifts every byte of every
				// message by one.
				field, value := line, ""
				if i := strings.IndexByte(line, ':'); i >= 0 {
					field, value = line[:i], line[i+1:]
					value = strings.TrimPrefix(value, " ")
				}
				switch field {
				case "event":
					name = value
				case "data":
					data = append(data, value)
					sawData = true
				}
				// `id:` and `retry:` are spec fields for reconnecting to a
				// broken stream. Neither appears in §B4, and resuming a
				// half-generated completion is not something this endpoint
				// offers, so they are ignored rather than half-supported.
			}
		}

		if err != nil {
			if err == io.EOF {
				// The stream ended. Anything still buffered is a real frame
				// that never got its terminating blank line — the Anthropic
				// side (§B6) ends exactly this way, by closing the connection
				// with no sentinel at all.
				return dispatch()
			}
			return err
		}
	}
}

// ---------------------------------------------------------------------------
// Half two: the OpenAI chunk schema.
// ---------------------------------------------------------------------------

// sseDoneSentinel is the frame the OpenAI protocol uses to say "that's all".
//
// DECISION: we skip it and KEEP DRAINING to EOF. It is not a stop signal here.
//
// §B4 frame 13 is a real frame that arrives *after* the sentinel:
// `{"choices":[],"cost":"0"}`. Every spec-conforming client stops reading at
// `[DONE]` and throws that away. Three reasons not to be one of them:
//
//   - Correctness. The cost frame is data this endpoint is trying to give us.
//   - Connection hygiene. Abandoning a response body with bytes still in it
//     means the HTTP transport cannot return the connection to the keep-alive
//     pool; you pay a fresh TLS handshake every turn and never notice why.
//   - Robustness. If usage ever moves after the sentinel — and on an endpoint
//     that already puts `cost` there, that is not a wild hypothesis — a client
//     that stops early reports zero tokens and is confidently wrong.
//
// Draining costs nothing: the server closes the stream immediately afterwards.
const sseDoneSentinel = "[DONE]"
