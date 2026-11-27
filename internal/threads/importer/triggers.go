package importer

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// supportedEvents is the target's closed trigger vocabulary. Everything else is
// refused by name: a trigger the engine cannot emit would be a workflow that
// silently never runs (docs/WORKFLOWS.md §2.2).
var supportedEvents = map[string]bool{
	"push":                        true,
	"pull_request":                true,
	"workflow_dispatch":           true,
	"workflow_call":               true,
	"schedule":                    true,
	"release":                     true,
	"issues":                      true,
	"issue_comment":               true,
	"pull_request_review":         true,
	"pull_request_review_comment": true,
	"label":                       true,
	"milestone":                   true,
}

// triggers translates `on`, in all three spellings it accepts.
func (t *translator) triggers(m *yaml.Node, i int) {
	val := m.Content[i+1]
	switch val.Kind {
	case yaml.ScalarNode:
		if isNull(val) {
			return // `on:` with no value: valid, fires on nothing
		}
		if val.Value == "schedule" {
			t.drop(m, i, t.untranslatedAt("on.schedule", "on: schedule",
				"a bare schedule has no cron entries; write `schedule: [{cron: \"…\"}]`"))
			return
		}
		if !supportedEvents[val.Value] {
			t.drop(m, i, t.untranslatedAt("on."+val.Value, val.Value, "not a trigger Threads can emit"))
			return
		}
		t.translated++
	case yaml.SequenceNode:
		for j := 0; j < len(val.Content); j++ {
			item := val.Content[j]
			name, ok := scalarText(item)
			if !ok || !supportedEvents[name] || name == "schedule" {
				reason := "not a trigger Threads can emit"
				if name == "schedule" {
					reason = "a bare schedule has no cron entries; write the mapping form"
				}
				t.dropItem(val, j, t.untranslatedAt("on."+name, name, reason))
				j--
				continue
			}
			t.translated++
		}
	case yaml.MappingNode:
		for j := 0; j < len(val.Content); j += 2 {
			ev := val.Content[j]
			path := "on." + ev.Value
			switch ev.Value {
			case "push":
				if t.pushTrigger(val, j, path) {
					t.translated++
				} else {
					j -= 2
				}
			case "pull_request":
				if t.pullRequestTrigger(val, j, path) {
					t.translated++
				} else {
					j -= 2
				}
			case "workflow_dispatch":
				if t.dispatchTrigger(val, j, path) {
					t.translated++
				} else {
					j -= 2
				}
			case "schedule":
				if t.scheduleTrigger(val, j, path) {
					t.translated++
				} else {
					j -= 2
				}
			case "fork", "watch":
				t.translated++ // a bare trigger with no filters
			case "release", "issues", "issue_comment", "pull_request_review", "pull_request_review_comment", "label", "milestone", "check_run":
				if t.typedTrigger(val, j, path, ev.Value) {
					t.translated++
				} else {
					j -= 2
				}
			case "workflow_call":
				if t.workflowCallTrigger(val, j, path) {
					t.translated++
				} else {
					j -= 2
				}
			default:
				t.drop(val, j, t.untranslatedAt(path, ev.Value, "not a trigger Threads can emit"))
				j -= 2
			}
		}
	default:
		t.drop(m, i, t.untranslatedAt("on", "on", "expected a string, a list, or a mapping"))
	}
}

// workflowCallTrigger carries `on.workflow_call` as written when the target's
// parser accepts it (PAR-075): typed inputs, declared secrets and outputs are
// the callee's interface and the contract a caller is checked against. An
// unsupported declaration is dropped with the parser's own reason, so the
// converted file stays parseable and the report names what was lost.
func (t *translator) workflowCallTrigger(m *yaml.Node, i int, path string) bool {
	val := m.Content[i+1]
	if isNull(val) {
		t.translated++
		return true
	}
	if val.Kind != yaml.MappingNode {
		t.drop(m, i, t.untranslatedAt(path, "on.workflow_call", "the callable declaration must be a mapping (inputs, secrets, outputs)"))
		return false
	}
	var call workflow.WorkflowCallTrigger
	if err := val.Decode(&call); err != nil {
		t.drop(m, i, t.untranslatedAt(path, "on.workflow_call", err.Error()))
		return false
	}
	t.note(path, "on.workflow_call",
		"carried as written; the callee's typed inputs, declared secrets and output templates are its interface")
	return true
}

// pushTrigger keeps the branch and tag filters the target accepts and refuses
// the path filters it does not (paths are a named gap, never a silent "runs on
// everything"). It reports whether the trigger survived.
func (t *translator) pushTrigger(m *yaml.Node, i int, path string) bool {
	val := m.Content[i+1]
	if isNull(val) {
		return true
	}
	if val.Kind != yaml.MappingNode {
		t.drop(m, i, t.untranslatedAt(path, "push", "the filters must be a mapping"))
		return false
	}
	for j := 0; j < len(val.Content); j += 2 {
		f := val.Content[j]
		p := path + "." + f.Value
		switch f.Value {
		case "branches", "tags", "branches-ignore", "tags-ignore", "paths", "paths-ignore":
			if !t.scalarListAt(val, j, p, "on.push."+f.Value) {
				j -= 2
			}
		default:
			t.drop(val, j, t.untranslatedAt(p, "on.push."+f.Value, "not part of the Spindle push-trigger schema"))
			j -= 2
		}
	}
	if mapIndex(val, "tags") >= 0 && mapIndex(val, "branches") < 0 {
		t.note(path+".tags", "tags-only push trigger",
			"converted as written; a tag push matching the glob starts a run, and a tags-only trigger never matches a branch push")
	}
	return true
}

// pullRequestTrigger keeps branches and filters `types` down to the three
// lifecycle actions the pulls service emits. A type outside that set is
// dropped by name (keeping the supported subset is closer than firing on all).
func (t *translator) pullRequestTrigger(m *yaml.Node, i int, path string) bool {
	val := m.Content[i+1]
	if isNull(val) {
		return true
	}
	if val.Kind != yaml.MappingNode {
		t.drop(m, i, t.untranslatedAt(path, "pull_request", "the filters must be a mapping"))
		return false
	}
	for j := 0; j < len(val.Content); j += 2 {
		f := val.Content[j]
		p := path + "." + f.Value
		switch f.Value {
		case "branches", "branches-ignore", "paths", "paths-ignore":
			if !t.scalarListAt(val, j, p, "on.pull_request."+f.Value) {
				j -= 2
			}
		case "types":
			if !t.typesAt(val, j, p) {
				j -= 2
			}
		default:
			t.drop(val, j, t.untranslatedAt(p, "on.pull_request."+f.Value, "not part of the Spindle pull-request trigger schema"))
			j -= 2
		}
	}
	return true
}

// typesAt filters the pull_request lifecycle list. The whole key is dropped
// when nothing survives, because an empty list is itself a parse error.
func (t *translator) typesAt(m *yaml.Node, i int, path string) bool {
	val := m.Content[i+1]
	if val.Kind != yaml.SequenceNode {
		t.drop(m, i, t.untranslatedAt(path, "pull_request.types", "expected a list of lifecycle actions"))
		return false
	}
	for j := 0; j < len(val.Content); j++ {
		name, ok := scalarText(val.Content[j])
		if ok && workflow.ValidPullRequestType(name) {
			t.translated++
			continue
		}
		t.dropItem(val, j, t.untranslatedAt(path, "pull_request.types: "+name,
			"Threads emits exactly the actions in pull_request.types; this one never fires"))
		j--
	}
	if len(val.Content) == 0 {
		t.drop(m, i, t.untranslatedAt(path, "pull_request.types", "an empty type list matches nothing; omit `types` to fire on every emitted action"))
		return false
	}
	return true
}

// dispatchTrigger translates the manual trigger and remembers its input names,
// which a concurrency group may reference.
func (t *translator) dispatchTrigger(m *yaml.Node, i int, path string) bool {
	val := m.Content[i+1]
	if isNull(val) {
		t.inputNames = nil
		return true
	}
	if val.Kind != yaml.MappingNode {
		t.drop(m, i, t.untranslatedAt(path, "workflow_dispatch", "the trigger must be a mapping"))
		return false
	}
	for j := 0; j < len(val.Content); j += 2 {
		field := val.Content[j]
		switch field.Value {
		case "inputs":
			t.inputsAt(val, j, path+".inputs")
		default:
			t.drop(val, j, t.untranslatedAt(path+"."+field.Value, "workflow_dispatch."+field.Value, "not part of the Spindle dispatch schema"))
			j -= 2
		}
	}
	t.collectInputNames(mapValue(val, "inputs"))
	return true
}

// collectInputNames records the surviving input names, sorted.
func (t *translator) collectInputNames(inputs *yaml.Node) {
	t.inputNames = t.inputNames[:0]
	if inputs == nil || inputs.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(inputs.Content); i += 2 {
		t.inputNames = append(t.inputNames, inputs.Content[i].Value)
	}
	sort.Strings(t.inputNames)
}

// inputsAt translates the typed dispatch inputs. The target accepts the string,
// boolean and choice types; any other type is carried as a string (the input
// still exists at the same name, so references keep resolving) and reported.
func (t *translator) inputsAt(m *yaml.Node, i int, path string) {
	val := m.Content[i+1]
	if isNull(val) {
		t.drop(m, i, "")
		return
	}
	if val.Kind != yaml.MappingNode {
		t.drop(m, i, t.untranslatedAt(path, "workflow_dispatch.inputs", "expected a mapping of input name to definition"))
		return
	}
	for j := 0; j < len(val.Content); j += 2 {
		name := val.Content[j].Value
		def := val.Content[j+1]
		p := path + "." + name
		if def.Kind != yaml.MappingNode {
			t.drop(val, j, t.untranslatedAt(p, "input "+name, "an input definition must be a mapping"))
			j -= 2
			continue
		}
		for k := 0; k < len(def.Content); k += 2 {
			field := def.Content[k]
			fp := p + "." + field.Value
			value := def.Content[k+1]
			switch field.Value {
			case "options":
				if value.Kind != yaml.SequenceNode {
					t.drop(def, k, t.untranslatedAt(fp, "input.options", "expected a list of choices"))
					k -= 2
					continue
				}
				for n := 0; n < len(value.Content); n++ {
					if _, ok := scalarText(value.Content[n]); !ok {
						t.dropItem(value, n, t.untranslatedAt(fp, "input.options", "a choice must be a string"))
						n--
						continue
					}
					t.translated++
				}
				continue
			case "type":
				text, ok := scalarText(value)
				if !ok {
					t.drop(def, k, t.untranslatedAt(fp, "input.type", "expected a string"))
					k -= 2
					continue
				}
				if text != "" && text != "string" && text != "boolean" && text != "choice" && text != "number" && text != "environment" {
					t.note(fp, "input type "+text, "carried as a string input: the type is not supported yet")
					setScalar(value, "string")
					t.untranslated = append(t.untranslated, Item{Path: fp, Construct: "input type " + oneLine(text),
						Reason: "the type is not supported; the input is carried as a string"})
					field.HeadComment = joinComment(field.HeadComment,
						"# UNTRANSLATED: "+fp+" (input type "+oneLine(text)+"): carried as a string")
				}
			case "description", "required", "default":
				if _, ok := scalarText(value); !ok {
					t.drop(def, k, t.untranslatedAt(fp, "input."+field.Value, "expected a scalar"))
					k -= 2
					continue
				}
			default:
				t.drop(def, k, t.untranslatedAt(fp, "input."+field.Value, "not part of the Spindle input schema"))
				k -= 2
				continue
			}
			t.translated++
		}
		if choiceWithoutOptions(def) {
			if ti := mapIndex(def, "type"); ti >= 0 {
				t.drop(def, ti, t.untranslatedAt(p+".type", "input type choice",
					"a choice input needs options; carried as a string input"))
			}
		}
	}
}

// choiceWithoutOptions reports a choice input the parser would reject.
func choiceWithoutOptions(def *yaml.Node) bool {
	typ := mapValue(def, "type")
	if typ == nil || typ.Kind != yaml.ScalarNode || typ.Value != "choice" {
		return false
	}
	opts := mapValue(def, "options")
	return opts == nil || opts.Kind != yaml.SequenceNode || len(opts.Content) == 0
}

// scalarListAt keeps a branches/tags filter. A scalar is accepted by the source
// dialects and the target wants a list, so it is rewritten to a one-element
// list (the same lossless rewrite as `needs: test`).
func (t *translator) scalarListAt(m *yaml.Node, i int, path, construct string) bool {
	val := m.Content[i+1]
	switch val.Kind {
	case yaml.ScalarNode:
		if isNull(val) {
			t.drop(m, i, "")
			return false
		}
		text := val.Value
		setSequence(val, []string{text})
		val.Style = yaml.FlowStyle
		t.note(path, construct+": "+text, "rewritten to a one-element list")
		t.translated++
		return true
	case yaml.SequenceNode:
		for j := 0; j < len(val.Content); j++ {
			if _, ok := scalarText(val.Content[j]); !ok {
				t.dropItem(val, j, t.untranslatedAt(path, construct, "a filter entry must be a string"))
				j--
				continue
			}
			t.translated++
		}
		return true
	default:
		t.drop(m, i, t.untranslatedAt(path, construct, "expected a string or a list of strings"))
		return false
	}
}

// scheduleTrigger validates each cron with the parser's own function. An entry
// the target scheduler cannot evaluate is refused and dropped (it would be a
// parse error for the whole file); when nothing survives, the trigger goes.
func (t *translator) scheduleTrigger(m *yaml.Node, i int, path string) bool {
	val := m.Content[i+1]
	if val.Kind != yaml.SequenceNode {
		t.drop(m, i, t.untranslatedAt(path, "on.schedule", "expected a list of `- cron: \"…\"` entries"))
		return false
	}
	for j := 0; j < len(val.Content); j++ {
		entry := val.Content[j]
		p := fmt.Sprintf("%s[%d]", path, j+1)
		if entry.Kind != yaml.MappingNode {
			t.dropItem(val, j, t.untranslatedAt(p, "schedule entry", "each entry must be a mapping with one `cron:` key"))
			j--
			continue
		}
		if len(entry.Content) != 2 && len(entry.Content) != 4 {
			t.dropItem(val, j, t.untranslatedAt(p, "schedule entry", "each entry must have `cron:` and an optional `timezone:`"))
			j--
			continue
		}
		if mapIndex(entry, "cron") < 0 || (len(entry.Content) == 4 && mapIndex(entry, "timezone") < 0) {
			t.dropItem(val, j, t.untranslatedAt(p, "schedule entry", "each entry must have `cron:` and an optional `timezone:`"))
			j--
			continue
		}
		expr, ok := scalarText(mapValue(entry, "cron"))
		if !ok {
			t.dropItem(val, j, t.untranslatedAt(p, "cron", "the expression must be a string"))
			j--
			continue
		}
		if _, err := workflow.ParseCron(expr); err != nil {
			t.dropItem(val, j, t.untranslatedAt(p, "cron: "+expr, err.Error()))
			j--
			continue
		}
		if tz := mapValue(entry, "timezone"); tz != nil {
			text, ok := scalarText(tz)
			if !ok {
				t.dropItem(val, j, t.untranslatedAt(p, "timezone", "expected an IANA timezone name"))
				j--
				continue
			}
			if _, err := (workflow.CronEntry{Cron: expr, Timezone: text}).Location(); err != nil {
				t.dropItem(val, j, t.untranslatedAt(p, "timezone: "+text, err.Error()))
				j--
				continue
			}
		}
		t.translated++
	}
	if len(val.Content) == 0 {
		t.drop(m, i, t.untranslatedAt(path, "on.schedule", "no cron entry survived conversion; the trigger is removed"))
		return false
	}
	return true
}

// typedTrigger translates a release/issues/issue_comment trigger: only `types`
// is part of the schema, and every type is checked against the set the owning
// service emits (a type that can never fire is dropped by name).
func (t *translator) typedTrigger(m *yaml.Node, i int, path, event string) bool {
	val := m.Content[i+1]
	if isNull(val) {
		return true
	}
	if val.Kind != yaml.MappingNode {
		t.drop(m, i, t.untranslatedAt(path, event, "the filters must be a mapping"))
		return false
	}
	supported := map[string][]string{
		"release":                     workflow.ReleaseTypes,
		"issues":                      workflow.IssuesTypes,
		"issue_comment":               workflow.IssueCommentTypes,
		"pull_request_review":         workflow.ReviewTypes,
		"pull_request_review_comment": workflow.ReviewCommentTypes,
		"label":                       workflow.LabelTypes,
		"milestone":                   workflow.MilestoneTypes,
	}[event]
	for j := 0; j < len(val.Content); j += 2 {
		f := val.Content[j]
		p := path + "." + f.Value
		if f.Value != "types" {
			t.drop(val, j, t.untranslatedAt(p, "on."+event+"."+f.Value, "not part of the Spindle trigger schema"))
			j -= 2
			continue
		}
		tv := val.Content[j+1]
		if tv.Kind != yaml.SequenceNode {
			t.drop(val, j, t.untranslatedAt(p, "on."+event+".types", "expected a list of lifecycle actions"))
			j -= 2
			continue
		}
		for k := 0; k < len(tv.Content); k++ {
			name, ok := scalarText(tv.Content[k])
			valid := false
			for _, s := range supported {
				if ok && s == name {
					valid = true
					break
				}
			}
			if valid {
				t.translated++
				continue
			}
			t.dropItem(tv, k, t.untranslatedAt(p, event+".types: "+name,
				"the "+event+" service never emits this action"))
			k--
		}
		if len(tv.Content) == 0 {
			t.drop(val, j, t.untranslatedAt(p, "on."+event+".types", "an empty type list matches nothing; omit `types` to fire on every emitted action"))
			j -= 2
		}
	}
	return true
}
