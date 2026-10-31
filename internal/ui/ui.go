// Package ui is bbctl's output contract. Results go to stdout (JSON with
// --json, text otherwise); progress and Terraform's own logs go to stderr, so
// a caller can always parse stdout. Failures carry a hint and an exit code.
package ui

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// JSON is set by the --json flag.
var JSON bool

// Verbose turns on progress lines (and Terraform's output) on stderr.
var Verbose bool

// Logf prints a progress line to stderr, with --verbose.
func Logf(format string, a ...any) {
	if !Verbose {
		return
	}
	fmt.Fprintf(os.Stderr, "› "+format+"\n", a...)
}

// Exit codes. Stable: agents and scripts may branch on them.
const (
	ExitError     = 1 // anything not covered below
	ExitUsage     = 2 // bad flags or invalid bbctl.yaml
	ExitAWS       = 3 // missing credentials, permissions or AWS resources
	ExitTerraform = 4 // a terraform command failed
	ExitAborted   = 5 // the user declined a confirmation
)

// Error is a failure with a suggested next step.
type Error struct {
	Code    int      `json:"code"`
	Message string   `json:"error"`
	Hint    string   `json:"hint,omitempty"`
	Details []string `json:"details,omitempty"`
	Err     error    `json:"-"`
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Err }

func Errf(code int, hint, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...), Hint: hint}
}

// Wrap attaches a code and hint to err.
func Wrap(code int, hint string, err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: code, Message: strings.TrimSpace(err.Error()), Hint: hint, Err: err}
}

// PrintError reports err on stdout (JSON) or stderr (text) and returns the
// process exit code.
func PrintError(err error) int {
	e := Wrap(ExitError, "", err)
	if JSON {
		_ = writeJSON(os.Stdout, e)
		return e.Code
	}
	fmt.Fprintf(os.Stderr, "error: %s\n", e.Message)
	for _, d := range e.Details {
		fmt.Fprintf(os.Stderr, "  - %s\n", d)
	}
	if e.Hint != "" {
		fmt.Fprintf(os.Stderr, "hint: %s\n", e.Hint)
	}
	return e.Code
}

// Result prints v as JSON with --json, otherwise calls text.
func Result(v any, text func(w io.Writer)) error {
	if JSON {
		return writeJSON(os.Stdout, v)
	}
	text(os.Stdout)
	return nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false) // keep <name> and -> readable
	return enc.Encode(v)
}

// stdin is shared by every prompt: a reader per prompt would buffer past its
// line and drop the answers to the next questions when they are pasted at once.
var stdin = bufio.NewReader(os.Stdin)

// Interactive reports whether bbctl can prompt the user.
func Interactive() bool {
	return !JSON && term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

// Confirm asks a yes/no question on stderr. Non-interactive sessions get an
// error telling them which flag skips the prompt.
func Confirm(question, flag string) error {
	if !Interactive() {
		return Errf(ExitUsage, "re-run with "+flag+" to proceed without a prompt", "confirmation required: %s", question)
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", question)
	line, _ := stdin.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return Errf(ExitAborted, "", "aborted")
}

// Prompt reads a line from the user, returning def when empty.
func Prompt(question, def string) string {
	if !Interactive() {
		return def
	}
	if def != "" {
		fmt.Fprintf(os.Stderr, "%s [%s]: ", question, def)
	} else {
		fmt.Fprintf(os.Stderr, "%s: ", question)
	}
	line, _ := stdin.ReadString('\n')
	if line = strings.TrimSpace(line); line != "" {
		return line
	}
	return def
}
