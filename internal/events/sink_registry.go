// sink_registry.go — the built-in sink-kind registry (14_extensibility.md
// §14.6). The bridge loop is frozen; a sink kind is a constructor, and the
// documented set is pinned by TestDocumentedSinkKindsAreConstructible, which
// also reads the doc so the registry and the doc cannot disagree.
package events

import (
	"errors"
	"fmt"
	"strings"
)

// Sink kind names (the documented built-ins).
const (
	KindWebhook = "webhook"
	KindJSONL   = "jsonl"
)

// Named sink-construction errors (fail closed at boot: a [[events.sinks]]
// entry that cannot be built stops the boot with the offending index).
var (
	ErrUnknownSinkKind = errors.New("events: unknown sink kind")
	ErrSinkSpec        = errors.New("events: invalid sink spec")
)

// SinkSpec is the resolved form of one `[[events.sinks]]` entry. Secret is
// already resolved from secret_env by the composition (the registry never
// reads the environment).
type SinkSpec struct {
	Name     string
	Kind     string
	URL      string
	Secret   string
	Path     string
	MaxBytes int64
	Keep     int
}

// SinkKinds returns the documented built-in sink kinds (stable order).
func SinkKinds() []string { return []string{KindWebhook, KindJSONL} }

// NewSink constructs one built-in sink. Unknown kinds are
// ErrUnknownSinkKind; a known kind with a missing required key is ErrSinkSpec.
func NewSink(spec SinkSpec) (Sink, error) {
	switch strings.TrimSpace(spec.Kind) {
	case KindWebhook:
		if strings.TrimSpace(spec.URL) == "" {
			return nil, fmt.Errorf("%w: webhook sink needs url", ErrSinkSpec)
		}
		return &WebhookSink{URL: spec.URL, Secret: spec.Secret, Label: spec.Name}, nil
	case KindJSONL:
		if strings.TrimSpace(spec.Path) == "" {
			return nil, fmt.Errorf("%w: jsonl sink needs path", ErrSinkSpec)
		}
		return &JSONLSink{
			Path: spec.Path, MaxBytes: spec.MaxBytes, Keep: spec.Keep, Label: spec.Name,
		}, nil
	default:
		return nil, fmt.Errorf("%w: %q (built-ins: %s)", ErrUnknownSinkKind, spec.Kind, strings.Join(SinkKinds(), ", "))
	}
}
