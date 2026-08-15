// Command composectl is the operator CLI for the Navarch / composectl control plane.
package main

import (
	"os"

	"github.com/craig/composectl/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
