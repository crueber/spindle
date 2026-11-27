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
// Volumes are refused unless the runner operator allow-lists the host path
// (ContainerPolicy.ValidVolumes, PAR-179): a workflow-supplied bind mount is
// host filesystem access, and a host path must never be implied.
//
// Two job-container mapping keys are NOT options and are handled here too:
//
//   - `container.ports` is applied as a `--publish 127.0.0.1::<port>`: an
//     EPHEMERAL loopback mapping, exactly like a service's readiness port, so
//     concurrent jobs cannot collide and no fixed host port is opened. A
//     host-side port (`8080:80`) is refused by name.
//   - `container.credentials` is refused by name (N7/D-23): the pull identity
//     is the runner operator's registry credential, never a workflow secret —
//     a workflow password would travel in the job payload (the bucket) and in
//     `docker create` argv.
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
//
// PAR-179/D-8 adds the runner OPERATOR's grants (ContainerPolicy). The server's
// creation-time policy (ServerContainerPolicy) accepts the GRANTABLE shapes —
// `--privileged`, `--cap-add` and a well-formed workflow volume — so they can
// reach a runner that declares the capability; permanent refusals (devices,
// security options, host namespaces, malformed/root volumes) are refused at
// both ends. The runner's own policy is the only place a host-level grant is
// decided, and it runs before any container, network or volume exists. A
// runner that grants privilege or a docker endpoint never accepts fork runs
// (refused at startup), so an anonymous PR can never widen its own isolation.

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ContainerPolicy is the runner OPERATOR's declared container grants
// (PAR-179, D-8). The zero value is the historical fail-closed policy: every
// workflow-controllable flag outside the resource/confinement allowlist is
// refused and every workflow volume is refused.
//
// The knobs are runner declarations, never workflow ones:
//
//	Privileged    admit `--privileged` and `--cap-add` in a workflow's options
//	                  (a runner granting this never accepts fork runs)
//	Network       a named docker network job/service/action containers join
//	                  instead of the per-job network ("" = per-job; host/none/
//	                  container:* are refused at startup)
//	ValidVolumes  absolute host paths (or path.Match globs) a workflow
//	                  `volumes:` source may bind; empty = every volume refused
//	DockerHost    a docker endpoint exposed to the JOB container only
//	                  (unix:///abs/socket or tcp://host:port); empty = no access
//
// DeferGrantable is the SERVER's mode: it accepts the grantable flags and
// volumes so a granting runner can be offered the job, and leaves the host-level
// refusal to the runner's plan-time check (which runs before any container,
// network or volume exists). Permanent refusals — devices, security options,
// host namespaces, mounts outside ValidVolumes — are refused at both ends.
type ContainerPolicy struct {
	Privileged     bool
	DeferGrantable bool
	Network        string
	ValidVolumes   []string
	DockerHost     string
}

// ServerContainerPolicy is the creation-time policy: grantable shapes are
// accepted so they can reach a runner that declares the capability, permanent
// ones are refused. A refused job fails at creation with a named Reason.
func ServerContainerPolicy() ContainerPolicy { return ContainerPolicy{DeferGrantable: true} }

// validContainerNetworkName matches a docker network name (the operator's knob).
func validContainerNetworkName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// ValidateContainerPolicy validates a runner's declared policy at startup. A
// bad knob is a startup error naming it, never a job that fails hours later.
func ValidateContainerPolicy(p ContainerPolicy) error {
	if p.Network != "" {
		switch strings.ToLower(p.Network) {
		case "host", "none", "bridge", "default":
			return fmt.Errorf("container network %q is refused: the operator knob names a user-defined docker network (job isolation is the per-job network when unset)", p.Network)
		}
		if strings.HasPrefix(p.Network, "container:") || strings.HasPrefix(p.Network, "service:") {
			return fmt.Errorf("container network %q is refused: a workflow host namespace is never a runner default", p.Network)
		}
		if !validContainerNetworkName(p.Network) {
			return fmt.Errorf("container network %q is not a valid docker network name", p.Network)
		}
	}
	for _, pattern := range p.ValidVolumes {
		pat := strings.TrimSpace(pattern)
		if pat == "" || !filepath.IsAbs(pat) || strings.ContainsRune(pat, 0) || strings.Contains(pat, "..") {
			return fmt.Errorf("valid volume pattern %q is refused: it must be an absolute host path (or a path.Match glob) without '..'", pattern)
		}
	}
	if p.DockerHost != "" {
		host := strings.TrimSpace(p.DockerHost)
		switch {
		case strings.HasPrefix(host, "unix://"):
			pth := strings.TrimPrefix(host, "unix://")
			if !filepath.IsAbs(pth) || strings.Contains(pth, "..") || strings.ContainsAny(pth, ",\n") {
				return fmt.Errorf("docker host %q is refused: unix:// takes an absolute socket path without ','", p.DockerHost)
			}
		case strings.HasPrefix(host, "tcp://"):
			addr := strings.TrimPrefix(host, "tcp://")
			if addr == "" || strings.ContainsAny(addr, " /\\") {
				return fmt.Errorf("docker host %q is refused: tcp:// takes host:port", p.DockerHost)
			}
		default:
			return fmt.Errorf("docker host %q is refused: use unix:///absolute/socket or tcp://host:port", p.DockerHost)
		}
	}
	return nil
}

// allowsVolume reports whether a workflow volume spec may bind under this
// policy, and the refusal reason when it may not.
func (p ContainerPolicy) allowsVolume(field, spec string) (bool, string) {
	if !p.DeferGrantable && len(p.ValidVolumes) == 0 {
		return false, fmt.Sprintf("%s declares volume %q, which is refused: workflow-supplied mounts are never allowed unless the runner operator allow-lists the host path (--container-valid-volume); the runner provides its own workspace volume", field, spec)
	}
	src, dst, opts, ok := splitVolumeSpec(spec)
	if !ok {
		return false, fmt.Sprintf("%s declares volume %q, which is refused: expected source:target[:options]", field, spec)
	}
	if !filepath.IsAbs(src) || !filepath.IsAbs(dst) || strings.Contains(src, "..") {
		return false, fmt.Sprintf("%s declares volume %q, which is refused: only an absolute host source and an absolute container target are accepted (a named volume or relative path could collide with another job's state)", field, spec)
	}
	if src == "/" {
		return false, fmt.Sprintf("%s declares volume %q, which is refused: the host root is never a valid volume", field, spec)
	}
	if !validVolumeOptions(opts) {
		return false, fmt.Sprintf("%s declares volume %q, which is refused: volume options are limited to ro/rw/z/Z/copy/nocopy", field, spec)
	}
	if p.DeferGrantable {
		return true, ""
	}
	clean := filepath.Clean(src)
	for _, pattern := range p.ValidVolumes {
		if ok, _ := path.Match(pattern, clean); ok {
			return true, ""
		}
	}
	return false, fmt.Sprintf("%s declares volume %q, which is refused: source %q is not in the runner's --container-valid-volume allowlist (%s)", field, spec, clean, strings.Join(p.ValidVolumes, ", "))
}

// splitVolumeSpec parses docker's source:target[:options] volume spelling.
func splitVolumeSpec(spec string) (source, target, options string, ok bool) {
	parts := strings.SplitN(spec, ":", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", false
	}
	source, target = parts[0], parts[1]
	if len(parts) == 3 {
		options = parts[2]
	}
	return source, target, options, true
}

// validVolumeOptions admits only mount semantics that cannot widen access.
func validVolumeOptions(opts string) bool {
	if opts == "" {
		return true
	}
	for _, opt := range strings.Split(opts, ",") {
		switch strings.TrimSpace(opt) {
		case "ro", "rw", "z", "Z", "copy", "nocopy":
		default:
			return false
		}
	}
	return true
}

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

// CheckWorkflowContainerOptions validates one workflow-controlled container
// under the historical fail-closed policy (every grant refused). It is the
// default the tests and callers that do not carry a runner policy use.
func CheckWorkflowContainerOptions(field, options string, volumes []string) *ContainerOptionRefusal {
	return CheckWorkflowContainerOptionsWith(ContainerPolicy{}, field, options, volumes)
}

// CheckWorkflowContainerOptionsWith validates one workflow-controlled
// container under a policy: the server's at creation (grantable shapes deferred
// to the runner) and a runner's own at plan time (where the host-level grant is
// decided). field is the record prefix ("container" or "services.<id>").
func CheckWorkflowContainerOptionsWith(pol ContainerPolicy, field, options string, volumes []string) *ContainerOptionRefusal {
	if r := checkWorkflowOptionsWith(pol, field+".options", options); r != nil {
		return r
	}
	for _, v := range volumes {
		if ok, reason := pol.allowsVolume(field+".volumes", v); !ok {
			return &ContainerOptionRefusal{Field: field + ".volumes", Option: v, Reason: reason}
		}
	}
	return nil
}

// grantableOption reports a workflow flag that a runner operator may grant.
func grantableOption(name string) bool {
	return name == "--privileged" || name == "--cap-add"
}

// checkWorkflowOptions walks the flag string exactly as the runner will.
func checkWorkflowOptionsWith(pol ContainerPolicy, field, options string) *ContainerOptionRefusal {
	tokens := SplitOptions(options)
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if strings.TrimSpace(tok) == "" {
			continue
		}
		name, inline, hasInline := containerOptionToken(tok)
		// A TTY is never granted: the runner's steps are `docker exec` without a
		// terminal, so a TTY on the container would be an accepted-but-inert
		// surface. It is named, with the in-container remedy (PAR-077).
		if name == "--tty" || name == "-t" {
			reason := fmt.Sprintf("%s carries %q, which is refused: steps run with `docker exec` and no terminal, so a container TTY would be inert; run a command that needs one through `script -qc '<command>' /dev/null` instead", field, tok)
			return &ContainerOptionRefusal{Field: field, Option: tok, Reason: reason}
		}
		spec, ok := containerOptionAllowlist[name]
		if !ok && grantableOption(name) {
			if !pol.DeferGrantable && !pol.Privileged {
				reason := fmt.Sprintf("%s carries %q, which is refused: privileged mode and added capabilities are not granted on this runner; its operator must set --container-privileged (and such a runner is excluded from fork runs, D-8)", field, tok)
				return &ContainerOptionRefusal{Field: field, Option: tok, Reason: reason}
			}
			// A grantable flag still takes a real value and is never another
			// flag; validating it keeps the argv well-formed. `--privileged`
			// takes no value at all (docker refuses `--privileged=true`).
			if name == "--privileged" && hasInline {
				reason := fmt.Sprintf("%s carries %q, which is refused: --privileged takes no value", field, tok)
				return &ContainerOptionRefusal{Field: field, Option: tok, Reason: reason}
			}
			if hasInline && !validGenericOptionValue(inline) {
				reason := fmt.Sprintf("%s carries %q, which is refused: the value is not one docker token", field, tok)
				return &ContainerOptionRefusal{Field: field, Option: tok, Reason: reason}
			}
			if !hasInline && name == "--cap-add" {
				if i+1 >= len(tokens) || !validGenericOptionValue(tokens[i+1]) {
					reason := fmt.Sprintf("%s carries %q without a capability name", field, tok)
					return &ContainerOptionRefusal{Field: field, Option: tok, Reason: reason}
				}
				i++
			}
			continue
		}
		if !ok {
			reason := fmt.Sprintf("%s carries %q, which is refused: workflow container options are limited to resource/confinement flags (%s); host namespaces, devices, security options and mounts are never workflow-controlled", field, tok, strings.Join(AllowedContainerOptions(), ", "))
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

// JobContainerPort is one validated `container.ports` entry (PAR-077).
type JobContainerPort struct {
	// Container is the port on the job container ("8080", "53/udp") — the
	// spelling a service on the job network dials.
	Container string
	// Publish is the `docker create --publish` argument: an ephemeral loopback
	// host port (`127.0.0.1::8080`), never a fixed one.
	Publish string
}

// ParseJobContainerPort validates one `container.ports` entry. A host-side port
// is refused by name: a fixed host port collides between concurrent jobs on one
// runner and opens a port on the runner host, while the job reaches its own
// container over loopback and a service reaches it over the job network anyway.
func ParseJobContainerPort(raw string) (JobContainerPort, error) {
	spec := strings.TrimSpace(raw)
	if spec == "" {
		return JobContainerPort{}, fmt.Errorf("container.ports declares an empty port")
	}
	if strings.Contains(spec, ":") {
		return JobContainerPort{}, fmt.Errorf("container.ports declares %q, which is refused: give the container port only (e.g. \"8080\"); the runner publishes it on an ephemeral loopback host port so concurrent jobs never collide and no fixed host port is opened", spec)
	}
	num, proto := spec, ""
	if i := strings.IndexByte(spec, '/'); i >= 0 {
		num, proto = spec[:i], strings.ToLower(strings.TrimSpace(spec[i+1:]))
		if proto != "tcp" && proto != "udp" {
			return JobContainerPort{}, fmt.Errorf("container.ports declares %q, which is refused: the protocol is tcp or udp", spec)
		}
	}
	n, err := strconv.Atoi(num)
	if err != nil || n < 1 || n > 65535 {
		return JobContainerPort{}, fmt.Errorf("container.ports declares %q, which is refused: expected a container port number, 1…65535", spec)
	}
	port := JobContainerPort{Container: strconv.Itoa(n), Publish: "127.0.0.1::" + strconv.Itoa(n)}
	if proto != "" {
		port.Container += "/" + proto
		port.Publish += "/" + proto
	}
	return port, nil
}

// containerCredentialRefusal is the job-level refusal for a workflow-supplied
// registry login (N7/D-23). A pull identity is the runner operator's
// `--registry-auth-file`, never the workflow's: the password would otherwise
// travel in the job payload (the bucket) and in docker create's environment.
func containerCredentialRefusal(spec *Spec) *ContainerOptionRefusal {
	if spec == nil || spec.Container == nil || spec.Container.Credentials == nil {
		return nil
	}
	return &ContainerOptionRefusal{
		Field:  "container.credentials",
		Option: "credentials",
		Reason: "container.credentials is refused: the registry pull identity is the runner operator's own credential (--registry-auth-file), never a workflow-supplied password — it would travel in the job payload (the bucket) and in docker create's environment; configure the runner's registry credential instead (docs/threads.md §8.6)",
	}
}

// ContainerCredentialsRefusalReason is the named refusal for a workflow
// registry credential, or "" when the spec carries none. The server's creation
// gate and the runner's plan check read the same text.
func ContainerCredentialsRefusalReason(spec *Spec) string {
	if r := containerCredentialRefusal(spec); r != nil {
		return r.Reason
	}
	return ""
}

// containerPortsRefusal is the job-level refusal for an invalid
// `container.ports` entry (a host-side port, a bad protocol or range).
func containerPortsRefusal(spec *Spec) *ContainerOptionRefusal {
	if spec == nil || spec.Container == nil {
		return nil
	}
	for _, raw := range spec.Container.Ports {
		if _, err := ParseJobContainerPort(raw); err != nil {
			return &ContainerOptionRefusal{Field: "container.ports", Option: raw, Reason: err.Error()}
		}
	}
	return nil
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

// containerOptionRefusalReason applies the SERVER creation-time policy to a
// job spec's container and every service. A nil refusal means the job may be
// created; grantable flags and allow-listed volumes are deferred to the runner
// that claims it. Sorted service ids give a deterministic refusal for a job with
// two offending sidecars.
func containerOptionRefusalReason(spec *Spec) *ContainerOptionRefusal {
	pol := ServerContainerPolicy()
	if spec == nil {
		return nil
	}
	if r := containerCredentialRefusal(spec); r != nil {
		return r
	}
	if r := containerPortsRefusal(spec); r != nil {
		return r
	}
	if spec.Container != nil {
		if r := CheckWorkflowContainerOptionsWith(pol, "container", spec.Container.Options, spec.Container.Volumes); r != nil {
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
		if r := CheckWorkflowContainerOptionsWith(pol, "services."+id, svc.Options, svc.Volumes); r != nil {
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
