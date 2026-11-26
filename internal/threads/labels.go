package threads

import (
	"fmt"
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

// Runner label→image mapping (PAR-171, docs/threads.md §8.2).
//
// Forgejo's runner config declares a label with the backend that serves it:
//
//	labels:
//	  - ubuntu-latest:docker://node:lts
//	  - self-hosted
//
// Threads keeps the executable half that is in scope — a CONTAINER image — and
// refuses the two that are not, by name: `lxc://` (LXC backend) and `host`
// (unisolated host execution) are the D-15 non-goals, so a runner that asks for
// one is a named configuration error rather than a silent fallback to docker.
// A third-party `docker://` image is the runner operator's declaration (the
// same trust class as its `--image` default), and the server honours it by
// putting the image in the job payload when the workflow named no
// `container.image` of its own: the workflow's explicit image always wins.
//
// The label part is still a normal label — a job's `runs-on` must match it (and
// the hosted-label expansion of PAR-170 still applies) — so the mapping never
// widens what a runner is offered.

// RunnerLabelMapping is the parsed declaration: the plain labels, the
// label→image map, and nothing for the refused backends.
type RunnerLabelMapping struct {
	Labels []string
	Images map[string]string
}

// ParseRunnerLabels splits Forgejo's `label:backend` spellings. The documented
// forms are `name:docker://image` (honoured), `name:lxc://template` and `name`
// (plain label; `host` is the refused host backend). An unrecognized
// `label:backend` is ErrInvalid naming the entry — never a label silently
// containing a colon, which could never match a workflow.
func ParseRunnerLabels(in []string) (RunnerLabelMapping, error) {
	var out RunnerLabelMapping
	images := map[string]string{}
	for _, raw := range in {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		label, backend, hasBackend := strings.Cut(entry, ":")
		label = strings.ToLower(strings.TrimSpace(label))
		if !hasBackend {
			out.Labels = append(out.Labels, label)
			continue
		}
		backend = strings.TrimSpace(backend)
		switch {
		case strings.HasPrefix(backend, "docker://"):
			image := strings.TrimSpace(strings.TrimPrefix(backend, "docker://"))
			if image == "" || strings.ContainsAny(image, " \t\r\n") {
				return RunnerLabelMapping{}, fmt.Errorf("%w: label %q has no image after docker://", ErrInvalid, raw)
			}
			if label == "" {
				return RunnerLabelMapping{}, fmt.Errorf("%w: label %q has no label name", ErrInvalid, raw)
			}
			out.Labels = append(out.Labels, label)
			images[label] = image
		case strings.HasPrefix(backend, "lxc://"):
			return RunnerLabelMapping{}, fmt.Errorf("%w: label %q asks for the LXC backend, which is a documented non-goal (D-15/PAR-182); declare a docker:// image",
				ErrInvalid, raw)
		case backend == "host" || strings.HasPrefix(backend, "host://"):
			return RunnerLabelMapping{}, fmt.Errorf("%w: label %q asks for unisolated host execution, which is a documented non-goal (D-15/PAR-182); declare a docker:// image",
				ErrInvalid, raw)
		default:
			return RunnerLabelMapping{}, fmt.Errorf("%w: label %q names backend %q; the documented forms are name:docker://image, name:lxc://template (refused) and a plain label",
				ErrInvalid, raw, backend)
		}
	}
	if len(images) == 0 {
		images = nil
	}
	out.Images = images
	out.Labels = normalizeLabels(out.Labels)
	if len(out.Labels) == 0 {
		out.Labels = []string{"self-hosted"}
	}
	return out, nil
}

// runnerImageFor returns the image the runner's label mapping assigns to one
// job, using the job's AUTHORED runs-on labels in order (the first mapped label
// wins, so the declaration order in `runs-on` is the tie-break).
func runnerImageFor(images map[string]string, runsOn []string) string {
	for _, label := range runsOn {
		if image, ok := images[strings.ToLower(strings.TrimSpace(label))]; ok {
			return image
		}
	}
	return ""
}

// jobRunsOn is a job's authored `runs-on` labels (the spec's list, falling back
// to the display string) — the spelling a runner's label mapping is keyed on.
func jobRunsOn(job *Job) []string {
	if job == nil {
		return nil
	}
	if job.Spec != nil && len(job.Spec.RunsOn) > 0 {
		return job.Spec.RunsOn
	}
	if strings.TrimSpace(job.RunsOn) != "" {
		return strings.Split(job.RunsOn, ",")
	}
	return nil
}
