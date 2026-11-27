package threads

// Named non-goals (PAR-182, decision D-15, security position S17).
//
// A non-goal is not a gap: the matrix carries the row, the refusal names it, and
// a workflow that asks for one is told so rather than waiting forever or
// silently degrading. The three the operator approved on 2026-11-26:
//
//	LXC and unisolated host runner backends   — no isolation story
//	hosted Windows/macOS runners              — no fleet, no equivalent isolation
//	artifact attestations                     — spindle operates no sigstore root
//
// The signatory is the operator's D-15 decision (docs/features/21_parity_master.md
// §7.18); the matrix row carries `approved_by`. Runner-label backends are refused
// in labels.go, the attestations permission scope in workflow/permissions.go, and
// a workflow `runs-on` naming a hosted Windows/macOS label gets the queue reason
// from nonGoalLabelReason below. A future managed pool does not change any of
// these: none is an implementation shortcut, each is a promise the instance does
// not make.

import (
	"strings"
)

// NonGoal is one signed refusal, safe to render on a page or print in a CLI.
type NonGoal struct {
	// Name is the capability someone might ask for ("hosted Windows/macOS
	// runners").
	Name string `json:"name"`
	// Ask is an example spelling a workflow would use.
	Ask string `json:"ask"`
	// Reason is why spindle does not provide it.
	Reason string `json:"reason"`
	// Refusal names the error/queue reason text a user actually sees.
	Refusal string `json:"refusal"`
	// Decision is the operator decision that signed it.
	Decision string `json:"decision"`
	// ApprovedBy is the signatory (the operator, by decision date).
	ApprovedBy string `json:"approved_by"`
}

// NonGoalLXC is the LXC/unisolated-host backend refusal (PAR-182/D-15).
const NonGoalLXC = "lxc-host-backends"

// NonGoalHostedWindowsMacOS is the hosted Windows/macOS refusal (PAR-182/D-15).
const NonGoalHostedWindowsMacOS = "hosted-windows-macos"

// NonGoalAttestations is the artifact-attestation refusal (PAR-182/D-15).
const NonGoalAttestations = "artifact-attestations"

// NonGoals is the signed table (sorted by Name for deterministic rendering).
func NonGoals() []NonGoal {
	return []NonGoal{
		{
			Name:       "LXC and unisolated host runner backends",
			Ask:        "`labels: [ubuntu-latest:lxc://template]` or `ubuntu-latest:host`",
			Reason:     "a job that is a process on the operator's machine has no isolation story between tenants",
			Refusal:    "the LXC backend and unisolated host execution are documented non-goals (D-15/PAR-182); declare a docker:// image",
			Decision:   "D-15",
			ApprovedBy: "operator, 2026-11-26",
		},
		{
			Name:       "Hosted Windows and macOS runners",
			Ask:        "`runs-on: windows-latest` or `runs-on: macos-14`",
			Reason:     "spindle operates no Windows/macOS fleet and cannot isolate one the way it isolates Linux containers",
			Refusal:    "hosted Windows/macOS runners are a documented non-goal (D-15/PAR-182); no spindle runner can serve this label",
			Decision:   "D-15",
			ApprovedBy: "operator, 2026-11-26",
		},
		{
			Name:       "Artifact attestations",
			Ask:        "`permissions: {attestations: write}` or `uses: actions/attest-build-provenance`",
			Reason:     "attestations require a sigstore trust root spindle does not operate",
			Refusal:    "artifact attestations are a documented non-goal (D-15/PAR-182)",
			Decision:   "D-15",
			ApprovedBy: "operator, 2026-11-26",
		},
	}
}

// nonGoalLabelReason names the non-goal when a job's `runs-on` asks for a hosted
// Windows/macOS label and no runner can match it. Empty for any other label set:
// an unmatched linux label is an ordinary "no runner matches" reason.
func nonGoalLabelReason(labels []string) string {
	for _, raw := range labels {
		label := strings.ToLower(strings.TrimSpace(raw))
		if strings.HasPrefix(label, "windows-") || strings.HasPrefix(label, "macos-") || label == "windows" || label == "macos" {
			return NonGoals()[1].Refusal
		}
	}
	return ""
}

// RefuseNonGoal names a refusal for a code path that is asked for a non-goal
// capability directly (a runner backend, an attestation action). It returns nil
// for anything else, so callers can gate cheaply.
func RefuseNonGoal(ask string) error {
	switch strings.ToLower(strings.TrimSpace(ask)) {
	case NonGoalLXC, "lxc", "host":
		return &NonGoalError{NonGoal: NonGoals()[0]}
	case NonGoalHostedWindowsMacOS, "windows", "macos", "windows-latest", "macos-latest":
		return &NonGoalError{NonGoal: NonGoals()[1]}
	case NonGoalAttestations, "attestations", "attest-build-provenance":
		return &NonGoalError{NonGoal: NonGoals()[2]}
	}
	return nil
}

// NonGoalError is the typed refusal (ErrInvalid-compatible for HTTP mapping).
type NonGoalError struct {
	NonGoal NonGoal
}

func (e *NonGoalError) Error() string {
	return "non-goal: " + e.NonGoal.Refusal + " (" + e.NonGoal.Decision + ")"
}

func (e *NonGoalError) Unwrap() error { return ErrInvalid }

// HostedNonGoalLabels are the `runs-on` spellings the non-goal covers (for the
// docs test and the queue-reason test).
func HostedNonGoalLabels() []string {
	return []string{"windows", "windows-latest", "windows-2022", "windows-2025", "macos", "macos-latest", "macos-13", "macos-14", "macos-15"}
}
