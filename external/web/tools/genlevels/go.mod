// A module of its own, for the same reason web/tools/wasmshell is: Go's package
// patterns stop at a nested go.mod, so `go build ./...` at the repository root
// never sees these files and the repo's own module stays exactly as it is.
//
// No requirements. go/parser, go/printer and go/types are standard library.
module genlevels

go 1.24.0
