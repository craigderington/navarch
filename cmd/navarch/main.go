// Command navarch is the operator CLI for the Navarch control plane.
//
// The directory is named for the binary so `go build ./cmd/navarch` and
// `go install github.com/craigderington/navarch/cmd/navarch@latest` both
// produce a `navarch`.
package main

import (
	"os"

	"github.com/craigderington/navarch/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
