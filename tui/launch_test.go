package tui

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// oneByteReader hands out a single byte per Read and counts the calls.
//
// bufio.Reader fills once per Read, so the count is exactly how many bytes
// holdOpen's line read consumed — which is the half of "reads nothing" that a
// buffer's remaining length cannot prove, since a bufio.Reader would happily
// swallow the whole stream into its own buffer.
type oneByteReader struct {
	data  string
	pos   int
	reads int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	r.reads++
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}

// On a terminal the user opened themselves, pausing would make the binary
// unusable in a script — so the call has to be inert, not merely quiet.
func TestHoldOpenWritesNothingAndReadsNothingWhenTheWindowIsNotOurs(t *testing.T) {
	var out bytes.Buffer
	in := &oneByteReader{data: "y\nleft over\n"}

	holdOpen(&out, in, false)

	if out.Len() != 0 {
		t.Errorf("holdOpen wrote %q, expected nothing at all", out.String())
	}
	if in.reads != 0 {
		t.Errorf("holdOpen read the input %d times, expected not to touch it", in.reads)
	}
	if in.pos != 0 {
		t.Errorf("holdOpen consumed %d bytes of input, expected 0", in.pos)
	}
}

// Started from Explorer, Windows destroys the console the moment the process
// returns, so an error printed on the way out is displayed for a few
// microseconds. Every report of "it just flashes and closes" is this.
func TestHoldOpenPromptsAndConsumesOneLineWhenWeOwnTheWindow(t *testing.T) {
	var out bytes.Buffer
	in := &oneByteReader{data: "y\nleft over\n"}

	holdOpen(&out, in, true)

	if !strings.Contains(out.String(), "press Enter to close this window") {
		t.Errorf("holdOpen wrote %q, expected it to say how to close the window", out.String())
	}
	if !strings.HasPrefix(out.String(), "\n") {
		t.Errorf("holdOpen wrote %q, expected a leading newline so the prompt does not land on the last line of output", out.String())
	}
	if in.pos != len("y\n") {
		t.Errorf("holdOpen consumed %d bytes, expected the %d of the first line and no more", in.pos, len("y\n"))
	}
}

// This runs on the way out, possibly after a failure that had nothing to do with
// the terminal. An input that is already closed must not turn a report into a
// hang.
func TestHoldOpenReturnsOnAClosedInputRatherThanWaitingForALineThatCannotArrive(t *testing.T) {
	var out bytes.Buffer
	in := &oneByteReader{data: "no newline here"}

	done := make(chan struct{})
	go func() {
		holdOpen(&out, in, true)
		close(done)
	}()
	<-done // if holdOpen ever blocks on EOF, the test times out here

	if in.pos != len(in.data) {
		t.Errorf("holdOpen stopped after %d of %d bytes, expected it to read to the end", in.pos, len(in.data))
	}
}

// An empty input is the same case with nothing in it: one Read, EOF, return.
func TestHoldOpenReturnsOnAnEmptyInput(t *testing.T) {
	var out bytes.Buffer
	in := &oneByteReader{}

	holdOpen(&out, in, true)

	if !strings.Contains(out.String(), "press Enter") {
		t.Errorf("holdOpen wrote %q, expected the prompt even when the answer cannot come", out.String())
	}
}
