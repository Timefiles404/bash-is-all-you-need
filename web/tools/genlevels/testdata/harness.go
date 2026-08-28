// Shown to the learner, read-only, in a collapsed pane. Everything above this
// line in the editor is theirs; this is what runs it.
//
// A learner who cannot see the harness cannot tell whether the output is theirs,
// so it is visible rather than hidden — but it is not editable, because a level
// whose harness can be changed is a level with no fixed expectation.
package main

import (
	"fmt"
	"os"
)

func main() {
	data, err := os.ReadFile("/work/input.txt")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, cut := truncate(string(data), 512)
	fmt.Println(out)
	fmt.Printf("\n--- %d bytes in, %d out, truncated=%v ---\n", len(data), len(out), cut)
}
