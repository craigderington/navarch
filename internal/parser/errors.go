package parser

import (
	"fmt"
	"strings"
)

// ValidationError is a single rejected directive, addressed by its path
// in the compose file so the CLI can point at the offending line.
type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// ValidationErrors is returned when a compose file is structurally valid
// but violates platform constraints. All violations are collected rather
// than failing on the first, so the user fixes everything in one pass.
type ValidationErrors []ValidationError

func (e ValidationErrors) Error() string {
	if len(e) == 0 {
		return "no validation errors"
	}
	parts := make([]string, len(e))
	for i, ve := range e {
		parts[i] = ve.Error()
	}
	return fmt.Sprintf("%d validation error(s):\n  %s", len(e), strings.Join(parts, "\n  "))
}
