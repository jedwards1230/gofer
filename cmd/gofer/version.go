package main

import (
	"fmt"
	"io"
)

// version is the gofer build version. It is overridden at release build time
// via -ldflags "-X main.version=<v>".
var version = "dev"

// runVersion prints the build version.
func runVersion(w io.Writer) {
	_, _ = fmt.Fprintln(w, version)
}
