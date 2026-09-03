// Package schema defines the versioned metadata document, its validation
// rules, and dot-path access. The document is kept as a raw map so fields
// written by newer versions of the tool round-trip intact.
package schema

import (
	"encoding/json"
	"fmt"
	"strings"
)

// CurrentVersion is the highest schemaVersion this binary understands.
const CurrentVersion = 1

// DefaultStates is the default allowed set for the "state" field,
// overridable via `git config track.states`.
var DefaultStates = []string{"todo", "in-progress", "blocked", "review", "done"}

// ErrTooNew is returned when a document's schemaVersion is higher than
// CurrentVersion. Callers must refuse to write in that case.
type ErrTooNew struct {
	Version int
}

func (e *ErrTooNew) Error() string {
	return fmt.Sprintf("metadata schemaVersion %d is newer than this binary supports (%d); upgrade git-track", e.Version, CurrentVersion)
}

// Doc is a metadata document. Unknown fields are preserved verbatim.
type Doc map[string]any

// New returns an empty document at the current schema version.
func New() Doc {
	return Doc{"schemaVersion": float64(CurrentVersion)}
}

// Parse decodes a document from JSON. A missing schemaVersion is treated as
// version 1.
func Parse(data []byte) (Doc, error) {
	var d Doc
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("invalid metadata JSON: %w", err)
	}
	if d == nil {
		return nil, fmt.Errorf("metadata document must be a JSON object")
	}
	if _, ok := d["schemaVersion"]; !ok {
		d["schemaVersion"] = float64(1)
	}
	return d, nil
}

// Marshal encodes the document as indented JSON with a trailing newline.
// encoding/json sorts map keys, so output is deterministic.
func (d Doc) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Version returns the document's schemaVersion.
func (d Doc) Version() int {
	if v, ok := d["schemaVersion"].(float64); ok {
		return int(v)
	}
	return 1
}

// CheckWritable returns ErrTooNew if the document was written by a newer
// schema version than this binary supports.
func (d Doc) CheckWritable() error {
	if v := d.Version(); v > CurrentVersion {
		return &ErrTooNew{Version: v}
	}
	return nil
}

// Get resolves a dot path ("agent.lockedBy") and reports whether it exists.
func (d Doc) Get(path string) (any, bool) {
	parts := strings.Split(path, ".")
	var cur any = map[string]any(d)
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Set writes a value at a dot path, creating intermediate objects as needed.
func (d Doc) Set(path string, value any) error {
	parts := strings.Split(path, ".")
	m := map[string]any(d)
	for i, p := range parts[:len(parts)-1] {
		next, ok := m[p]
		if !ok {
			child := map[string]any{}
			m[p] = child
			m = child
			continue
		}
		child, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("cannot set %q: %q is not an object", path, strings.Join(parts[:i+1], "."))
		}
		m = child
	}
	m[parts[len(parts)-1]] = value
	return nil
}

// Unset removes the value at a dot path; returns false if it didn't exist.
func (d Doc) Unset(path string) bool {
	parts := strings.Split(path, ".")
	m := map[string]any(d)
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			return false
		}
		m = next
	}
	last := parts[len(parts)-1]
	if _, ok := m[last]; !ok {
		return false
	}
	delete(m, last)
	return true
}

// Clone returns a deep copy of the document.
func (d Doc) Clone() Doc {
	b, _ := json.Marshal(d)
	var c Doc
	_ = json.Unmarshal(b, &c)
	return c
}

// Validate checks the types of known fields and the state value. Unknown
// fields are never rejected. states is the allowed state set (empty slice
// disables state validation).
func Validate(d Doc, states []string) error {
	if err := d.CheckWritable(); err != nil {
		return err
	}
	if v, ok := d["issue"]; ok && v != nil {
		if f, ok := v.(float64); !ok || f != float64(int64(f)) {
			return fmt.Errorf("field \"issue\" must be an integer")
		}
	}
	for _, key := range []string{"title", "state", "context", "updatedAt", "updatedBy"} {
		if v, ok := d[key]; ok && v != nil {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("field %q must be a string", key)
			}
		}
	}
	for _, key := range []string{"next", "labels", "links"} {
		if v, ok := d[key]; ok && v != nil {
			arr, ok := v.([]any)
			if !ok {
				return fmt.Errorf("field %q must be an array of strings", key)
			}
			for _, item := range arr {
				if _, ok := item.(string); !ok {
					return fmt.Errorf("field %q must be an array of strings", key)
				}
			}
		}
	}
	if v, ok := d["agent"]; ok && v != nil {
		if _, ok := v.(map[string]any); !ok {
			return fmt.Errorf("field \"agent\" must be an object")
		}
	}
	if len(states) > 0 {
		if v, ok := d["state"].(string); ok && v != "" {
			valid := false
			for _, s := range states {
				if v == s {
					valid = true
					break
				}
			}
			if !valid {
				return fmt.Errorf("invalid state %q (allowed: %s; configure with `git config track.states`)", v, strings.Join(states, ", "))
			}
		}
	}
	return nil
}
