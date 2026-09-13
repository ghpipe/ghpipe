// Package result defines the single machine-readable result envelope and the
// exit-code contract shared by every command.
package result

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// SchemaVersion is the envelope schema version.
const SchemaVersion = 1

// Exit codes. The distinction that matters most is 1 (definitely failed, no
// side effect) versus 2 (outcome unknown - the write may have happened and must
// be reconciled by re-reading remote evidence, never replayed).
const (
	ExitOK         = 0
	ExitFailed     = 1
	ExitUnknown    = 2
	ExitUsageError = 3
)

// Status is the outcome of a command.
type Status string

const (
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusUnknown   Status = "unknown"
	StatusReady     Status = "ready"
	StatusPending   Status = "pending"
)

// Target identifies the object a command acted on.
type Target struct {
	Kind   string `json:"kind"`
	Number int    `json:"number,omitempty"`
}

// Error is a redacted error description: never contains tokens, raw URLs,
// response bodies or user paths.
type Error struct {
	Category string `json:"category"`
	Phase    string `json:"phase,omitempty"`
	Status   any    `json:"status,omitempty"`
	Method   string `json:"method,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// Envelope is the one JSON object every --json command prints.
type Envelope struct {
	SchemaVersion int     `json:"schema_version"`
	Command       string  `json:"command"`
	Status        Status  `json:"status"`
	Repository    string  `json:"repository,omitempty"`
	Target        *Target `json:"target,omitempty"`
	Data          any     `json:"data,omitempty"`
	Error         *Error  `json:"error"`
	Next          []string `json:"next"`
	ObservedAt    string  `json:"observed_at"`
}

// New builds an envelope with the current observation time.
func New(command string, status Status) *Envelope {
	return &Envelope{
		SchemaVersion: SchemaVersion,
		Command:       command,
		Status:        status,
		Error:         nil,
		Next:          []string{},
		ObservedAt:    time.Now().UTC().Format(time.RFC3339),
	}
}

// Fail attaches a redacted error and returns the envelope.
func (e *Envelope) Fail(category, phase string) *Envelope {
	e.Status = StatusFailed
	e.Error = &Error{Category: category, Phase: phase}
	return e
}

// ExitCode maps the envelope status onto the process exit code.
func (e *Envelope) ExitCode() int {
	switch e.Status {
	case StatusSucceeded, StatusReady:
		return ExitOK
	case StatusFailed:
		return ExitFailed
	case StatusUnknown, StatusPending:
		return ExitUnknown
	default:
		return ExitFailed
	}
}

// WriteJSON prints the envelope as the sole stdout payload.
func (e *Envelope) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(e)
}

// UsageError reports a bad invocation (no network, no writes happened).
func UsageError(command, detail string) *Envelope {
	e := New(command, StatusFailed)
	e.Error = &Error{Category: "usage", Phase: "precondition", Detail: detail}
	return e
}

// String renders a short human line for stderr.
func (e *Envelope) String() string {
	if e.Error == nil {
		return fmt.Sprintf("%s: %s", e.Command, e.Status)
	}
	return fmt.Sprintf("%s: %s (%s)", e.Command, e.Status, e.Error.Category)
}
