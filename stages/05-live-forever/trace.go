// Stage 02 — the trace file.
//
// The first subscriber that is not a renderer. It draws nothing; it turns the
// event stream into a file, and that file is what makes everything downstream
// possible: replay without an API key, a cost report you can re-run next week,
// a bug report that is evidence instead of a memory of some scrollback.
//
// The format is JSONL — one JSON object per line — for one reason above all
// others: it is the only text format where an interrupted write costs you the
// last record instead of the whole file. A JSON array needs a closing bracket
// that a killed process never writes, so the file documenting the crash would be
// unparseable *because* of the crash. ReadTrace in replay.go is the other half
// of that bargain.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// TraceWriter appends one event per line to a file. It is a Subscriber, so the
// agent core never learns that it exists.
type TraceWriter struct {
	mu   sync.Mutex
	path string
	f    *os.File

	closed bool

	// err holds the *first* write failure and nothing after it. A writer that
	// reports every failure turns a full disk into ten thousand lines of noise
	// on the terminal the user was trying to read the agent on. The failure is
	// loud exactly once; after that recording degrades quietly and Close reports
	// the damage as one number.
	err     error
	dropped int

	// warn is where that single notice goes. It is a field so a test can assert
	// the "once" in "recorded once" without spraying the test runner's stderr.
	warn func(format string, args ...any)
}

// NewTraceWriter opens path for append-writing one JSON object per line.
func NewTraceWriter(path string) (*TraceWriter, error) {
	// Real traces live in dated directories (traces/2026-08-27/session-3.jsonl),
	// so creating the parent is part of this job rather than every caller's
	// chore.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("trace: cannot create %s: %w", dir, err)
		}
	}

	// O_APPEND, not O_TRUNC: a resumed session extends its own trace instead of
	// deleting it, and under O_APPEND every write lands at the current end of
	// the file as one operation — so two agents pointed at the same trace
	// interleave whole lines rather than overwrite each other's offsets.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("trace: cannot open %s: %w", path, err)
	}
	return &TraceWriter{
		path: path,
		f:    f,
		warn: func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
	}, nil
}

// Path is where the trace is being written, so a renderer can tell the user
// where to find it when the session ends.
func (w *TraceWriter) Path() string { return w.path }

// OnEvent records one event. It cannot fail in any way the caller can observe,
// and that is deliberate.
//
// Bus.Emit dispatches synchronously while holding its own lock. A panic in here
// does not crash "the trace" — it crashes the agent, mid-turn, with a
// half-streamed reply and an unreaped child process. Nothing this file can get
// wrong is worth that, so the whole method is a backstop: it swallows, it
// records, it keeps going. Swallowing errors is normally a bug; in a subscriber
// running inside another component's lock it is the contract.
func (w *TraceWriter) OnEvent(e Event) {
	defer func() {
		// Reached only if the impossible happens — a future field whose
		// MarshalJSON panics, a nil *os.File after a botched refactor. The
		// recover runs after writeEvent's deferred Unlock has already fired, so
		// fail may take the lock again without deadlocking.
		if r := recover(); r != nil {
			w.fail(fmt.Errorf("panic writing event %d (%s): %v", e.Seq, e.Kind, r))
		}
	}()
	w.writeEvent(e)
}

func (w *TraceWriter) writeEvent(e Event) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed || w.err != nil {
		// Already degraded. Count it, so Close can say how much of the session
		// is missing: a trace that is silently short is worse than no trace at
		// all, because it looks complete.
		w.dropped++
		return
	}

	line, err := marshalEvent(e)
	if err != nil {
		// In practice this means Request holds bytes that are not valid JSON —
		// a provider body captured verbatim, say. Drop the payload and keep the
		// event: a trace missing one request body is still a trace, whereas a
		// hole in the Seq sequence is a mystery nobody can solve six months
		// later.
		degraded := e
		degraded.Request = json.RawMessage(`{"trace_error":"request body was not valid JSON and was dropped"}`)
		line, err = marshalEvent(degraded)
		if err != nil {
			w.failLocked(fmt.Errorf("encode event %d (%s): %w", e.Seq, e.Kind, err))
			return
		}
	}
	line = append(line, '\n')

	// Durability. The bytes go straight to the file: there is no bufio.Writer in
	// this path, and its absence is the whole design.
	//
	// A 64KB buffer would batch a few hundred events into one syscall and lose
	// every one of them when the agent is killed — precisely the moment the
	// trace existed to explain. Unbuffered costs one write(2) per event, a few
	// microseconds into the kernel's page cache, set against model calls
	// measured in hundreds of milliseconds. The trade is not close.
	//
	// We stop deliberately short of f.Sync(). fsync additionally survives a
	// power cut, and costs real disk latency (~0.1ms on an SSD, ~10ms on a
	// spinning disk or a network mount) on *every text delta*, inside the bus
	// lock: three orders of magnitude more, to defend against a failure mode
	// (the machine dies) far rarer than the one already covered (the process
	// dies). Once this Write returns the data survives SIGKILL, panic and
	// os.Exit with no further help from us.
	//
	// One Write per line also keeps a line atomic under O_APPEND, which is what
	// stops a concurrent writer from splicing an unparseable record into the
	// middle of the file.
	if _, err := w.f.Write(line); err != nil {
		w.failLocked(fmt.Errorf("write to %s: %w", w.path, err))
	}
}

// Note the shape of what is *not* here: no goroutine, no channel, no queue.
//
// "Never block the bus" is usually answered with an async writer, and a queue
// has exactly two behaviours when it fills — block the producer (the thing we
// were avoiding) or drop events (a trace that lies by omission, silently, under
// exactly the load you most wanted recorded). A local append never waits
// unboundedly, so the synchronous version has neither problem. The rule the bus
// actually needs is "no unbounded wait", not "no I/O": no fsync, no network, no
// lock held across a channel send.

func (w *TraceWriter) fail(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failLocked(err)
}

func (w *TraceWriter) failLocked(err error) {
	w.dropped++
	if w.err != nil {
		return // already reported once; stay quiet and keep counting
	}
	w.err = err
	if w.warn != nil {
		w.warn("trace: %v — recording is disabled for the rest of this session", err)
	}
}

// Close flushes nothing (nothing is buffered) and reports the damage.
//
// It is idempotent because main defers it while a signal handler may also call
// it, and a second Close that returned an error would make an orderly shutdown
// look like a failure.
func (w *TraceWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true

	cerr := w.f.Close()
	if w.err != nil {
		return fmt.Errorf("trace %s: %d event(s) went unrecorded after the first failure: %w",
			w.path, w.dropped, w.err)
	}
	return cerr
}

// marshalEvent encodes one event with HTML escaping OFF.
//
// json.Marshal escapes <, > and & into <, > and &, and — this
// is the part that bites — encoding/json applies that *inside a
// json.RawMessage* too, while compacting it. Event.Request is a RawMessage
// holding the exact bytes the adapter posted, and both adapters go out of their
// way to encode with SetEscapeHTML(false) precisely because a shell agent's
// requests are mostly `2>&1`, `>/tmp/out` and `<<EOF`.
//
// So without this, everything the adapters were careful about is undone one
// layer later, in the file:
//
//	posted:  {"command":"ls 2>&1 <in"}
//	traced:  {"command":"ls 2>&1 <in"}
//
// Nothing errors, the JSON is equivalent, and every consumer that decodes it
// gets the right string back. What breaks is the claim: events.go calls
// Request "the exact bytes about to be sent", stage 06's wire view promises
// "byte for byte", and a byte-level comparison between a live run and a
// replayed one shows a diff that is entirely this. A trace is evidence; the
// moment it is not byte-identical it stops being evidence about bytes.
//
// Found in a real trace, where all 24 recorded requests carried the escapes.
func marshalEvent(e Event) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return nil, err
	}
	// Encoder.Encode appends a newline that Marshal does not; the caller adds
	// its own, and two would be a blank line in the middle of a JSONL file.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
