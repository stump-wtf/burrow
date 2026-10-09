// Package buildinfo carries version metadata stamped in at link time.
package buildinfo

// Version is set by the Makefile's -ldflags build; "dev" in a bare `go build`.
var Version = "dev"
