// Package runorigin defines caller attribution, never caller authorization.
package runorigin

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/oklog/ulid/v2"
)

// Caller is optional, non-secret metadata asserted by the submitting client.
// The named owner validates its own task and generation before using it.
type Caller struct {
	Namespace  string `json:"namespace"`
	Home       string `json:"home"`
	Task       string `json:"task"`
	Generation string `json:"generation"`
}

// Reference is an opaque local producer reference, not a lease or credential.
type Reference string

const (
	ReferenceEnv = "NO_MISTAKES_EXECUTION_REF"
	SocketEnv    = "NO_MISTAKES_EXECUTION_SOCKET"
)

func (r Reference) Validate() error {
	if _, err := ulid.ParseStrict(string(r)); err != nil {
		return fmt.Errorf("invalid execution reference")
	}
	return nil
}

func (c Caller) Validate() error {
	for _, v := range []string{c.Namespace, c.Home, c.Task, c.Generation} {
		if v == "" || len(v) > 4096 || strings.TrimSpace(v) != v || !utf8.ValidString(v) || strings.ContainsFunc(v, unicode.IsControl) {
			return fmt.Errorf("caller origin requires bounded nonempty namespace, home, task and generation without control characters")
		}
	}
	if !filepath.IsAbs(c.Home) || filepath.Clean(c.Home) != c.Home {
		return fmt.Errorf("caller origin home must be an absolute clean path")
	}
	return nil
}

// Decode accepts only the published caller fields.
func Decode(data []byte) (*Caller, error) {
	if len(data) > 16*1024 {
		return nil, fmt.Errorf("caller origin exceeds 16 KiB")
	}
	var c Caller
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("invalid caller origin: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("caller origin must contain one JSON object")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Execution is the durable association returned by the owning daemon.
// SourceHead remains the submitted source commit when a retry validates a
// later pipeline head. Status is current and must be checked by consumers.
type Execution struct {
	Reference         Reference `json:"reference"`
	RunID             string    `json:"run_id"`
	SourceCheckout    string    `json:"source_checkout"`
	SourceHead        string    `json:"source_head"`
	ExecutionCheckout string    `json:"execution_checkout"`
	Caller            *Caller   `json:"caller_origin"`
	Status            string    `json:"run_status"`
}

// Stamp removes ambient/overlay references even for unattributed runs. Only
// values held in the immutable run overlay can establish this association.
func Stamp(base []string, pinned runenv.Overlay) []string {
	stamp := runenv.Overlay{Unset: []string{ReferenceEnv, SocketEnv}}
	if ref := pinned.Set[ReferenceEnv]; ref != "" {
		stamp.Set = map[string]string{ReferenceEnv: ref, SocketEnv: pinned.Set[SocketEnv]}
	}
	return stamp.Apply(base)
}

// Submission is frozen by the submitting client and accepted once per run.
// SourceCheckout is its physical Git top-level, never a registered-repo path.
type Submission struct {
	SourceCheckout string `json:"source_checkout"`
	Caller         Caller `json:"caller_origin"`
}

func (s *Submission) Validate() error {
	if s == nil {
		return nil
	}
	if err := s.Caller.Validate(); err != nil {
		return err
	}
	if !filepath.IsAbs(s.SourceCheckout) || filepath.Clean(s.SourceCheckout) != s.SourceCheckout || len(s.SourceCheckout) > 4096 || strings.ContainsFunc(s.SourceCheckout, unicode.IsControl) {
		return fmt.Errorf("source checkout must be a bounded absolute clean path")
	}
	return nil
}

// Origin is stored only on the run that accepted it. A rerun copies the
// original source facts and binds its own execution checkout.
type Origin struct {
	Submission Submission `json:"submission"`
	SourceHead string     `json:"source_head"`
}

func (b Origin) Value() (driver.Value, error) {
	data, err := json.Marshal(b)
	return string(data), err
}

func (b *Origin) Scan(src any) error {
	var data []byte
	switch value := src.(type) {
	case string:
		data = []byte(value)
	case []byte:
		data = value
	default:
		return fmt.Errorf("invalid persisted caller origin")
	}
	if err := decodeStrict(data, b); err != nil {
		return fmt.Errorf("invalid persisted caller origin: %w", err)
	}
	if err := b.Submission.Validate(); err != nil {
		return err
	}
	if b.SourceHead == "" {
		return fmt.Errorf("incomplete persisted caller origin association")
	}
	return nil
}

func (c *Caller) UnmarshalJSON(data []byte) error {
	type wire Caller
	var value wire
	if err := decodeStrict(data, &value); err != nil {
		return err
	}
	*c = Caller(value)
	return c.Validate()
}

func (s *Submission) UnmarshalJSON(data []byte) error {
	type wire Submission
	var value wire
	if err := decodeStrict(data, &value); err != nil {
		return err
	}
	*s = Submission(value)
	return s.Validate()
}

func decodeStrict(data []byte, value any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("caller origin must contain one JSON object")
	}
	return nil
}
