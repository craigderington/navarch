// Command navarch is the operator CLI for the Navarch control plane.
//
// The directory is named for the binary, not the module: the import path stays
// github.com/craig/composectl so the rename does not churn every internal
// package, while `go build ./cmd/navarch` still produces a `navarch`.
package main

import (
	"os"

	"github.com/craig/composectl/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
