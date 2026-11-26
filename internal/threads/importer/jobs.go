package importer

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// jobs translates every job. `strategy` is handled first because a job-level
// concurrency group may reference a matrix axis.
func (t *translator) jobs(m *yaml.Node, i int) error {
	val := m.Content[i+1]
	if val.Kind != yaml.MappingNode || len(val.Content) == 0 {
		return fmt.Errorf("%w: `jobs:` must be a mapping of job id to job", ErrUnconvertible)
	}
	for j := 0; j+1 < len(val.Content); j += 2 {
		t.job(val.Content[j].Value, val.Content[j+1])
	}
	return nil
}

// job translates one job. A job is never dropped: other jobs may name it in
// `needs`, and a missing job makes the whole file invalid. A job with no usable
// shape becomes a job that fails with the reason.
func (t *translator) job(name string, node *yaml.Node) {
	base := "jobs." + name
	if node.Kind != yaml.MappingNode {
		t.failingJob(node, base, "job", "a job must be a mapping")
		return
	}
	matrixLoss := false
	if i := mapIndex(node, "strategy"); i >= 0 {
		matrixLoss = t.strategyAt(node, i, base+".strategy")
	}
	axes := matrixAxes(node)
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		p := base + "." + key.Value
		switch key.Value {
		case "strategy":
			continue
		case "name":
			if _, ok := scalarText(node.Content[i+1]); !ok {
				t.drop(node, i, t.untranslatedAt(p, "name", "a job name must be a string"))
				i -= 2
			}
		case "runs-on":
			t.runsOnAt(node, i, p)
		case "environment":
			t.environmentAt(node, i, p, name)
		case "container":
			t.containerAt(node, i, p)
		case "services":
			t.servicesAt(node, i, p)
		case "env":
			t.stringMapAt(node, i, p)
		case "steps":
			t.stepsAt(node, i, p)
		case "needs":
			t.needsAt(node, i, p)
		case "if":
			if _, ok := scalarText(node.Content[i+1]); !ok {
				t.drop(node, i, t.untranslatedAt(p, "if", "a condition must be a string"))
				i -= 2
			}
		case "timeout-minutes":
			if node.Content[i+1].Tag != "!!int" {
				t.drop(node, i, t.untranslatedAt(p, "timeout-minutes", "only a whole number is supported; an expression is not"))
				i -= 2
			}
		case "continue-on-error":
			if node.Content[i+1].Tag != "!!bool" {
				t.drop(node, i, t.untranslatedAt(p, "continue-on-error", "only a literal true/false is supported; an expression is not"))
				i -= 2
			}
		case "outputs":
			t.stringMapAt(node, i, p)
		case "with":
			t.stringMapAt(node, i, p)
		case "uses":
			t.reusableJobAt(node, i, base)
			i -= 2
		case "concurrency":
			t.concurrencyAt(node, i, p, true, axes)
		case "permissions":
			t.permissionsAt(node, i, p)
		case "enable-openid-connect":
			if node.Content[i+1].Tag != "!!bool" {
				t.drop(node, i, t.untranslatedAt(p, "enable-openid-connect", "expected true or false"))
				i -= 2
			}
		default:
			reason := "not part of the Spindle job schema"
			if key.Value == "defaults" {
				reason = "job defaults.run is not supported yet; set shell/working-directory on each step"
			}
			if key.Value == "secrets" {
				reason = "secrets passed to a reusable workflow are not supported; pass them as repository secrets"
			}
			t.drop(node, i, t.untranslatedAt(p, key.Value, reason))
			i -= 2
		}
	}
	t.ensureRunnable(name, node, base)
	if matrixLoss {
		t.failJob(node, base+".strategy",
			"the matrix declaration could not be fully converted; see the UNTRANSLATED comments")
	}
}

// failJob appends a step that fails with the reason. It is used when a
// construct was refused in a way that leaves the file valid but makes the
// job's work unknown (a partially converted matrix), so the job must not run.
func (t *translator) failJob(node *yaml.Node, path, reason string) {
	i := mapIndex(node, "steps")
	if i < 0 || node.Content[i+1].Kind != yaml.SequenceNode {
		return
	}
	appendFailureStep(node.Content[i+1], path+": "+reason)
}

// ensureRunnable makes a job that lost its required shape runnable (and loud).
// A job with no `runs-on` gets the profile's fallback with a note; a job with no
// steps gets one step that fails with the reason.
func (t *translator) ensureRunnable(name string, node *yaml.Node, base string) {
	if mapIndex(node, "runs-on") < 0 {
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "runs-on"}
		val := &yaml.Node{}
		setSequence(val, t.prof.fallback)
		key.HeadComment = "# " + base + ".runs-on was absent or untranslatable; assumed " + strings.Join(t.prof.fallback, ", ")
		t.note(base+".runs-on", "runs-on (assumed)", "the source job declared no usable runs-on; assumed the "+string(t.platform)+" default labels")
		node.Content = append(node.Content, key, val)
	}
	i := mapIndex(node, "steps")
	if i < 0 {
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "steps"}
		val := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		node.Content = append(node.Content, key, val)
		i = mapIndex(node, "steps")
	}
	steps := node.Content[i+1]
	if steps.Kind != yaml.SequenceNode {
		setSequence(steps, nil)
	}
	if len(steps.Content) == 0 {
		if !t.reported(base + ".steps") {
			comment := t.untranslatedAt(base+".steps", "steps", "the source job declared no steps the importer could carry; a step that fails is added")
			node.Content[i].HeadComment = joinComment(node.Content[i].HeadComment, comment)
		}
		appendFailureStep(steps, base+": no steps")
	}
}

// reported reports whether an untranslated item already names path.
func (t *translator) reported(path string) bool {
	for _, item := range t.untranslated {
		if item.Path == path {
			return true
		}
	}
	return false
}

// failingJob replaces a job that cannot be a mapping with a job that fails and
// says why. Keeping the id preserves every `needs:` that names it.
func (t *translator) failingJob(node *yaml.Node, path, construct, reason string) {
	comment := t.untranslatedAt(path, construct, reason)
	node.Kind = yaml.MappingNode
	node.Tag = "!!map"
	node.Style = 0
	node.Value = ""
	node.Content = nil
	runsOnKey := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "runs-on"}
	runsOnVal := &yaml.Node{}
	setSequence(runsOnVal, t.prof.fallback)
	stepsKey := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "steps"}
	stepsVal := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	node.Content = []*yaml.Node{runsOnKey, runsOnVal, stepsKey, stepsVal}
	appendFailureStep(stepsVal, path+": "+reason)
	runsOnKey.HeadComment = joinComment(runsOnKey.HeadComment, comment)
	t.translated++
}

// reusableJobAt refuses a reusable-workflow job (`jobs.<id>.uses`). The engine
// does not run reusable workflows yet, and a job with `uses` has no steps, so it
// becomes a job that fails with the named reason instead of a job that silently
// does nothing.
func (t *translator) reusableJobAt(node *yaml.Node, i int, base string) {
	uses, _ := scalarText(node.Content[i+1])
	reason := "reusable workflows are not supported yet; this job is replaced by a step that fails"
	t.drop(node, i, t.untranslatedAt(base+".uses", "uses: "+uses, reason))
	j := mapIndex(node, "steps")
	if j < 0 {
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "steps"}
		val := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		node.Content = append(node.Content, key, val)
		j = mapIndex(node, "steps")
	}
	steps := node.Content[j+1]
	if steps.Kind != yaml.SequenceNode {
		setSequence(steps, nil)
	}
	steps.Content = nil
	appendFailureStep(steps, base+": "+reason+" (uses: "+uses+")")
}

// strategyAt carries matrix, include/exclude and fail-fast. It reports whether
// the matrix was only partially convertible: the caller then adds a step that
// fails, because running a matrix whose cells are unknown is a silent loss.
func (t *translator) strategyAt(node *yaml.Node, i int, path string) (lossy bool) {
	val := node.Content[i+1]
	if isNull(val) {
		t.drop(node, i, "")
		return false
	}
	if val.Kind != yaml.MappingNode {
		t.drop(node, i, t.untranslatedAt(path, "strategy", "expected a mapping"))
		return false
	}
	for j := 0; j < len(val.Content); j += 2 {
		key := val.Content[j]
		p := path + "." + key.Value
		switch key.Value {
		case "matrix":
			dropped, matrixLossy := t.matrixAt(val, j, p)
			if dropped {
				j -= 2
			}
			if matrixLossy {
				lossy = true
			}
		case "fail-fast":
			if val.Content[j+1].Tag != "!!bool" {
				t.drop(val, j, t.untranslatedAt(p, "strategy.fail-fast", "only a literal true/false is supported; an expression is not"))
				j -= 2
			}
		case "max-parallel":
			t.drop(val, j, t.untranslatedAt(p, "strategy.max-parallel", "a per-job cell cap is not supported; a concurrency group caps runs, not cells"))
			j -= 2
		default:
			t.drop(val, j, t.untranslatedAt(p, "strategy."+key.Value, "not part of the Spindle strategy schema"))
			j -= 2
		}
	}
	t.translated++
	return lossy
}

// matrixAt validates and carries `strategy.matrix`. Axes are a scalar or a list
// of scalars; `include`/`exclude` keep GitHub's object-list shape and values
// (the target expands them with the same merge rules, docs/threads.md §3.3a).
//
// An object-valued axis is the one genuinely unconvertible shape: the target's
// axes are scalars. Its key is kept declared as one placeholder cell, so a
// `${{ matrix.<key> }}` reference still parses, and the job gets a step that
// fails — the combinations are unknown, so the job must not run silently.
func (t *translator) matrixAt(m *yaml.Node, i int, path string) (dropped, lossy bool) {
	val := m.Content[i+1]
	if isNull(val) {
		t.drop(m, i, "")
		return true, false
	}
	if val.Kind != yaml.MappingNode {
		t.drop(m, i, t.untranslatedAt(path, "strategy.matrix", "expected a mapping of axis to list"))
		return true, true
	}
	for j := 0; j < len(val.Content); j += 2 {
		axis := val.Content[j]
		p := path + "." + axis.Value
		if axis.Value == "include" || axis.Value == "exclude" {
			entryDropped, entryLossy := t.matrixListAt(val, j, p, axis.Value)
			if entryDropped {
				j -= 2
			}
			if entryLossy {
				lossy = true
			}
			continue
		}
		cells := val.Content[j+1]
		if !matrixAxisOK(cells) {
			comment := t.untranslatedAt(p, "matrix."+axis.Value,
				"object-valued matrix cells are not supported; the axis is carried as one placeholder cell and the job fails")
			axis.HeadComment = joinComment(axis.HeadComment, comment)
			setSequence(cells, []string{untranslatedMatrixValue})
			cells.Style = yaml.FlowStyle
			lossy = true
			continue
		}
		t.translated++
	}
	if len(val.Content) == 0 && val.HeadComment != "" {
		// An empty mapping renders as `{}`, which would bury the comment
		// inside the braces; lift it onto the `matrix:` key's block instead.
		comment := val.HeadComment
		val.HeadComment = ""
		m.Content[i].HeadComment = joinComment(m.Content[i].HeadComment, comment)
	}
	return false, lossy
}

// untranslatedMatrixValue is the placeholder a refused matrix value is carried
// as. Keeping the key declared is what lets a `${{ matrix.<key> }}` reference
// still parse; the job's failing step means it can never look green.
const untranslatedMatrixValue = "untranslated-object-cell"

// matrixAxisOK reports whether an axis value has a target spelling: a scalar
// (including an empty one) or a list of scalars.
func matrixAxisOK(n *yaml.Node) bool {
	switch n.Kind {
	case yaml.ScalarNode:
		return true
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return false
			}
		}
		return true
	}
	return false
}

// matrixListAt carries an `include:`/`exclude:` list. Each entry keeps its
// object shape and scalar values; a nested list/object value has no matrix
// spelling, so it becomes a placeholder string (the key stays declared and the
// job fails) rather than being dropped. It reports whether the list key had to
// be dropped as a whole (so the caller can fix its index) and whether the
// conversion lost a cell shape.
func (t *translator) matrixListAt(m *yaml.Node, i int, path, kind string) (dropped, lossy bool) {
	val := m.Content[i+1]
	if isNull(val) {
		return false, false
	}
	if val.Kind != yaml.SequenceNode {
		t.drop(m, i, t.untranslatedAt(path, "strategy.matrix."+kind, "expected a list of objects (each object is a set of key: value pairs)"))
		return true, true
	}
	for j := 0; j < len(val.Content); j++ {
		entry := val.Content[j]
		p := fmt.Sprintf("%s[%d]", path, j+1)
		if entry.Kind != yaml.MappingNode {
			t.dropItem(val, j, t.untranslatedAt(p, kind+" entry", "each entry must be an object of key: value pairs"))
			j--
			lossy = true
			continue
		}
		emptyKey := false
		for k := 0; k+1 < len(entry.Content); k += 2 {
			key, value := entry.Content[k], entry.Content[k+1]
			if key.Value == "" {
				t.dropItem(val, j, t.untranslatedAt(p, kind+" entry", "an entry key must not be empty"))
				j--
				emptyKey = true
				lossy = true
				break
			}
			if value.Kind == yaml.ScalarNode && !isNull(value) {
				continue
			}
			comment := t.untranslatedAt(p+"."+key.Value, kind+"."+key.Value,
				"a nested list or object has no matrix-value spelling; the value is carried as a placeholder and the job fails")
			key.HeadComment = joinComment(key.HeadComment, comment)
			setScalar(value, untranslatedMatrixValue)
			lossy = true
		}
		if emptyKey {
			continue
		}
		t.translated++
	}
	return false, lossy
}

// matrixAxes lists the matrix keys a `${{ matrix.<key> }}` reference may name —
// the usable axes plus every include entry's keys — mirroring the parser's
// matrix.FieldNames, for job-level concurrency-group validation.
func matrixAxes(job *yaml.Node) []string {
	i := mapIndex(job, "strategy")
	if i < 0 {
		return nil
	}
	m := mapValue(job.Content[i+1], "matrix")
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for j := 0; j+1 < len(m.Content); j += 2 {
		key, value := m.Content[j], m.Content[j+1]
		switch key.Value {
		case "include":
			if value.Kind == yaml.SequenceNode {
				for _, entry := range value.Content {
					if entry.Kind != yaml.MappingNode {
						continue
					}
					for k := 0; k+1 < len(entry.Content); k += 2 {
						add(entry.Content[k].Value)
					}
				}
			}
		case "exclude":
			// An exclude entry names no field (docs/threads.md §3.3a).
		default:
			usable := value.Kind == yaml.SequenceNode && len(value.Content) > 0
			if value.Kind == yaml.ScalarNode && !isNull(value) {
				usable = true
			}
			if usable {
				add(key.Value)
			}
		}
	}
	sort.Strings(out)
	return out
}

// runsOnAt maps the source labels to the labels a default runner declares. An
// unmapped label is carried through verbatim (labels are exact and subset-
// matched; a wrong label queues with a named reason, never silently runs
// elsewhere).
func (t *translator) runsOnAt(node *yaml.Node, i int, path string) {
	val := node.Content[i+1]
	var labels []string
	switch val.Kind {
	case yaml.ScalarNode:
		if isNull(val) {
			t.drop(node, i, t.untranslatedAt(path, "runs-on", "empty; assumed the default labels"))
			return
		}
		labels = []string{val.Value}
	case yaml.SequenceNode:
		for _, item := range val.Content {
			text, ok := scalarText(item)
			if !ok {
				t.drop(node, i, t.untranslatedAt(path, "runs-on", "labels must be strings"))
				return
			}
			labels = append(labels, text)
		}
	default:
		t.drop(node, i, t.untranslatedAt(path, "runs-on", "expected a label or a list of labels"))
		return
	}
	var mapped []string
	seen := map[string]bool{}
	changed := false
	for _, label := range labels {
		expansion, ok := t.prof.labels[label]
		if ok {
			changed = true
			for _, e := range expansion {
				if !seen[e] {
					seen[e] = true
					mapped = append(mapped, e)
				}
			}
			t.note(path, "runs-on: "+label, "mapped to the default runner labels ["+strings.Join(expansion, ", ")+"]")
			continue
		}
		if !seen[label] {
			seen[label] = true
			mapped = append(mapped, label)
		}
		if reason, ok := t.prof.hosted[label]; ok {
			t.note(path, "runs-on: "+label, reason)
		}
	}
	if changed {
		setSequence(val, mapped)
	}
	t.translated++
}

// containerAt translates a job container. The target accepts image/env/ports/
// command/entrypoint/options/volumes; `credentials` has no lock-free spelling
// (registry auth is a runner/docker-config concern).
func (t *translator) containerAt(node *yaml.Node, i int, path string) {
	val := node.Content[i+1]
	if isNull(val) {
		t.drop(node, i, "")
		return
	}
	if val.Kind == yaml.ScalarNode {
		t.translated++
		return
	}
	if val.Kind != yaml.MappingNode {
		t.drop(node, i, t.untranslatedAt(path, "container", "expected an image name or a mapping"))
		return
	}
	for j := 0; j < len(val.Content); j += 2 {
		key := val.Content[j]
		p := path + "." + key.Value
		switch key.Value {
		case "image":
			if _, ok := scalarText(val.Content[j+1]); !ok {
				t.drop(val, j, t.untranslatedAt(p, "container.image", "the image must be a string"))
				j -= 2
				continue
			}
		case "options":
			if _, ok := scalarText(val.Content[j+1]); !ok {
				t.drop(val, j, t.untranslatedAt(p, "container.options", "the options must be a string"))
				j -= 2
				continue
			}
			t.note(p, "container.options", "carried through; the runner allowlist still refuses anything outside resource confinement")
		case "env", "ports", "command", "entrypoint", "volumes":
			t.translated++
		case "cmd":
			key.Value = "command"
			t.note(p, "container.cmd", "renamed to command")
			t.translated++
		case "credentials":
			t.drop(val, j, t.untranslatedAt(p, "container.credentials", "registry credentials are not a workflow key; the runner supplies the pull identity"))
			j -= 2
			continue
		default:
			t.drop(val, j, t.untranslatedAt(p, "container."+key.Value, "not part of the Spindle container schema"))
			j -= 2
			continue
		}
		t.translated++
	}
}

// servicesAt translates sidecars. Forgejo's `cmd` and `entrypoint` spellings are
// carried to the target's `command`/`entrypoint`.
func (t *translator) servicesAt(node *yaml.Node, i int, path string) {
	val := node.Content[i+1]
	if isNull(val) {
		t.drop(node, i, "")
		return
	}
	if val.Kind != yaml.MappingNode {
		t.drop(node, i, t.untranslatedAt(path, "services", "expected a mapping of service name to definition"))
		return
	}
	for j := 0; j+1 < len(val.Content); j += 2 {
		name := val.Content[j].Value
		svc := val.Content[j+1]
		p := path + "." + name
		if svc.Kind != yaml.MappingNode {
			t.drop(val, j, t.untranslatedAt(p, "service "+name, "a service definition must be a mapping"))
			j -= 2
			continue
		}
		for k := 0; k < len(svc.Content); k += 2 {
			key := svc.Content[k]
			kp := p + "." + key.Value
			switch key.Value {
			case "image":
				if _, ok := scalarText(svc.Content[k+1]); !ok {
					t.drop(svc, k, t.untranslatedAt(kp, "service.image", "the image must be a string"))
					k -= 2
					continue
				}
			case "cmd":
				key.Value = "command"
				t.note(kp, "service.cmd", "renamed to command")
			case "env", "ports", "command", "entrypoint", "options", "volumes":
			case "credentials":
				t.drop(svc, k, t.untranslatedAt(kp, "service.credentials", "registry credentials are not a workflow key; the runner supplies the pull identity"))
				k -= 2
				continue
			default:
				t.drop(svc, k, t.untranslatedAt(kp, "service."+key.Value, "not part of the Spindle service schema"))
				k -= 2
				continue
			}
			t.translated++
		}
	}
}

// needsAt accepts both spellings: a bare string and a list. The bare string is
// a parse error in the target, so it is rewritten (docs/WORKFLOWS.md §5.1).
func (t *translator) needsAt(node *yaml.Node, i int, path string) {
	val := node.Content[i+1]
	if isNull(val) {
		t.drop(node, i, "")
		return
	}
	switch val.Kind {
	case yaml.ScalarNode:
		text, ok := scalarText(val)
		if !ok {
			t.drop(node, i, t.untranslatedAt(path, "needs", "expected a job id or a list of job ids"))
			return
		}
		setSequence(val, []string{text})
		val.Style = yaml.FlowStyle
		t.note(path, "needs: "+text, "rewritten to a one-element list")
		t.translated++
	case yaml.SequenceNode:
		for j := 0; j < len(val.Content); j++ {
			if _, ok := scalarText(val.Content[j]); !ok {
				t.dropItem(val, j, t.untranslatedAt(path, "needs", "each dependency must be a job id"))
				j--
				continue
			}
			t.translated++
		}
	default:
		t.drop(node, i, t.untranslatedAt(path, "needs", "expected a job id or a list of job ids"))
	}
}

// appendFailureStep appends a step that fails with the reason.
func appendFailureStep(steps *yaml.Node, reason string) {
	step := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "run"}
	val := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Style: yaml.LiteralStyle, Value: failureScript(reason)}
	step.Content = []*yaml.Node{key, val}
	steps.Content = append(steps.Content, step)
}

// mapValue returns a mapping's value for key, or nil.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if i := mapIndex(m, key); i >= 0 {
		return m.Content[i+1]
	}
	return nil
}
