package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// isTerminal reports whether f is an interactive terminal.
//
// It is what decides between prompting and reading stdin: `navarch login` in a
// pipeline must take the token from the pipe, and the same command run by a
// person must not sit waiting on a stdin nobody is going to type into.
func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// promptSecret reads a line from the terminal without echoing it.
//
// Echo suppression is the whole point: a token typed in the clear is a token in
// a screen recording, in a shoulder-surfer's memory, and in a scrollback buffer
// somebody later pastes into a bug report. term.ReadPassword restores the
// terminal state itself, including on interrupt, which is why this does not
// shell out to `stty` — a crashed process that has turned echo off leaves the
// operator with a terminal that appears broken.
func promptSecret(out io.Writer, prompt string) (string, error) {
	fmt.Fprint(out, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(out)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// readLine reads one plain line, for prompts that are not secrets.
func readLine(prompt string, out io.Writer) (string, error) {
	fmt.Fprint(out, prompt)
	s, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && s == "" {
		return "", err
	}
	return strings.TrimSpace(s), nil
}
