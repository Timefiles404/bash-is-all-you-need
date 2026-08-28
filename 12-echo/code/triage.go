// Stage 09 — Triage.
//
// One idea: **an error is a decision, not a string.**
//
// An agent that has just failed a model call has exactly three moves available.
// Wait and send the same bytes again. Send them somewhere else. Stop and say
// why. Everything in this file exists to turn a failure into one of those
// three, and the reason it is a file rather than an `if` is that the two rules
// everybody starts with are both wrong on the endpoint this repo was built
// against:
//
//	"401 means the key is bad, so stop."
//	    A wrong MODEL NAME returns 401 here, with the same envelope shape as a
//	    revoked key (§D11). Stopping is correct for one of those and throws away
//	    a working session for the other.
//
//	"5xx is transient, so retry with backoff."
//	    A malformed request body returns 500 here (§D11). That is a bug in the
//	    client, and a policy keyed on the status alone retries it forever — the
//	    retry never succeeds and never stops.
//
// Neither rule gets fixed by reading the status code harder. The fix is to carry
// enough of the failure to decide with: where it broke, what the provider called
// it, and what the body actually said. That is the difference between this file
// and `fmt.Errorf`.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// What failed
// ---------------------------------------------------------------------------

// callPhase is where in one model call the failure happened, and it is the
// first thing the classifier looks at — because it answers the question that
// decides everything else: *was anything generated?*
//
// Nothing generated means a retry is free. Something generated means a retry
// costs a second full prompt and the first one is already on the invoice.
//
// It is called a phase and not a stage because "stage" already means a chapter
// of this course in every other file, and a field named Stage on an event in a
// directory named stages/09-triage is a sentence nobody can read twice the same
// way.
type callPhase string

const (
	phaseBuild   callPhase = "build"   // we could not even render the request
	phaseConnect callPhase = "connect" // no response at all: DNS, refused, TLS, reset before headers
	phaseStatus  callPhase = "status"  // a response arrived and it was not 200
	phaseStream  callPhase = "stream"  // 200, then the body broke or carried an error event
)

// CallError is one failed model call, in a shape a decision can be made from.
//
// Every field is here because some triage rule below needed it and a string
// could not carry it. The one that looks redundant — Body, sitting next to Type
// and Message — is the one that earns its place most often: on this gateway an
// observed 400 came back with no error envelope at all, just a 24-byte echo of
// the request (`{"model":"qwen3.7-plus"}`, §D11). Type and Message are both
// empty for that response and the raw body is the only evidence there is.
type CallError struct {
	Phase   callPhase
	Status  int    // 0 when there was no response
	Type    string // the provider's error.type, verbatim — never normalised, because it is evidence
	Message string
	Body    string // first 8 KiB of the response body, verbatim

	// RetryAfter is what the server asked us to wait, and 0 when it did not
	// ask. The server's number always beats our computed backoff: it is the one
	// party to this conversation that knows when the capacity comes back.
	RetryAfter time.Duration

	// Partial is whatever the adapter had accumulated when the stream broke.
	//
	// Kept rather than dropped, and this is the seam stage 09 came to fix.
	// Both adapters deliberately return a non-nil result alongside a non-nil
	// error — openai.go and anthropic.go both say so in a comment — and every
	// stage up to this one bound that value and never looked at it.
	//
	// Nothing here feeds a partial back to the model: the plan it was halfway
	// through is gone, and half a tool call is worse than none. What it is for
	// is the bill. Those tokens were generated, and generated tokens are
	// charged for, so an agent that drops the partial on the floor is an agent
	// that cannot explain its own invoice — which is the failure this whole
	// repository is about.
	Partial *CallResult

	Err error // the underlying transport error, so errors.Is still works

	// cause is set when the call was ended by one of THIS program's clocks —
	// the stall watchdog, the total deadline, or a human pressing Ctrl-C —
	// rather than by anything the provider did. See deadline.go.
	//
	// It has to be a separate field because by the time such a failure reaches
	// the classifier every one of them is context.Canceled, and Phase/Status
	// describe a call that never got to finish saying what went wrong. Without
	// it, an interrupt is an unclassified stream failure: retried three times,
	// then failed over to a second provider. Pressing stop would fan out.
	cause error
}

func (e *CallError) Error() string {
	switch e.Phase {
	case phaseBuild:
		return fmt.Sprintf("could not build the request: %v", e.Err)
	case phaseConnect:
		return fmt.Sprintf("no response from the provider: %v", e.Err)
	case phaseStatus:
		// The envelope-missing case gets a visibly different message rather
		// than an empty one. "http 400: " with nothing after the colon reads
		// like a bug in the agent; naming the absence points at the server.
		if e.Type == "" && e.Message == "" {
			return fmt.Sprintf("http %d with no error envelope: %.200s", e.Status, strings.TrimSpace(e.Body))
		}
		return fmt.Sprintf("http %d %s: %s", e.Status, e.Type, e.Message)
	case phaseStream:
		if e.Type != "" {
			return fmt.Sprintf("the provider sent %s mid-stream: %s", e.Type, e.Message)
		}
		return fmt.Sprintf("the stream broke: %s", e.Message)
	}
	return e.Message
}

func (e *CallError) Unwrap() error { return e.Err }

// asCallError is errors.As with the target declared, because the three-line
// dance appears at every call site otherwise.
func asCallError(err error) (*CallError, bool) {
	var ce *CallError
	ok := errors.As(err, &ce)
	return ce, ok
}

// ---------------------------------------------------------------------------
// The decision
// ---------------------------------------------------------------------------

// Triage is what to do about a failure. Three values, because an agent has
// three moves — and naming them after the moves rather than after the failures
// is the point of the whole file. `ErrRateLimited` tells you what happened;
// `TriageRetry` tells you what to do, which is the only thing the caller needs.
type Triage string

const (
	TriageRetry    Triage = "retry"    // the same bytes, later
	TriageFallback Triage = "fallback" // the same bytes, elsewhere
	TriageFatal    Triage = "fatal"    // stop, and say why
)

// triage maps one failure onto one decision.
//
// Read this next to §D11 of docs/wire-notes.md. Almost every line here exists
// because the recorded bytes contradicted the obvious rule.
func (e *CallError) triage() Triage {
	// Our own clocks answer first. A call this program ended is not evidence
	// about the provider, so nothing below has jurisdiction over it — and the
	// one that matters most, an interrupt, would otherwise be classified by
	// whatever half-formed status the dying request happened to carry.
	if v, _, ok := triageCause(e.cause); ok {
		return v
	}

	switch e.Phase {
	case phaseBuild:
		// Our own bug. A request we could not render will not render on the
		// second attempt either, and retrying it burns the budget that a real
		// outage later in the session needed.
		return TriageFatal

	case phaseConnect:
		// No response means nothing was generated and nothing was billed, so
		// this is the one class of failure where a retry is genuinely free.
		// DNS, connection refused, TLS handshake, RST before headers — all of
		// them are worth another attempt, and none of them are worth a
		// different provider until the attempts run out.
		return TriageRetry

	case phaseStream:
		// A stream that broke on the wire and a stream that carried an error
		// *event* are the same Go error today and must not be the same
		// decision.
		//
		// Type == "" is the transport case: the connection died mid-body. Worth
		// another attempt.
		//
		// A named type came from the provider deliberately, mid-response, and
		// only these four mean "ask again". The rest arrived because of what
		// we sent, and sending it again produces the same event — this is the
		// phaseStream twin of the 5xx trap.
		if e.Type == "" {
			return TriageRetry
		}
		switch e.Type {
		case "overloaded_error", "api_error", "rate_limit_error", "timeout_error":
			return TriageRetry
		}
		return TriageFatal

	case phaseStatus:
		return triageStatus(e.Status, e.Type)
	}
	return TriageFatal
}

// triageStatus is split out because it is the part with the surprises in it,
// and a table-driven test wants to call it directly.
func triageStatus(status int, typ string) Triage {
	t := strings.ToLower(typ)

	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		// §D11, the finding that motivates this entire file: on this gateway a
		// nonexistent model id returns **401**, not 404, with the same
		// `{"type":"error","error":{...}}` envelope a revoked key returns. The
		// status cannot separate them. `error.type` can — `ModelError` vs
		// `AuthError` — and the two need opposite decisions, because one is a
		// dead session and the other is a working session pointed at the wrong
		// model name.
		//
		// Matched as a substring, deliberately. The observed values are
		// PascalCase (`ModelError`) where both protocol specs say snake_case
		// (`not_found_error`, `invalid_request_error`), so an equality check
		// against either spelling would be correct against the documentation
		// and wrong against the wire.
		if strings.Contains(t, "model") {
			return TriageFallback
		}
		return TriageFatal

	case status == http.StatusNotFound:
		// The route or the model is not on this endpoint. Another endpoint may
		// have it; waiting will not make it appear here.
		return TriageFallback

	case status == http.StatusTooManyRequests:
		return TriageRetry

	case status == http.StatusRequestTimeout, status == http.StatusConflict:
		return TriageRetry

	case status == http.StatusRequestEntityTooLarge:
		// The bytes are the problem. Neither waiting nor a different provider
		// changes them; compaction does, and that is stage 05's job, not this
		// one's. Fatal here so the message reaches the human who can shrink it.
		return TriageFatal

	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity:
		// Ours. Retrying an argument the server rejected is how a client bug
		// becomes an outage.
		return TriageFatal

	case status >= 500:
		// Retried, but on the shortest leash of anything here — see leash().
		//
		// §D11 is why: a malformed request body returns **500** on this
		// gateway, and so does an OpenAI-shaped body POSTed to the Anthropic
		// route. Both are client bugs wearing a server's status code. "5xx =
		// transient" retries them until the budget dies, every turn, forever.
		return TriageRetry
	}

	// A status nothing above claimed. Fatal rather than retried, because an
	// unclassified failure retried is just a failure repeated — and the event
	// this emits is how the missing case gets found.
	return TriageFatal
}

// leash caps the attempts a failure class is worth, 0 meaning "the policy's
// full allowance".
//
// One rule, one reason. A 503 is a real capacity signal and gets everything. A
// bare 500 gets two attempts total, because on this endpoint it is at least as
// likely to be *our* misconfiguration as their outage (§D11), and two attempts
// is enough to ride out a blip while being far too few to hide a permanent
// mistake behind a retry loop.
func (e *CallError) leash() int {
	if e.Phase == phaseStatus && e.Status >= 500 && e.Status != http.StatusServiceUnavailable {
		return 2
	}
	return 0
}

// ---------------------------------------------------------------------------
// How long to wait
// ---------------------------------------------------------------------------

// retryPolicy is the whole configuration of retrying, and it is four numbers
// because the fifth one people add — "retry forever until it works" — is how a
// transient failure turns into a bill.
type retryPolicy struct {
	attempts int           // total attempts per provider, including the first
	base     time.Duration // the first backoff
	max      time.Duration // ceiling on any single wait
	budget   time.Duration // ceiling on all waiting in one call, summed
}

func defaultRetryPolicy() retryPolicy {
	return retryPolicy{attempts: 3, base: 500 * time.Millisecond, max: 8 * time.Second, budget: 30 * time.Second}
}

// wait returns how long to sleep before attempt n (n counts from 1, so the
// first wait is wait(1) and happens after the first failure).
//
// Full jitter — a uniform draw from [0, exp) — rather than the more common
// `exp/2 + rand(exp/2)`. The difference matters here specifically: stage 07
// spawns subagents that share one http.Client and one endpoint, so a provider
// hiccup fails several calls in the same millisecond. Half-jitter keeps those
// calls clustered and they re-collide on every attempt; full jitter spreads
// them across the whole interval. A retry policy that synchronises its own
// clients is a load generator aimed at the service that is already struggling.
//
// after is the server's Retry-After. It wins outright when present, because it
// is the only number in this function that came from someone who knows when the
// capacity returns — but it is still clamped by the caller's budget, since a
// server is also allowed to say "an hour".
func (p retryPolicy) wait(n int, after time.Duration, rnd func() float64) time.Duration {
	if after > 0 {
		if after > p.max*8 {
			// A server may ask for longer than we are willing to hold a turn
			// open. Honour the shape of the request but not the length: the
			// alternative is an agent that looks hung for an hour.
			return p.max * 8
		}
		return after
	}
	exp := p.base << (n - 1)
	if exp > p.max || exp <= 0 { // <= 0 catches the shift overflowing
		exp = p.max
	}
	return time.Duration(rnd() * float64(exp))
}

// parseRetryAfter reads the header in both forms RFC 9110 allows: delta-seconds
// and an HTTP-date.
//
// Written from the RFC rather than from observation, and the chapter says so:
// the gateway in docs/wire-notes.md never sent a 429 at all, so there is no
// recorded Retry-After anywhere in this repo's evidence. The tests exercise it
// against a local server instead. That is a weaker footing than the rest of the
// file has and it is better to name the weakness than to imply a measurement
// that does not exist.
//
// now is a parameter because the date form is relative to it, and a test that
// cannot fix "now" cannot test the date form at all.
func parseRetryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
		// A date in the past means "now", not "negative sleep".
		return 0
	}
	// Unparseable. Ignored rather than guessed at: falling back to the computed
	// backoff is a known-safe number, and inventing one from a malformed header
	// is not.
	return 0
}

// ---------------------------------------------------------------------------
// The error envelope
// ---------------------------------------------------------------------------

// errEnvelope covers both shapes with one struct, which is possible only
// because of a finding in §D11: this gateway returns the **Anthropic** envelope
// for the OpenAI route too. The nested `error` object is the part both agree
// on, so that is the part this reads.
type errEnvelope struct {
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Code    any    `json:"code"` // OpenAI's; sometimes a string, sometimes null — hence `any`
	} `json:"error"`
}

// parseErrorBody extracts what the protocols agree on, and survives the case
// where they agree on nothing.
//
// Three observed shapes have to pass through here (§D11):
//
//	{"type":"error","error":{"type":"AuthError","message":"Invalid API key."}}
//	{"type":"error","error":{"type":"error","message":"Internal server error"}}
//	{"model":"qwen3.7-plus"}                       <- a 400, no envelope at all
//
// The third is why this returns two empty strings rather than an error. A
// missing envelope is not a parse failure to be reported, it is a fact about
// the response, and the caller already keeps the raw body for exactly this
// case. Returning an error here would mean the agent reports "could not parse
// the error" instead of reporting the error.
//
// Served as `Content-Type: text/plain;charset=UTF-8`, incidentally, which is
// why nothing in this function looks at the content type.
func parseErrorBody(raw []byte) (typ, msg string) {
	var env errEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Error == nil {
		return "", ""
	}
	return env.Error.Type, env.Error.Message
}

// ---------------------------------------------------------------------------
// Where to send it instead
// ---------------------------------------------------------------------------

// rung is one provider in the fallback ladder, with the identity and the prices
// that go with it.
type rung struct {
	p    Provider
	info ProviderInfo
}

// ladder is the ordered list of providers a session may use: the configured one
// first, the rest reachable only by a TriageFallback verdict.
//
// Deliberately not a load balancer and deliberately not a circuit breaker.
// Nothing here spreads traffic and the order never changes. Two consequences
// worth stating rather than discovering:
//
//   - It never climbs back. Once a session has fallen to rung 1 it stays there,
//     because "is the primary healthy again" cannot be answered without
//     spending a real call to ask. So a fallback is a downgrade you keep, and
//     that is exactly why every switch emits KindProvider — a session quietly
//     served by the cheaper model for an hour is the failure this design trades
//     for simplicity, and it is only survivable if it is visible.
//
//   - Each rung has its own prices and its own context window. The panel
//     re-prices from the event rather than from startup configuration, because
//     the alternative is a cost report that silently bills the second
//     provider's tokens at the first provider's rates.
//
// The mutex is not decoration. Stage 07's subagents run concurrently and share
// this ladder by pointer — deliberately, because "the endpoint is down" is a
// property of the endpoint and not of whichever agent noticed first, so a
// parent should not have to rediscover a failure its child already paid for.
// Sharing it means two children can call advance() in the same microsecond.
type ladder struct {
	mu    sync.Mutex
	rungs []rung
	cur   int
}

func newLadder(rungs ...rung) *ladder { return &ladder{rungs: rungs} }

// pos returns the current rung index and everything on it, in one lock, because
// a caller that fetched the provider and the index separately could act on two
// different rungs — and the index is what it later passes to advance.
func (l *ladder) pos() (int, Provider, ProviderInfo) {
	if l == nil {
		return 0, nil, ProviderInfo{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.rungs) == 0 {
		return 0, nil, ProviderInfo{}
	}
	return l.cur, l.rungs[l.cur].p, l.rungs[l.cur].info
}

// advance steps off rung `from`, reporting false when there is nowhere to go.
//
// It takes the caller's rung rather than reading its own, and the two lines
// that come of that are the whole concurrency story. Stage 07's subagents run
// concurrently against one endpoint, so a dead provider fails several calls at
// once and every one of them wants to fall back.
//
// Setting `cur = from + 1` is already idempotent for two siblings who failed on
// the same rung: both write the same value. The case that needs the guard has
// three participants and it is a *rewind*, not a double step. A fails on rung 0
// and moves to 1; C fails on 1 and moves to 2; then B — still holding the rung 0
// it read before any of this — asks to fall back. Without the guard it writes
// cur = 1 and sends the next call to a provider two siblings have already given
// up on. With it, B is told *yes* without moving anything, which is the honest
// answer to the question it was actually asking: "is there somewhere else to
// send this?" There is, and the ladder is already there.
func (l *ladder) advance(from int) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur > from {
		return true // a sibling already moved us; that is a success, not a step
	}
	if from+1 >= len(l.rungs) {
		return false
	}
	l.cur = from + 1
	return true
}

// ---------------------------------------------------------------------------
// One attempt
// ---------------------------------------------------------------------------

// modelCall performs one attempt at one model call: render the request, send
// it, parse the stream, and classify anything that goes wrong.
//
// One function, two callers — the agent's turn and stage 05's summariser. They
// were two copies before this stage, and the copies had drifted: the compaction
// copy read no response body, so a failing compaction reported `http 500` and
// nothing else, which is the one thing that cannot be debugged. Sharing the code
// is not really the point. Sharing the *taxonomy* is, because a failure that two
// paths classify differently is a failure you cannot write one policy for.
//
// It rebuilds the request on every attempt rather than reusing one, and that is
// not incidental: an *http.Request's body is a consumed reader after the first
// Do, so a retry that re-sent the same request object would send zero bytes and
// get a 400 back — a bug in the retry that looks exactly like a bug in the
// server.
func modelCall(ctx context.Context, p Provider, httpc *http.Client, bus *Bus, turn int,
	system string, msgs []Msg, tools []Tool, maxTokens int,
	dl deadlines, now func() time.Time) (*CallResult, error) {

	if now == nil {
		now = time.Now
	}

	// The total budget goes on first, so it is the outermost of this call's own
	// clocks and any inner cancellation still reports its own cause. Both
	// wrappers are per ATTEMPT, not per call: a retry that inherited the first
	// attempt's remaining time would get a smaller budget each round and fail
	// faster the more it needed to succeed.
	if dl.total > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeoutCause(ctx, dl.total, errCallTimeout)
		defer stop()
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(context.Canceled)

	req, body, err := p.BuildRequest(ctx, system, msgs, tools, maxTokens)
	if err != nil {
		return nil, &CallError{Phase: phaseBuild, Err: err, Message: err.Error()}
	}
	bus.Emit(Event{Kind: KindRequest, Turn: turn, Request: body})

	started := now()
	resp, err := httpc.Do(req)
	if err != nil {
		// A cancelled request never reaches the wire in any meaningful sense,
		// but the error it returns is *url.Error wrapping context.Canceled and
		// says nothing about which clock fired. The cause does.
		return nil, &CallError{Phase: phaseConnect, Err: err, Message: err.Error(),
			cause: cancelCause(ctx)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		typ, msg := parseErrorBody(raw)
		return nil, &CallError{
			Phase: phaseStatus, Status: resp.StatusCode,
			Type: typ, Message: msg, Body: strings.TrimSpace(string(raw)),
			// The first response header this repo has ever read. Until now the
			// agent could not have honoured a Retry-After if one had arrived,
			// because it never looked.
			RetryAfter: parseRetryAfter(resp.Header, time.Now()),
		}
	}

	// From here on the stream is watched. guardBody owns the watchdog and
	// stopGuard ends it — on every path out, including the happy one, because a
	// goroutine that outlives its call is a leak that multiplies by the number
	// of subagents.
	stream, _, stopGuard := guardBody(ctx, resp.Body, dl.idle, cancel, bus, turn)
	defer stopGuard()

	res, err := p.ParseStream(stream, bus, turn, started)
	if err != nil {
		// Both adapters return a non-nil result alongside a non-nil error, on
		// purpose and with a comment saying so, and every stage before this one
		// bound that value and never read it. Here it becomes CallError.Partial
		// — see that field for why the bill is the reason to keep it.
		//
		// An in-stream error EVENT already arrives as a *CallError from
		// anthropic.go carrying the provider's own error.type. Enriching it
		// rather than wrapping it keeps that type reachable by the classifier,
		// which is the difference between riding out a capacity blip and giving
		// up on one.
		if ce, ok := asCallError(err); ok {
			ce.Partial = res
			return res, ce
		}
		return res, &CallError{Phase: phaseStream, Message: err.Error(), Err: err,
			Partial: res, cause: cancelCause(ctx)}
	}
	return res, nil
}

// cancelCause reports why this call's context ended, or nil if it did not.
//
// It is a one-line wrapper and it stays a function only for the name: `cause:
// cancelCause(ctx)` at four call sites says what is being asked, where
// `cause: context.Cause(ctx)` invites the reader to wonder what happens on a
// context that is still open.
//
// The answer is nothing: context.Cause returns nil until the context is
// cancelled. An earlier version of this guarded the call with a
// `select { case <-ctx.Done(): ... default: return nil }` and a comment
// explaining why that mattered. Mutation testing killed the comment — removing
// the whole select changed no test, because there was no behaviour there to
// change. The guard was restating the standard library's documented contract
// back to itself.
func cancelCause(ctx context.Context) error { return context.Cause(ctx) }

// forCompaction is the same policy with a shorter leash.
//
// Compaction already has a safe failure: the agent continues uncompacted and
// says so on the bus. So giving up early costs little, while retrying hard
// costs a great deal — every attempt re-sends the whole transcript at full
// price, and it does it while the turn that needed the space is still waiting.
// Two attempts and five seconds, whatever the session's policy is.
func (p retryPolicy) forCompaction() retryPolicy {
	if p.attempts > 2 {
		p.attempts = 2
	}
	if p.budget > 5*time.Second {
		p.budget = 5 * time.Second
	}
	return p
}

// buildLadder assembles the ladder from the resolved primary and --fallback.
//
// Every rung is constructed and its key checked at startup, before the first
// request. That is the whole reason this is a function rather than a lazy lookup
// at the moment of failure: a fallback built on demand is a fallback that fails
// on demand, and the moment it is needed is the only moment it exists for. A
// typo in --fallback should cost a startup error, not a dead session during an
// outage.
func buildLadder(pf *providersFile, name string, pcfg providerConfig, p Provider, fallback string, cacheBreakpoints bool) (*ladder, error) {
	describe := func(n string, c providerConfig, pr Provider) ProviderInfo {
		return ProviderInfo{Name: n, Protocol: pr.Protocol(), Model: pr.Model(), Window: c.Window, Prices: c.Prices}
	}
	rungs := []rung{{p: p, info: describe(name, pcfg, p)}}
	seen := map[string]bool{name: true}

	for _, n := range strings.Split(fallback, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if seen[n] {
			// Rejected rather than skipped. A ladder listing the same provider
			// twice reads as extra resilience and delivers none — the second
			// rung fails for exactly the reason the first one did, and the only
			// thing it buys is a longer wait before the session gives up.
			return nil, fmt.Errorf("--fallback lists %q more than once (or it is already the primary)", n)
		}
		seen[n] = true

		c, resolved, err := pf.resolve(n)
		if err != nil {
			return nil, err
		}
		pr, err := c.build(cacheBreakpoints)
		if err != nil {
			return nil, fmt.Errorf("fallback %q: %w", n, err)
		}
		rungs = append(rungs, rung{p: pr, info: describe(resolved, c, pr)})
	}
	return newLadder(rungs...), nil
}

// ---------------------------------------------------------------------------
// The loop
// ---------------------------------------------------------------------------

// retryLoop runs one model call under the policy, and is the only place in this
// agent that acts on a triage verdict.
//
// It takes a closure rather than a request for one reason: **the compaction
// call is a model call too**, and it is the one every agent forgets. Stage 05's
// summariser does its own POST, and before this stage it had its own error
// handling — half of the other one, minus the response body. Both callers now
// get the same decisions from the same code.
//
// The two knobs the callers differ on are the ladder and the policy, and the
// difference is the argument: compaction passes a nil ladder, because switching
// the whole session's provider as a side effect of a summarisation hiccup is
// not a recovery, it is a surprise. Compaction already has a safe failure —
// continue uncompacted — so it wants a short leash and no lasting consequences.
//
// sleep is injected so tests can run the real loop with the real waits and take
// no time at all. Nothing else in this function reads a clock: the budget is
// tracked as a running sum of what it decided to wait, not as a deadline, which
// means the whole thing is deterministic given a deterministic rnd.
func retryLoop(
	ctx context.Context, bus *Bus, turn int, pol retryPolicy, lad *ladder,
	sleep func(context.Context, time.Duration) error, rnd func() float64,
	do func(context.Context, Provider) (*CallResult, error),
) (*CallResult, error) {
	if rnd == nil {
		rnd = rand.Float64
	}
	if sleep == nil {
		sleep = waitFor
	}
	var waited time.Duration
	attempt, perRung := 0, 0

	for {
		attempt++
		perRung++
		at, p, _ := lad.pos()
		res, err := do(ctx, p)
		if err == nil {
			return res, nil
		}

		ce, ok := asCallError(err)
		if !ok {
			// A failure this stage does not model. Returned, not retried: an
			// unclassified failure retried is a failure repeated, and the
			// honest move is to surface the thing we do not understand.
			return res, err
		}

		v := ce.triage()
		bus.Emit(Event{
			Kind:    KindCallError,
			Turn:    turn,
			Status:  ce.Status,
			Phase:   string(ce.Phase),
			ErrType: ce.Type,
			Triage:  string(v),
			Attempt: attempt,
			Text:    ce.Error(),
			// The partial's own accounting, when the stream got far enough to
			// have any. It is almost always empty — usage arrives at the end of
			// a stream, and this stream did not end — which is itself the
			// finding in docs/09-triage.md: the bill for a broken stream is
			// real and unobservable at the same time.
			Usage: partialUsage(ce),
		})

		switch v {
		case TriageFatal:
			return res, err

		case TriageFallback:
			if !lad.advance(at) {
				// Nowhere left to go. The error the caller sees is the last
				// provider's, which is the right one: it is the reason the
				// session cannot continue.
				return res, err
			}
			_, _, info := lad.pos()
			bus.Emit(Event{
				Kind: KindProvider, Turn: turn, Triage: string(TriageFallback),
				Provider: &info,
				Text:     fmt.Sprintf("fell back after %s", ce.Error()),
			})
			perRung = 0
			continue

		case TriageRetry:
			limit := pol.attempts
			if l := ce.leash(); l > 0 && l < limit {
				limit = l
			}
			if perRung >= limit {
				// Out of attempts on this rung. A retryable failure that has
				// run out of retries is worth one look at the ladder before
				// giving up: "the provider is down" and "this provider is
				// down" are different sentences.
				if lad.advance(at) {
					_, _, info := lad.pos()
					bus.Emit(Event{
						Kind: KindProvider, Turn: turn, Triage: string(TriageFallback),
						Provider: &info,
						Text:     fmt.Sprintf("fell back after %d attempts: %s", perRung, ce.Error()),
					})
					perRung = 0
					continue
				}
				return res, fmt.Errorf("%d attempts: %w", perRung, err)
			}

			w := pol.wait(perRung, ce.RetryAfter, rnd)
			if waited+w > pol.budget {
				// The budget is wall clock, not attempts, because the thing a
				// human notices is not "it tried four times", it is "it sat
				// there for two minutes". Reporting the budget by name is
				// deliberate: it is the number they will want to change.
				return res, fmt.Errorf("retry budget %s exhausted after %d attempts: %w", pol.budget, perRung, err)
			}
			waited += w
			bus.Emit(Event{
				Kind: KindRetry, Turn: turn, Attempt: attempt + 1,
				Millis: w.Milliseconds(), Status: ce.Status, ErrType: ce.Type,
				Text: retryWhy(ce),
			})
			if err := sleep(ctx, w); err != nil {
				// Interrupted mid-backoff. Reported as the cancellation it is,
				// not as the provider error that started the wait: a session
				// that ends during a 4s sleep did not end because of an HTTP
				// 503, and the trace should not say it did.
				return res, &CallError{Phase: phaseStream, Err: err,
					Message: err.Error(), cause: cancelCause(ctx)}
			}
			continue
		}
	}
}

// retryWhy is the one-line reason printed next to a wait. It names the source
// of the delay, because "waiting 4s" and "waiting 4s because the server asked
// for 4s" lead to different debugging.
func retryWhy(ce *CallError) string {
	if ce.RetryAfter > 0 {
		return fmt.Sprintf("%s · the server asked for %s", ce.Error(), ce.RetryAfter)
	}
	return ce.Error()
}

// partialUsage reports what a broken attempt managed to account for, or nil.
//
// nil rather than a zero Usage: a zero would print as "0 tokens", which reads
// as "this cost nothing" — and the entire point of keeping the partial is that
// this is the one case where the cost is nonzero and unknown.
func partialUsage(ce *CallError) *Usage {
	if ce.Partial == nil {
		return nil
	}
	if ce.Partial.Usage == (Usage{}) {
		return nil
	}
	u := ce.Partial.Usage
	return &u
}

// names lists the ladder in order, for a status report.
//
// A snapshot, and deliberately not what the fallback logic reads — that goes
// through pos() and advance() under the lock, because a caller acting on a stale
// index is the failure this file was written to prevent.
func (l *ladder) names() []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.rungs))
	for _, r := range l.rungs {
		out = append(out, r.info.Name)
	}
	return out
}
