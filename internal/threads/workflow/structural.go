package workflow

// structural.go — the fields the engine reads structurally: `runs-on`,
// `container.image`, `services.<id>.image`, `environment` (`name`/`url`) and
// the `env` maps above the step layer (workflow, job, container, service).
//
// `runs-on` (PAR-053) and `container.image` (PAR-030) are the two sanctioned
// substitution sites: their labels/image are validated here
// (validateRunsOn/validateContainerImage) and expanded per matrix cell before
// the job record is written. Every other field is not a substitution site
// (docs/WORKFLOWS.md §6.2a; docs/threads.md §3.6): it is chosen before any
// matrix cell or dispatch input exists, and a job/container/service `env` value
// is handed to the runner verbatim (only a STEP `env:` value is substituted).
// Waves before PAR-053 accepted a `${{ … }}` span and carried it verbatim, which
// is the silent-acceptance failure the strict parser exists to prevent: a
// service image or an env value carrying the authored span would reach the image
// gate and the process environment as text. Fail closed at parse time instead.

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrStructuralExpression is the parse error for a `${{ … }}` span in a
// structural field or an env layer above the step. It is a value so callers
// (and the importer's report) can match it.
var ErrStructuralExpression = errors.New("workflow: field does not take ${{ }}")

// Remedies named by the error. A structural field is chosen before a cell
// exists, so only a literal or a job per value can express it; an env value at
// the workflow/job/container/service layer is copied verbatim, so the writable
// fix is a literal or a step `env:` (the one layer that substitutes).
const (
	structuralRemedy = "write the value literally, or declare one job per value with literal fields (a matrix cannot vary this field, and it is never substituted)"
	envLayerRemedy   = "write the value literally, or move the reference into a step `env:` (only a step's env values are substituted)"
)

// validateStructuralFields rejects any `${{` span in one job's structural
// fields, naming the field's path from the document root. `runs-on` (PAR-053)
// and `container.image` (PAR-030) are the two sanctioned exceptions; they are
// validated separately and expanded per matrix cell. A literal value —
// including one that merely contains `{` — is unchanged.
func validateStructuralFields(jobKey string, job *Job) error {
	base := "jobs." + jobKey
	if err := validateStructuralEnv(base+".env", job.Env); err != nil {
		return err
	}
	if job.Container != nil {
		if err := validateStructuralEnv(base+".container.env", job.Container.Env); err != nil {
			return err
		}
		// `entrypoint`/`command` are applied (PAR-077) and are literal like the
		// image: a `${{ }}` span accepted here would become the argument text,
		// not its value (the silent-acceptance failure this file prevents).
		for i, word := range job.Container.Command {
			if err := rejectStructuralExpression(fmt.Sprintf("%s.container.command[%d]", base, i), word); err != nil {
				return err
			}
		}
		for i, word := range job.Container.Entrypoint {
			if err := rejectStructuralExpression(fmt.Sprintf("%s.container.entrypoint[%d]", base, i), word); err != nil {
				return err
			}
		}
	}
	for _, id := range sortedMapKeys(job.Services) {
		svc := job.Services[id]
		if err := rejectStructuralExpression(base+".services."+id+".image", svc.Image); err != nil {
			return err
		}
		if err := validateStructuralEnv(base+".services."+id+".env", svc.Env); err != nil {
			return err
		}
		for i, word := range svc.Command {
			if err := rejectStructuralExpression(fmt.Sprintf("%s.services.%s.command[%d]", base, id, i), word); err != nil {
				return err
			}
		}
		for i, word := range svc.Cmd {
			if err := rejectStructuralExpression(fmt.Sprintf("%s.services.%s.cmd[%d]", base, id, i), word); err != nil {
				return err
			}
		}
		for i, word := range svc.Entrypoint {
			if err := rejectStructuralExpression(fmt.Sprintf("%s.services.%s.entrypoint[%d]", base, id, i), word); err != nil {
				return err
			}
		}
	}
	if env := job.Environment; env != nil {
		if err := rejectStructuralExpression(base+".environment", env.Name); err != nil {
			return err
		}
		if err := rejectStructuralExpression(base+".environment.url", env.URL); err != nil {
			return err
		}
	}
	return nil
}

// validateStructuralEnv rejects a `${{` span in every value of one unsubstituted
// env map, naming each as `<base>.<NAME>` in key order (so the reported name is
// stable).
func validateStructuralEnv(base string, env map[string]string) error {
	for _, name := range sortedMapKeys(env) {
		if err := rejectEnvExpression(base+"."+name, env[name]); err != nil {
			return err
		}
	}
	return nil
}

// rejectStructuralExpression fails on the first `${{` in a structural value.
func rejectStructuralExpression(field, value string) error {
	return rejectExpression(field, value, structuralRemedy)
}

// rejectEnvExpression fails on the first `${{` in an env value at a layer the
// runner copies verbatim.
func rejectEnvExpression(field, value string) error {
	return rejectExpression(field, value, envLayerRemedy)
}

func rejectExpression(field, value, remedy string) error {
	if !strings.Contains(value, "${{") {
		return nil
	}
	return fmt.Errorf("%w: %s is %q; %s", ErrStructuralExpression, field, value, remedy)
}

// sortedMapKeys returns a map's string keys in a stable order, so a file with
// two offending services (or env names) always reports the same one.
func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
