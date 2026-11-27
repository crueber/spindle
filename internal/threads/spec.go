package threads

import (
	"strings"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// buildSpec assembles the versioned execution payload from the parsed workflow
// job (15 §2: the runner never reads the workflow file). Workflow-level env and
// permissions are merged under the job's (job wins), matching the CI semantics
// authors expect; concurrency is the job's, falling back to the workflow's.
func buildSpec(wfName string, wf *workflow.Workflow, def workflow.Job, inputs map[string]string) *Spec {
	spec := &Spec{
		Version:             SpecVersion,
		Workflow:            wfName,
		RunsOn:              append([]string{}, def.RunsOn...),
		Services:            serviceSpecs(def.Services),
		Env:                 mergeEnv(wf.Env, def.Env),
		If:                  def.If,
		IfSecrets:           strings.Contains(strings.ToLower(def.If), "secrets."),
		IfVars:              strings.Contains(strings.ToLower(def.If), "vars."),
		TimeoutMinutes:      def.TimeoutMinutes.Int(),
		TimeoutExpr:         def.TimeoutMinutes.Expr,
		ContinueOnError:     def.ContinueOnError.Bool(),
		ContinueOnErrorExpr: def.ContinueOnError.Expr,
		Permissions:         mergePermissions(wf.Permissions, def.Permissions),
		// The Forgejo OIDC key is a grant: workflow or job may set it, and a
		// job cannot turn the workflow's grant off with a boolean (`permissions:
		// id-token: none` is the denial spelling).
		EnableOpenIDConnect: def.EnableOpenIDConnect || (wf != nil && wf.EnableOpenIDConnect),
		Strategy:            strategySpec(def.Strategy),
		Concurrency:         concurrencySpec(wf, def),
		Uses:                strings.TrimSpace(def.Uses),
		With:                nonNilMap(def.With),
		Inputs:              nonNilMap(inputs),
		Outputs:             nonNilMap(def.Outputs),
		Steps:               stepSpecs(def.Steps),
	}
	// Workflow/job `defaults.run` fill the steps that name neither key; a
	// step-level value still wins (PAR-054).
	shell, wd := workflow.DefaultsFor(wf, &def)
	applyStepDefaults(spec.Steps, shell, wd)
	if def.Environment != nil {
		spec.Environment = &EnvironmentSpec{
			Name: strings.TrimSpace(def.Environment.Name),
			URL:  strings.TrimSpace(def.Environment.URL),
		}
	}
	if def.Container != nil {
		spec.Container = containerSpec(def.Container)
	}
	return spec
}

// mergeEnv layers two env maps (over wins); nil when both are empty.
func mergeEnv(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// mergePermissions layers two permission maps (job wins); nil when both empty.
func mergePermissions(base, over workflow.Permissions) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// concurrencySpec prefers the job's concurrency over the workflow default.
func concurrencySpec(wf *workflow.Workflow, def workflow.Job) *ConcurrencySpec {
	switch {
	case def.Concurrency != nil:
		return &ConcurrencySpec{
			Group:                def.Concurrency.Group,
			CancelInProgress:     def.Concurrency.CancelInProgress.Bool(),
			CancelInProgressExpr: def.Concurrency.CancelInProgress.Expr,
		}
	case wf != nil && wf.Concurrency != nil:
		return &ConcurrencySpec{
			Group:                wf.Concurrency.Group,
			CancelInProgress:     wf.Concurrency.CancelInProgress.Bool(),
			CancelInProgressExpr: wf.Concurrency.CancelInProgress.Expr,
		}
	}
	return nil
}

// strategySpec copies the matrix product, fail-fast flag and admission bound.
func strategySpec(in *workflow.Strategy) *StrategySpec {
	if in == nil {
		return nil
	}
	out := &StrategySpec{FailFast: in.FailFast}
	if in.MaxParallel != nil {
		out.MaxParallel = *in.MaxParallel
	}
	if len(in.Matrix.Axes) > 0 {
		out.Matrix = make(map[string][]string, len(in.Matrix.Axes))
		for axis, values := range in.Matrix.Axes {
			out.Matrix[axis] = append([]string{}, values...)
		}
	}
	for _, combo := range in.Matrix.Include {
		out.Include = append(out.Include, comboCopy(combo))
	}
	for _, combo := range in.Matrix.Exclude {
		out.Exclude = append(out.Exclude, comboCopy(combo))
	}
	return out
}

// applyStepDefaults fills steps that declare neither a shell nor a working
// directory from `defaults.run` (PAR-054). A parallel group is a container of
// ordinary steps, so its members inherit too; a composite action's inner steps
// are defined by its own action file and never inherit the caller's defaults.
func applyStepDefaults(steps []StepSpec, shell, workingDir string) {
	for i := range steps {
		st := &steps[i]
		// GitHub applies defaults.run to RUN steps only; a `uses:` step has its
		// own action metadata and must not inherit a caller's shell.
		if st.Run != "" {
			if st.Shell == "" && shell != "" {
				st.Shell = shell
			}
			if st.WorkingDirectory == "" && workingDir != "" {
				st.WorkingDirectory = workingDir
			}
		}
		if len(st.Parallel) > 0 {
			applyStepDefaults(st.Parallel, shell, workingDir)
		}
	}
}

// comboCopy copies one matrix include/exclude object.
func comboCopy(in workflow.MatrixCombination) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// containerSpec copies the job container's mapping fields.
func containerSpec(in *workflow.Container) *ContainerSpec {
	out := &ContainerSpec{
		Image:      in.Image,
		Env:        nonNilMap(in.Env),
		Ports:      nonNilStr(in.Ports),
		Command:    nonNilStr(in.Command),
		Entrypoint: nonNilStr(in.Entrypoint),
		Options:    in.Options,
		Volumes:    nonNilStr(in.Volumes),
	}
	if in.Credentials != nil {
		out.Credentials = &RegistryCredentialsSpec{
			Username: in.Credentials.Username,
			Password: in.Credentials.Password,
		}
	}
	return out
}

// serviceSpecs copies the sidecars (nil when none: no empty map on the wire).
func serviceSpecs(in map[string]workflow.Service) map[string]ServiceSpec {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]ServiceSpec, len(in))
	for name, svc := range in {
		out[name] = ServiceSpec{
			Image:      svc.Image,
			Env:        nonNilMap(svc.Env),
			Ports:      nonNilStr(svc.Ports),
			Command:    serviceCommand(svc),
			Entrypoint: nonNilStr(svc.Entrypoint),
			Options:    svc.Options,
			Volumes:    nonNilStr(svc.Volumes),
		}
	}
	return out
}

// serviceCommand merges the `command`/`cmd` spellings (command wins).
func serviceCommand(svc workflow.Service) []string {
	if len(svc.Command) > 0 {
		return nonNilStr(svc.Command)
	}
	return nonNilStr(svc.Cmd)
}

// stepSpecs copies every step field a runner must execute.
func stepSpecs(in []workflow.Step) []StepSpec {
	out := make([]StepSpec, 0, len(in))
	for _, st := range in {
		spec := StepSpec{
			ID:               st.ID,
			Name:             st.Name,
			Run:              st.Run,
			Uses:             st.Uses,
			With:             nonNilMap(st.With),
			Env:              nonNilMap(st.Env),
			If:               st.If,
			Shell:            st.Shell,
			WorkingDirectory: st.WorkingDirectory,
			ContinueOnError:  st.ContinueOnError,
			TimeoutMinutes:   st.TimeoutMinutes.Int(),
			TimeoutExpr:      st.TimeoutMinutes.Expr,
			Background:       st.Background,
			Wait:             strings.TrimSpace(st.Wait),
			WaitAll:          st.WaitAll,
			Cancel:           strings.TrimSpace(st.Cancel),
		}
		if len(st.Parallel) > 0 {
			spec.Parallel = stepSpecs(st.Parallel)
		}
		out = append(out, spec)
	}
	return out
}

// convenienceSteps projects the spec's steps onto the record's display shape
// (name/run/uses), which the run page and older readers use. A repository-local
// composite action is shown EXPANDED, in place, with each inner step carrying
// the action step it came from in Parent (docs/threads.md §3.4a): the page then
// shows what will actually run, before (and independently of) the runner's
// report. The display walk is bounded like the resolver's; a hostile payload
// cannot loop it.
func convenienceSteps(spec *Spec) []Step {
	if spec == nil {
		return nil
	}
	var out []Step
	appendDisplaySteps(&out, spec.Steps, spec.Actions, "", 1)
	return out
}

// appendDisplaySteps appends one step list, expanding local composites. depth
// is the composite nesting level (workflow steps are 1).
func appendDisplaySteps(out *[]Step, steps []StepSpec, actions map[string]ActionSpec, parent string, depth int) {
	for _, st := range steps {
		uses := strings.TrimSpace(st.Uses)
		if IsLocalAction(uses) && depth <= workflow.MaxActionDepth {
			if dir, err := LocalActionDir(uses); err == nil {
				if def, ok := actions[dir]; ok && def.Using == workflow.ActionUsingComposite {
					name := strings.TrimSpace(st.Name)
					if name == "" {
						name = dir
					}
					child := name
					if parent != "" {
						child = parent + " / " + name
					}
					appendDisplaySteps(out, def.Steps, actions, child, depth+1)
					continue
				}
				if def, ok := actions[dir]; ok && def.Using == workflow.ActionUsingDocker && def.Image != "" {
					// One row, but the page should say which image runs.
					*out = append(*out, Step{Name: st.Name, Run: st.Run, Uses: "docker://" + def.Image, Parent: parent})
					continue
				}
			}
		}
		*out = append(*out, Step{Name: st.Name, Run: st.Run, Uses: st.Uses, Parent: parent})
	}
}

// nonNilMap normalizes an empty map to nil (maps render as {} only when set).
func nonNilMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	return in
}

// containerImage is the display convenience field's value.
func containerImage(c *ContainerSpec) string {
	if c == nil {
		return ""
	}
	return c.Image
}
