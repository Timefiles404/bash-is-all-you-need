// Stage 11 — the tool call is the one thing in the conversation that the model
// wrote and you have to execute.
//
// Every other field a model produces is text: wrong text is a bad answer.
// Arguments are different — they cross into `exec.Command`, and they arrive over
// a wire that has three separate ways of handing you something that is not what
// the model meant:
//
//   - Truncation. §A2: on the OpenAI route a tool call cut off at max_tokens
//     comes back with `tool_calls: []` and the gateway's internal
//     `<tool_call><function=bash>` markup dumped into `message.content`. §A3c: on
//     the Anthropic route `input` is replaced by `{"raw_arguments": "<invalid
//     JSON>"}` — and `stop_reason` still says `"tool_use"`, so the envelope
//     carries no signal at all.
//   - A schema nobody enforces. §E13: neither route validates the returned call
//     against the `input_schema`/`parameters` it was given. An `enum` violation
//     came back untouched, and so did a property forbidden by
//     `additionalProperties: false`.
//   - The accumulator. Arguments arrive in fragments, and §B4 shows the splits
//     land mid-token. Three dialects exist in the wild and no protocol document
//     names any of them; see mergeArgs.
//
// So this file is one boundary that every call crosses before it is dispatched
// AND before it enters the message array. The second half of that sentence is
// the part that costs money: §E14 measured what each route does with a bad call
// that was kept, and they fail in opposite directions. The Anthropic route
// accepts it forever, so the model is asked to continue a conversation in which
// it appears to have called a tool with arguments it never wrote. The OpenAI
// route answers **400 for every subsequent request in the session** — and a 400
// is correctly fatal, so one unvalidated tool call is a permanently dead
// session.
//
// What this file deliberately does NOT do is repair truncated arguments. That
// decision is measured rather than asserted; see docs/11-malformed.md.
package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// The taxonomy
// ---------------------------------------------------------------------------

// argFault is why a tool call cannot be dispatched.
//
// Three values, not one, for the same reason stage 09 has three triage verdicts:
// they lead to different actions, and collapsing them loses the action. The
// distinction that matters most is faultCut versus faultSchema, because they
// disagree about *whose mistake it was*. A call the model never finished writing
// is not a call with bad arguments, and telling the model its JSON was invalid
// when the truth is that it ran out of budget spends a round trip on a false
// diagnosis — the model will re-send the same too-long command.
type argFault string

const (
	faultNone argFault = ""

	// faultCut — generation stopped in the middle of the arguments. Detected
	// from the gateway's own `raw_arguments` shape (§A3c), or from a fragment
	// whose JSON is still *open* at the end.
	faultCut argFault = "cut"

	// faultNotJSON — something arrived, it is closed, and it is not JSON.
	// Prose, harness markup, an apology.
	faultNotJSON argFault = "not_json"

	// faultSchema — valid JSON that contradicts the schema this program
	// published for this tool in this very request.
	faultSchema argFault = "schema"
)

// argCheck is the boundary's verdict on one call.
type argCheck struct {
	Fault argFault

	// Detail names the specific violation, for the trace and for the model.
	// Never a whole payload: a truncated argument can be thousands of bytes and
	// it is about to be replayed on every subsequent request.
	Detail string

	// Args is the arguments to dispatch with, and it is set ONLY when Fault is
	// faultNone. There is no partial success here on purpose — a checked call
	// and an unchecked one must not be the same type, or the one call site that
	// forgets to look at Fault is a shell command the model never wrote.
	Args map[string]any

	// Dropped names properties the schema does not declare, which were removed
	// rather than rejected.
	//
	// This is the one place the boundary is deliberately lenient, and the reason
	// is arithmetic. §E13 measured that models really do add fields — a
	// `timeout_ms` the schema forbade, arriving as the string "5000" — and that
	// nothing upstream stops them. An unknown property is by definition one the
	// tool does not read, so dropping it cannot change what runs, while refusing
	// costs a full round trip: the model writes, the harness rejects, the model
	// reads the rejection, the model writes again. Paying that to remove a key
	// that was already going to be ignored is a poor trade.
	//
	// It is still reported, because the alternative is a silent divergence
	// between what the model thinks it asked for and what happened — and the
	// model asking for a `timeout_ms` is a fact about the tool's design, not
	// noise.
	Dropped []string
}

// maxDetail bounds what goes into the message array. The number is not
// important; that there is one is. Detail rides along in the history for the
// rest of the session, so an unbounded detail is an unbounded per-turn cost
// paid for evidence nobody rereads. clip() is compact.go's, which keeps both
// ends — the useful half of a truncated argument is at the cut.
const maxDetail = 200

// ---------------------------------------------------------------------------
// checkCall
// ---------------------------------------------------------------------------

// checkCall decides whether a tool call may be executed.
//
// `raw` is the accumulated argument string exactly as it came off the wire, and
// `t` is the tool definition **as sent** — the same Schema map that went into
// the request. That is the point of taking the whole Tool rather than a
// hand-written validator per tool: the check and the advertisement cannot drift,
// because they are the same object. Stage 10 had a `parseBashArgs` and a
// `parseTaskArgs`, each re-stating in Go what bashToolDef and taskToolDef had
// already said in JSON, and nothing made them agree.
func checkCall(t Tool, raw string) argCheck {
	trimmed := strings.TrimSpace(raw)

	// A zero-argument call. §E14 measured that replaying `arguments: ""` is an
	// HTTP 400 on the OpenAI route, so the empty string never survives this
	// boundary in either direction — see also renderArgs.
	if trimmed == "" {
		trimmed = "{}"
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		// Which of the two "unparseable" faults is this? The answer is in the
		// bracket state, and reading it is the *only* use this file has for the
		// brace-counting machinery that lenient-JSON libraries use to repair:
		// here it decides who to blame, not what to run.
		if jsonIsOpen(trimmed) {
			return argCheck{Fault: faultCut, Detail: clip(trimmed, maxDetail)}
		}
		return argCheck{Fault: faultNotJSON, Detail: clip(trimmed, maxDetail)}
	}

	// The gateway's own truncation shape, §A3c. `raw_arguments` is not part of
	// the Anthropic Messages spec, so a tool that genuinely declares a property
	// by that name would be a different case — hence the schema consultation
	// rather than a bare key test.
	if inner, ok := obj["raw_arguments"]; ok && !declaresProperty(t.Schema, "raw_arguments") {
		s, _ := inner.(string)
		return argCheck{Fault: faultCut, Detail: clip(s, maxDetail)}
	}

	if why := schemaViolation(t.Schema, obj); why != "" {
		return argCheck{Fault: faultSchema, Detail: why}
	}
	dropped := pruneUndeclared(t.Schema, obj)
	return argCheck{Args: obj, Dropped: dropped}
}

// pruneUndeclared removes properties the schema does not declare and returns
// their names. It only prunes when the schema actually says
// `additionalProperties: false`; a tool that stays silent keeps the JSON Schema
// default and accepts them.
//
// So the declaration is honoured either way, which is the property worth having:
// the schema in the request and the behaviour at the boundary are the same
// statement, and neither can drift into being decoration.
func pruneUndeclared(schema map[string]any, obj map[string]any) []string {
	if allowsExtra(schema) {
		return nil
	}
	props, _ := schema["properties"].(map[string]any)
	var dropped []string
	for _, name := range sortedKeys(obj) {
		if _, declared := props[name]; !declared {
			dropped = append(dropped, name)
			delete(obj, name)
		}
	}
	return dropped
}

// jsonIsOpen reports whether the text ends mid-value: inside a string, or inside
// a container.
//
// It answers "was this cut off" and nothing else. It is not a validator: `{]` is
// closed and nonsense, and this returns false for it, which is correct — that is
// a faultNotJSON, not a truncation.
//
// It also used to check for a trailing comma or colon, and mutation testing
// removed that: no input can reach it. A cut immediately after `{"a":` or
// `{"a":1,` leaves the object open, so `depth > 0` has already decided, and a
// value that is not inside a container cannot contain a comma or a colon at all.
// Two lines of code and a variable that could never change an answer.
func jsonIsOpen(s string) bool {
	depth := 0
	inStr, esc := false, false

	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
	}

	// A trailing lone backslash is an escape with nothing to escape, which only
	// happens when the cut landed between the two bytes.
	return inStr || esc || depth > 0
}

// ---------------------------------------------------------------------------
// The schema subset
// ---------------------------------------------------------------------------

// schemaViolation checks `obj` against `schema` and returns the first violation
// as a sentence, or "".
//
// This is NOT a JSON Schema implementation, and it should not become one. It
// covers exactly the keywords this repo's tools declare — `type`, `properties`,
// `required`, `enum`, `additionalProperties` — because a validator that
// understands keywords you never emit is a dependency you wrote yourself, and it
// will disagree with the real thing in ways nobody tests.
//
// Two deliberate omissions, both of which a stricter validator would catch and
// which are left to the tool:
//
//   - Numbers are checked for being numbers, not for being integers or in range.
//     Models emit 5.0 where 5 was declared and 200 where the maximum is 100, and
//     a refusal costs a whole round trip — model writes, harness rejects, model
//     reads, model retries — to fix something a clamp fixes for free. Strictness
//     at the boundary is a latency and a cost decision, not only a correctness
//     one. The rule this file settles on: coerce what has exactly one obvious
//     reading, refuse what does not.
//   - Nested object properties. No tool here has one, and a schema walker that
//     recurses is a schema walker with a cycle bug waiting for it.
//
// §E13 is why any of this exists: the endpoint returned `"shell": "powershell"`
// against an enum of `["bash","sh"]`, and a property forbidden by
// `additionalProperties: false`, both with a 200 and a normal finish reason.
// Nothing upstream is doing this.
func schemaViolation(schema map[string]any, obj map[string]any) string {
	props, _ := schema["properties"].(map[string]any)

	// Required first, and missing-required before wrong-type, because a
	// truncated-but-parseable call — `{}` from §E14 — is missing rather than
	// wrong, and "the command field is absent" is a more useful thing to be
	// told than a list of everything else that was fine.
	for _, name := range requiredNames(schema) {
		if _, ok := obj[name]; !ok {
			return fmt.Sprintf("the required %q field is absent", name)
		}
	}

	// Sorted, because map iteration order in Go is randomised and an error
	// message that names a different field on each run is a bug report nobody
	// can reproduce.
	for _, name := range sortedKeys(obj) {
		value := obj[name]
		spec, declared := props[name].(map[string]any)
		if !declared {
			// Handled by pruneUndeclared, not here: an undeclared property is
			// not a violation the model needs to hear about, it is a key the
			// tool will not read. See argCheck.Dropped.
			continue
		}
		if want, ok := spec["type"].(string); ok {
			if got := jsonTypeOf(value); !typeMatches(want, got) {
				return fmt.Sprintf("%q should be %s and arrived as %s", name, want, got)
			}
		}
		if allowed, ok := spec["enum"].([]any); ok && !inEnum(value, allowed) {
			return fmt.Sprintf("%q was %v, which is not one of %v", name, value, allowed)
		}
	}
	return ""
}

// requiredNames reads the `required` list, tolerating both []string (what this
// repo's tool definitions write) and []any (what a JSON round trip produces).
// The two forms exist because a Tool built in Go and a Tool decoded from a
// config file are the same type, and a validator that only understood one of
// them would work in tests and not in replay.
func requiredNames(schema map[string]any) []string {
	switch req := schema["required"].(type) {
	case []string:
		return req
	case []any:
		out := make([]string, 0, len(req))
		for _, r := range req {
			if s, ok := r.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// allowsExtra defaults to true — the JSON Schema default — so a tool that never
// mentions additionalProperties keeps accepting the harmless extra field a model
// occasionally adds, rather than silently becoming strict.
func allowsExtra(schema map[string]any) bool {
	if v, ok := schema["additionalProperties"].(bool); ok {
		return v
	}
	return true
}

func declaresProperty(schema map[string]any, name string) bool {
	props, _ := schema["properties"].(map[string]any)
	_, ok := props[name]
	return ok
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// jsonTypeOf names the JSON type of a value decoded into `any`. Note that
// json.Unmarshal has already collapsed integers and floats into float64, so
// "integer" is not observable here — which is the mechanical half of the reason
// integer bounds are not checked.
func jsonTypeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

func typeMatches(want, got string) bool {
	if want == "integer" {
		return got == "number"
	}
	return want == got
}

func inEnum(v any, allowed []any) bool {
	for _, a := range allowed {
		if a == v {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// What the model is told
// ---------------------------------------------------------------------------

// faultText is the tool result a rejected call gets.
//
// One rule governs every string in here, and it is not obvious: **the text
// describes what happened and gives no instruction.** No "send valid JSON", no
// "try again with a shorter command", no "do not retry this unchanged".
//
// The reason is that this text is not a message. It is a permanent addition to
// the prompt: it goes into the message array, and every subsequent request in
// the session re-sends it. An imperative in there reads as a fresh instruction
// several turns later, when its context is gone — so the model re-issues a call
// that was already handled, and the older the message gets the more confidently
// it does so. Stage 10 shipped four of these ("send valid JSON", "send it
// again", "Retry with a shorter command", "Do not retry it unchanged") and they
// are gone.
//
// A statement of fact ages into a statement of fact.
func faultText(t Tool, c argCheck) string {
	switch c.Fault {
	case faultCut:
		// Deliberately does not quote the fragment back. The model wrote it and
		// does not need to be shown it, and §A3c fragments run to hundreds of
		// bytes of shell command that would then be replayed forever.
		return fmt.Sprintf("[not executed: the arguments for %s stopped mid-value, "+
			"so the call never finished being written]", t.Name)
	case faultNotJSON:
		return fmt.Sprintf("[not executed: the arguments for %s were not JSON. "+
			"What arrived: %s]", t.Name, c.Detail)
	case faultSchema:
		return fmt.Sprintf("[not executed: %s]", c.Detail)
	}
	return ""
}

// ---------------------------------------------------------------------------
// Identity
// ---------------------------------------------------------------------------

// uniqueIDs makes every tool call id distinct across the whole session, renaming
// collisions in place and reporting how many it had to rename.
//
// This exists because a gateway can mint the same id for every call it ever
// makes. It is legal inside one turn — nothing reads the id except the matching
// result — and it is fatal the moment the history is replayed, because the
// protocol requires ids to be unique across the message array, and the rejection
// names a message index rather than the tool, so the error points at the wrong
// place.
//
// Two properties worth stating, because both were bugs on the way here:
//
//   - `seen` spans the session, not the turn. Duplicates that matter live in
//     *different* assistant messages, so a per-turn check finds nothing and the
//     request is rejected anyway.
//   - Renaming happens BEFORE the result blocks are built, so the call and its
//     answer carry the same new id. Renaming a call whose result already exists
//     is how you turn a duplicate-id rejection into an orphaned-result
//     rejection, which is the same outage with a less helpful message.
func uniqueIDs(calls []Block, seen map[string]bool) int {
	renamed := 0
	for i := range calls {
		if calls[i].Kind != BlockToolCall {
			continue
		}
		id := calls[i].ID
		if id == "" {
			// An id is how a result finds its call. A call without one cannot
			// be answered, and an unanswered call is a rejected request.
			id = fmt.Sprintf("call_%d", len(seen)+1)
		}
		if !seen[id] {
			seen[id] = true
			calls[i].ID = id
			continue
		}
		for n := 2; ; n++ {
			// The suffix is bounded to keep the id inside the 64-character
			// limit gateways validate; ids observed here are 24-28 characters,
			// so there is room, and truncating the *prefix* instead would risk
			// colliding with the very id being avoided.
			cand := fmt.Sprintf("%s_%d", id, n)
			if len(cand) > 56 {
				cand = fmt.Sprintf("call_dup_%d", len(seen)+1)
			}
			if !seen[cand] {
				seen[cand] = true
				calls[i].ID = cand
				renamed++
				break
			}
		}
	}
	return renamed
}

// ---------------------------------------------------------------------------
// The markup leak
// ---------------------------------------------------------------------------

// harnessMarkers are the openings of the gateway's internal tool-call syntax,
// as captured in §A2: `<tool_call>\n<function=bash>\n<parameter=command>…`.
var harnessMarkers = []string{"<tool_call>", "<function=", "<parameter="}

// stripHarnessMarkup removes leaked gateway internals from assistant text and
// says whether it found any.
//
// §A2 is the mechanism: the model does not emit JSON on the wire, it emits this
// XML-ish syntax, and the gateway parses it server-side into `tool_calls`. When
// generation stops mid-syntax the parse fails and the gateway falls back to
// handing you the raw markup in `message.content`. So there are two failures at
// once, and stage 10 had both: the markup is printed to the human as if the
// assistant said it, and it is appended to the history as assistant text, where
// it teaches the model that emitting this syntax as prose is a thing that
// happens here.
//
// It cuts from the first marker to the end rather than trying to excise a
// balanced block, because there is no balanced block: the text is truncated by
// definition, and the closing tags are exactly what is missing.
//
// What it does NOT do is parse the markup into a tool call. The fragment often
// contains a complete-looking `<parameter=command>` value, and running it is the
// same mistake as repairing truncated JSON with one extra step: a model
// *discussing* a tool call and a model *making* one would become
// indistinguishable, and the deciding evidence — that the gateway itself could
// not parse it — is evidence against executing, not for it.
func stripHarnessMarkup(text string) (string, bool) {
	cut := -1
	for _, m := range harnessMarkers {
		if i := strings.Index(text, m); i >= 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if cut < 0 {
		return text, false
	}
	return strings.TrimRight(text[:cut], " \t\n"), true
}

// ---------------------------------------------------------------------------
// The accumulator's dialects
// ---------------------------------------------------------------------------

// mergeArgs adds one streamed argument fragment to what has accumulated.
//
// Appending unconditionally is the obvious implementation and it is wrong,
// because three different dialects arrive on this field and no protocol document
// names any of them:
//
//	incremental  each fragment is the next few bytes           -> append
//	cumulative   each fragment is the whole value so far       -> replace
//	re-send      the final fragment repeats the whole value    -> replace
//
// §B4 shows the incremental dialect on this endpoint, with splits landing
// mid-token, so append is the right default. The other two are real: a gateway
// that re-sends complete arguments in its last chunk turns append into
// `{"command":"ls"}{"command":"ls"}`, whose error — `invalid character '{' after
// top-level value` — names a byte offset and nothing about the cause.
//
// The test that separates them is that **a tool call's arguments are exactly one
// top-level JSON value**, so "the buffer already parses" is a terminal state.
// Anything arriving after it cannot be a continuation.
func mergeArgs(have, frag string) string {
	if frag == "" {
		return have
	}
	if have == "" {
		return frag
	}
	// Terminal state: what we have is already a whole value.
	if json.Valid([]byte(strings.TrimSpace(have))) {
		// Latest complete value wins; a trailing partial is dropped. Keeping
		// the first would be equally defensible and is worse in practice —
		// the re-send dialect exists precisely because the gateway believes
		// its final chunk is the authoritative one.
		if json.Valid([]byte(strings.TrimSpace(frag))) {
			return frag
		}
		return have
	}
	// Cumulative: this fragment contains everything we already had.
	if strings.HasPrefix(frag, have) {
		return frag
	}
	return have + frag
}

// ---------------------------------------------------------------------------
// Reading a checked call
// ---------------------------------------------------------------------------

// toolByName finds the definition the boundary will validate against.
//
// It takes the list the agent is *currently* advertising rather than a package
// global, because that list is a function of depth: at the recursion limit the
// `task` tool is removed from the request entirely (see tools()). Validating
// against a global table would accept a call to a tool this agent did not offer,
// which is the one case where "there is no such tool" is the honest answer.
func toolByName(tools []Tool, name string) (Tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// argsForDisplay is the best readable form of a tool call's arguments, for the
// TUI and for the compaction summary.
//
// **Its result must never reach execution**, and that sentence is the reason it
// is a separate function with a name that says so. It is exactly the lenient
// parse the rest of this file refuses: it takes whatever it can get and falls
// back to the raw bytes, because a viewer showing a mangled command is right and
// a viewer showing nothing is not. The same leniency on a dispatch path is how a
// truncated command gets run.
//
// It reads `command` specifically rather than the first string property, because
// the alternative silently promotes whichever field a future tool happens to
// declare first into the thing the human sees as "the command".
// It also never returns blank. `{"raw_arguments":""}` and `{"command":"  "}` are
// both real payloads (the first is §A3c's, the second is a model that emitted a
// whitespace command), and extracting from either gives an empty string — which
// on a panel or in a compaction summary is a tool call that simply vanished.
// Falling back to the raw bytes keeps the evidence that *something* was tried,
// which is the whole reason a viewer parses leniently at all.
func argsForDisplay(args string) string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(args)), &obj); err == nil {
		for _, key := range []string{"command", "raw_arguments"} {
			if s, ok := obj[key].(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return args
}

// strArg reads a string property from a checked call.
//
// The type assertion is safe only because schemaViolation already ran, and that
// coupling is why this takes an argCheck rather than a bare map: a caller cannot
// reach the arguments without having gone through the boundary that makes the
// assertion true.
func strArg(c argCheck, name string) string {
	s, _ := c.Args[name].(string)
	return s
}

// renderArgs is what goes on the wire for one tool call's arguments.
//
// `{}` rather than `""` for a zero-argument call, because §E14 measured
// `arguments: ""` as an HTTP 400 on the OpenAI route while `{}` is accepted —
// and the empty string is not a hypothetical: §B4 shows the first streamed
// `tool_calls` delta arriving with `"arguments":""`, so a stream that breaks
// between the announcement and the first fragment accumulates exactly that.
//
// The Anthropic side has always done this (see anthropicToolInput); this is the
// symmetric half, which stage 10 did not have.
func renderArgs(args string) string {
	if strings.TrimSpace(args) == "" {
		return "{}"
	}
	return args
}
