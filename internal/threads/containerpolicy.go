package threads

// The secure-by-default container option and mount policy (docs/threads.md §8.7).
//
// `docker create` receives two workflow-controlled fields that are not images:
// `container.options` (a flag string) and `container.volumes` (a list of -v
// specs), plus the same pair on every service. Whatever reaches docker there is
// node-root-equivalent, not container-confined: the runner holds the docker
// socket, so a job container is a SIBLING of every other container on that
// daemon. One `--privileged`, `--pid=host`, `--cap-add=ALL`, `--device`,
// `--security-opt`, or `-v /:/host` therefore hands a workflow the runner host
// without any container escape at all — and a fork pull request is a workflow
// an anonymous author wrote.
//
// The allowlist is deliberately small and argued: resource accounting and
// in-container confinement only.
//
//	--cpus VALUE            CPU quota
//	--memory VALUE, -m      memory limit
//	--memory-swap VALUE     swap limit
//	--shm-size VALUE        /dev/shm size (databases need it)
//	--ulimit VALUE          ONLY nofile=… (open-file limit)
//	--init                  a PID-1 init (reaps zombies)
//	--read-only             read-only root filesystem
//	--user VALUE, -u        run as a non-root user
//	--workdir VALUE, -w     working directory
//
// Everything else is refused: namespace flags (`--pid/--ipc/--network/--userns/
// --cgroupns/--uts`), privilege (`--privileged`, `--cap-add`, `--device`,
// `--security-opt`, `--group-add`), mounts (`--volume`, `--mount`, `--tmpfs`,
// `--volumes-from`), and daemon/socket access. `--ulimit` is limited to nofile
// because the rest of its keys (core, nproc, memlock, …) are node-level knobs,
// not job sizing.
//
// Volumes are refused outright in this version: the runner adds its own
// workspace volume itself, and a workflow-supplied bind mount is host
// filesystem access, not a job resource. A later decision may admit named
// volumes; a host path must never be one of them.
//
// Both spellings are parsed exactly as the runner splits the string
// (SplitOptions): `--opt=value` and `--opt value`, quoting included. A bare
// argument (no leading '-') is refused too: it would be appended to docker's
// argv and could become the container command.
//
// Two enforcement points, one implementation: the server refuses a job at
// creation (a failed job with a Reason naming the field and the option, the
// image-policy shape), and the runner re-checks at plan time so an older or
// bypassed server can never hand it a hostile argv.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// containerOption is one allowlisted docker create flag.
type containerOption struct {
	// value reports whether the flag takes an argument.
	value bool
	// valid checks the argument when value is true; nil accepts the generic
	// value rule (non-empty, no leading '-', no whitespace).
	valid func(string) bool
}

// containerOptionAllowlist is the complete set of workflow-controllable docker
// create flags. Adding an entry is a security decision: it must be a resource
// or in-container confinement knob, never namespace, capability, device,
// security-opt or mount related.
var containerOptionAllowlist = map[string]containerOption{
	"--cpus":        {value: true},
	"--memory":      {value: true},
	"-m":            {value: true},
	"--memory-swap": {value: true},
	"--shm-size":    {value: true},
	"--ulimit":      {value: true, valid: validUlimitValue},
	"--init":        {},
	"--read-only":   {},
	"--user":        {value: true},
	"-u":            {value: true},
	"--workdir":     {value: true},
	"-w":            {value: true},
	// Service health checks (PAR-078): the command runs INSIDE the service
	// container and the interval/timeout/retries drive docker's own probe, so
	// they are in-container confinement knobs, not host capabilities. They are
	// the documented way to gate a job on a database being ready
	// (`--health-cmd pg_isready`), and the runner's readiness wait reads the
	// result through `docker inspect`.
	"--health-cmd":          {value: true, valid: validHealthCommand},
	"--health-interval":     {value: true, valid: validHealthDuration},
	"--health-timeout":      {value: true, valid: validHealthDuration},
	"--health-retries":      {value: true, valid: validHealthRetries},
	"--health-start-period": {value: true, valid: validHealthDuration},
}

// validHealthCommand admits one in-container health command. It is a COMMAND,
// so spaces and shell punctuation are legitimate (`pg_isready -U postgres`);
// it may not be empty or look like another flag, and a control character is
// refused because the value is one argv element.
func validHealthCommand(v string) bool {
	if strings.TrimSpace(v) == "" || strings.HasPrefix(v, "-") {
		return false
	}
	for _, r := range v {
		if r == 0 || r == '\r' || r == '\n' {
			return false
		}
	}
	return true
}

// validHealthDuration admits a positive Go duration (`10s`, `1m30s`), which is
// exactly what docker's flag parser accepts.
func validHealthDuration(v string) bool {
	d, err := time.ParseDuration(v)
	return err == nil && d > 0
}

// validHealthRetries admits a positive integer retry count.
func validHealthRetries(v string) bool {
	n, err := strconv.Atoi(v)
	return err == nil && n > 0 && n <= 1000
}

// validUlimitValue admits only the nofile key ("nofile=1024" or
// "nofile=1024:2048").
func validUlimitValue(v string) bool {
	lower := strings.ToLower(v)
	if !strings.HasPrefix(lower, "nofile=") {
		return false
	}
	return validGenericOptionValue(lower[len("nofile="):])
}

// validGenericOptionValue is the argument rule shared by every value flag: a
// real value that cannot be read by docker as another flag.
func validGenericOptionValue(v string) bool {
	if v == "" || strings.HasPrefix(v, "-") {
		return false
	}
	for _, r := range v {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// ContainerOptionRefusal is one refused option or mount: the record field it
// came from ("container.options", "services.postgres.volumes"), the offending
// token as authored, and the operator-facing reason.
type ContainerOptionRefusal struct {
	Field  string
	Option string
	Reason string
}

// String renders the reason stored on the job record.
func (r ContainerOptionRefusal) String() string { return r.Reason }

// AllowedContainerOptions is the allowlist in its documented spelling, sorted,
// for the refusal message (so an author learns what they may use).
func AllowedContainerOptions() []string {
	out := make([]string, 0, len(containerOptionAllowlist))
	for name := range containerOptionAllowlist {
		if name == "-m" || name == "-u" || name == "-w" {
			continue // long spellings only in the message
		}
		if name == "--ulimit" {
			out = append(out, "--ulimit nofile=")
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// CheckWorkflowContainerOptions validates one workflow-controlled container.
// field is the record prefix ("container" or "services.<id>"); the returned
// refusal names field+".options" or field+".volumes".
func CheckWorkflowContainerOptions(field, options string, volumes []string) *ContainerOptionRefusal {
	if r := checkWorkflowOptions(field+".options", options); r != nil {
		return r
	}
	if len(volumes) > 0 {
		v := volumes[0]
		return &ContainerOptionRefusal{
			Field: field + ".volumes", Option: v,
			Reason: fmt.Sprintf("%s declares volume %q, which is refused: workflow-supplied mounts are never allowed (the runner provides its own workspace volume; a bind mount of a host path is host filesystem access, not a job resource)", field+".volumes", v),
		}
	}
	return nil
}

// checkWorkflowOptions walks the flag string exactly as the runner will.
func checkWorkflowOptions(field, options string) *ContainerOptionRefusal {
	tokens := SplitOptions(options)
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if strings.TrimSpace(tok) == "" {
			continue
		}
		name, inline, hasInline := containerOptionToken(tok)
		spec, ok := containerOptionAllowlist[name]
		if !ok {
			reason := fmt.Sprintf("%s carries %q, which is refused: workflow container options are limited to resource/confinement flags (%s); privileged mode, host namespaces, added capabilities, devices, security options and mounts are never workflow-controlled", field, tok, strings.Join(AllowedContainerOptions(), ", "))
			return &ContainerOptionRefusal{Field: field, Option: tok, Reason: reason}
		}
		if !spec.value {
			if hasInline {
				reason := fmt.Sprintf("%s carries %q, which is refused: %s takes no value", field, tok, name)
				return &ContainerOptionRefusal{Field: field, Option: tok, Reason: reason}
			}
			continue
		}
		value := inline
		if !hasInline {
			if i+1 >= len(tokens) {
				reason := fmt.Sprintf("%s carries %q without a value: %s requires one", field, tok, name)
				return &ContainerOptionRefusal{Field: field, Option: tok, Reason: reason}
			}
			value = tokens[i+1]
			i++
		}
		okValue := validGenericOptionValue(value)
		if spec.valid != nil {
			okValue = spec.valid(value)
		}
		if !okValue {
			reason := fmt.Sprintf("%s carries %q with value %q, which is refused: %s accepts %s", field, tok, value, name, optionValueHint(name))
			return &ContainerOptionRefusal{Field: field, Option: tok + " " + value, Reason: reason}
		}
	}
	return nil
}

// optionValueHint names the accepted argument shape for a refusal.
func optionValueHint(name string) string {
	switch name {
	case "--ulimit":
		return "only a nofile=… limit"
	case "--health-cmd":
		return "one in-container command (quoted if it has spaces)"
	case "--health-interval", "--health-timeout", "--health-start-period":
		return "a positive duration, e.g. 10s or 1m30s"
	case "--health-retries":
		return "a positive retry count"
	}
	return "one non-empty value that is not another flag"
}

// containerOptionToken splits one token into its flag name and inline value:
// "--memory=512m" → ("--memory", "512m", true), "-m512m" → ("-m", "512m", true),
// "--init" → ("--init", "", false).
func containerOptionToken(tok string) (name, value string, hasValue bool) {
	if strings.HasPrefix(tok, "--") {
		if i := strings.IndexByte(tok, '='); i >= 0 {
			return tok[:i], tok[i+1:], true
		}
		return tok, "", false
	}
	if strings.HasPrefix(tok, "-") && len(tok) >= 2 {
		name = tok[:2]
		rest := tok[2:]
		rest = strings.TrimPrefix(rest, "=")
		if rest != "" {
			return name, rest, true
		}
		return name, "", false
	}
	return tok, "", false // a bare argument: no allowlist entry can match it
}

// SplitOptions splits a workflow `options:` string into docker argv tokens.
// Quoting is honoured ('…' or "…"), a backslash is a literal character (docker
// receives it untouched), and whitespace outside quotes separates tokens. It is
// the ONE splitter: the runner builds argv with it, and the policy above parses
// the same string with it, so "what docker receives" and "what was checked" can
// never diverge.
func SplitOptions(s string) []string {
	var out []string
	var cur strings.Builder
	quote := byte(0)
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote = c
			inWord = true
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// containerOptionRefusalReason applies the policy to a job spec's container and
// every service. A nil refusal means the job may run. Sorted service ids give a
// deterministic refusal for a job with two offending sidecars.
func containerOptionRefusalReason(spec *Spec) *ContainerOptionRefusal {
	if spec == nil {
		return nil
	}
	if spec.Container != nil {
		if r := CheckWorkflowContainerOptions("container", spec.Container.Options, spec.Container.Volumes); r != nil {
			return r
		}
	}
	ids := make([]string, 0, len(spec.Services))
	for id := range spec.Services {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		svc := spec.Services[id]
		if r := CheckWorkflowContainerOptions("services."+id, svc.Options, svc.Volumes); r != nil {
			return r
		}
	}
	return nil
}

// containerRefusalReason is the job-level Reason when the container option
// policy refuses this spec, and whether it refused.
func (s *Service) containerRefusalReason(spec *Spec) (string, bool) {
	if r := containerOptionRefusalReason(spec); r != nil {
		return r.Reason, true
	}
	return "", false
}

// jobRefusalReason is the single creation-time gate: the image policy, then the
// container option and mount policy. Run creation and rerun both call it, so a
// policy tightened after a run was created also stops its reruns.
func (s *Service) jobRefusalReason(spec *Spec) (string, bool) {
	if reason, refused := s.imageRefusalReason(spec); refused {
		return reason, true
	}
	return s.containerRefusalReason(spec)
}
