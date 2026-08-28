// A module of its own, on purpose.
//
// If these files were part of bash-is-all-you-need's module, `go build ./...`,
// `go vet ./...` and `go test ./...` at the repo root would pick them up, and
// the repo's rule that stage 08 is the only place a dependency appears would be
// quietly false. Go's package patterns stop at a nested go.mod, so this costs
// the site nothing and costs the teaching repo nothing.
module wasmshell

go 1.24.0

require mvdan.cc/sh/v3 v3.12.0

require (
	golang.org/x/sys v0.33.0 // indirect
	golang.org/x/term v0.32.0 // indirect
)
