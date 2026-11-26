package threads

import (
	"sort"
	"strings"
)

// Hosted runner labels (docs/features/21_parity_master.md PAR-170, docs/threads.md §8.2).
//
// GitHub and Forgejo spell their hosted Linux runner as `ubuntu-latest` (and the
// dated variants). Spindle has no hosted images — that is the D-15 non-goal — so
// the label would otherwise match no runner at all and a corpus workflow written
// for either platform would queue forever. The mapping here expands a hosted
// label onto the spindle label set that provides the same CONTRACT: a Linux
// container runner. It never widens anything a runner declared — every label in
// the mapped set is still a requirement, so the job runs only on a runner that
// actually advertises that platform. The image itself stays the runner's own
// declaration (`--image`, or the workflow's `container.image`); the operator who
// wants the hosted toolchain installs it in the image (PAR-171/177 are the
// label→image and stock-image items, not this alias).
//
// Withheld on purpose:
//   - `windows-*` and `macos-*`: Windows/macOS runners are the D-15 non-goal, so
//     there is no spindle label set that honours them. They stay unmapped and the
//     job queues (the run page shows its `runs-on`; the named reason is the
//     queue-observability surface, PAR-156).
//   - `ubuntu-24.04-arm` and the other architecture variants: mapping them to a
//     `linux` runner would silently run an amd64 image where the workflow asked
//     for arm64. An operator can still declare the label themselves.
var HostedLabelAliases = map[string][]string{
	"ubuntu-latest": {"self-hosted", "linux"},
	"ubuntu-24.04":  {"self-hosted", "linux"},
	"ubuntu-22.04":  {"self-hosted", "linux"},
	"ubuntu-20.04":  {"self-hosted", "linux"},
}

// ExpandRunsOn expands the hosted labels in a job's `runs-on` into the spindle
// label set the queue matches on. A mapped label is REPLACED by its expansion
// (the hosted spelling would never match a real runner); every other label,
// including the deliberately withheld ones, passes through unchanged. The result
// is normalised (lowercase, trimmed, de-duplicated, sorted) exactly like
// normalizeLabels.
func ExpandRunsOn(labels []string) []string {
	out := make([]string, 0, len(labels)+2)
	for _, label := range labels {
		key := strings.ToLower(strings.TrimSpace(label))
		if mapped, ok := HostedLabelAliases[key]; ok {
			out = append(out, mapped...)
			continue
		}
		out = append(out, label)
	}
	return normalizeLabels(out)
}

// HostedAliasNames lists the mapped labels sorted, for error messages and docs
// tests (the table is the source of truth).
func HostedAliasNames() []string {
	names := make([]string, 0, len(HostedLabelAliases))
	for name := range HostedLabelAliases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
