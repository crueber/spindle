package importer

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// stepsAt translates a job's step list. Steps are never silently dropped: a
// step whose shape cannot be carried becomes a step that fails with the reason,
// so the job can never look green after losing work.
func (t *translator) stepsAt(job *yaml.Node, i int, path string) {
	val := job.Content[i+1]
	if val.Kind != yaml.SequenceNode {
		t.drop(job, i, t.untranslatedAt(path, "steps", "expected a list of steps"))
		return
	}
	for j := 0; j < len(val.Content); j++ {
		step := val.Content[j]
		p := fmt.Sprintf("%s[%d]", path, j+1)
		if step.Kind != yaml.MappingNode {
			replaceStepWithFailure(val, j, p+": a step must be a mapping")
			t.untranslated = append(t.untranslated, Item{Path: p, Construct: "step",
				Reason: "a step must be a mapping; replaced by a step that fails"})
			continue
		}
		t.step(step, p)
	}
}

// step translates one step's fields, then enforces the target's one-verb rule.
func (t *translator) step(step *yaml.Node, path string) {
	for i := 0; i < len(step.Content); i += 2 {
		key := step.Content[i]
		p := path + "." + key.Value
		switch key.Value {
		case "id", "name":
			if _, ok := scalarText(step.Content[i+1]); !ok {
				t.drop(step, i, t.untranslatedAt(p, key.Value, "must be a string"))
				i -= 2
			}
		case "run":
			if _, ok := scalarText(step.Content[i+1]); !ok {
				t.drop(step, i, t.untranslatedAt(p, "run", "a script must be a string"))
				i -= 2
			}
		case "uses":
			if t.usesAt(step, i, p) {
				i -= 2
			}
		case "with", "env":
			t.stringMapAt(step, i, p)
		case "if", "shell", "working-directory":
			if _, ok := scalarText(step.Content[i+1]); !ok {
				t.drop(step, i, t.untranslatedAt(p, key.Value, "must be a string"))
				i -= 2
			}
		case "continue-on-error":
			if step.Content[i+1].Tag != "!!bool" {
				t.drop(step, i, t.untranslatedAt(p, "continue-on-error", "only a literal true/false is supported; an expression is not"))
				i -= 2
			}
		case "timeout-minutes":
			if step.Content[i+1].Tag != "!!int" {
				t.drop(step, i, t.untranslatedAt(p, "timeout-minutes", "only a whole number is supported; an expression is not"))
				i -= 2
			}
		default:
			t.drop(step, i, t.untranslatedAt(p, key.Value, "not part of the Spindle step schema"))
			i -= 2
		}
	}
	run, uses := mapIndex(step, "run") >= 0, mapIndex(step, "uses") >= 0
	switch {
	case run && uses:
		t.drop(step, mapIndex(step, "uses"),
			t.untranslatedAt(path+".uses", "uses", "a step has one verb; the source declared both run and uses, and run was kept"))
	case !run && !uses:
		appendFailureRun(step, path+": the step has neither run nor uses")
		step.Content[len(step.Content)-2].HeadComment = joinComment(step.Content[len(step.Content)-2].HeadComment,
			"# UNTRANSLATED: "+path+": the step has neither run nor uses")
		t.untranslated = append(t.untranslated, Item{Path: path, Construct: "step",
			Reason: "the step has neither run nor uses; replaced by a step that fails"})
	}
}

// usesAt maps `uses:` to one of the spellings Threads runs: the three built-ins,
// a repository-local `./path`, or `docker://image`. A remote action is refused
// in place: the key becomes a `run` step that fails and names it. It reports
// whether the key/value pair was removed (so the caller can fix its index).
func (t *translator) usesAt(step *yaml.Node, i int, path string) bool {
	val := step.Content[i+1]
	uses, ok := scalarText(val)
	if !ok {
		t.drop(step, i, t.untranslatedAt(path, "uses", "the action reference must be a string"))
		return true
	}
	base, hadRef := usesBase(uses)
	switch {
	case base == "actions/checkout", base == "actions/upload-artifact", base == "actions/download-artifact":
		if hadRef {
			t.note(path, "uses: "+uses, "the @ref is dropped: Threads runs its own built-in "+base)
		}
		setScalar(val, base)
		t.translated++
		return false
	case strings.HasPrefix(uses, LocalActionPrefix):
		t.translated++
		return false
	case strings.HasPrefix(strings.ToLower(uses), "docker://"):
		t.translated++
		if mapIndex(step, "with") >= 0 {
			t.note(path, "uses: "+uses, "`with:` on a docker:// step is not applied yet; pass arguments in the image or a local action")
		}
		return false
	}
	reason := "remote and marketplace actions are not supported; use a repository-local ./action or a docker:// image"
	comment := t.untranslatedAt(path, "uses: "+uses, reason)
	if mapIndex(step, "run") >= 0 {
		// Both verbs were declared and run is the one that can execute.
		t.drop(step, i, comment)
		return true
	}
	// The step must keep a verb the parser accepts: turn the pair into a run
	// step that fails loudly instead of dropping the step (which would make the
	// job look green while its work vanished). `with` is left in place; the
	// parser accepts it and the step's own failure is the outcome.
	key := step.Content[i]
	key.Value = "run"
	val.Kind = yaml.ScalarNode
	val.Tag = "!!str"
	val.Style = yaml.LiteralStyle
	val.Value = failureScript(path + ": " + reason + " (uses: " + uses + ")")
	key.HeadComment = joinComment(key.HeadComment, comment)
	return false
}

// LocalActionPrefix is the spelling that selects a repository-local action.
const LocalActionPrefix = "./"

// usesBase splits `owner/repo[/path]@ref` at the ref. A `docker://` reference is
// returned whole (its registry port and digest contain colons and @).
func usesBase(uses string) (base string, hadRef bool) {
	u := strings.TrimSpace(uses)
	if strings.HasPrefix(strings.ToLower(u), "docker://") {
		return u, false
	}
	if at := strings.LastIndexByte(u, '@'); at >= 0 {
		return u[:at], true
	}
	return u, false
}

// appendFailureRun gives a step that lost its verb a run body that fails.
func appendFailureRun(step *yaml.Node, reason string) {
	key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "run"}
	val := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Style: yaml.LiteralStyle, Value: failureScript(reason)}
	step.Content = append(step.Content, key, val)
}

// replaceStepWithFailure replaces one sequence entry with a failing step and
// marks the site with a comment (the script's own text is not a YAML comment).
func replaceStepWithFailure(steps *yaml.Node, index int, reason string) {
	step := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	appendFailureRun(step, reason)
	step.Content[0].HeadComment = "# UNTRANSLATED: " + oneLine(reason)
	if index >= 0 && index < len(steps.Content) {
		steps.Content[index] = step
		return
	}
	steps.Content = append(steps.Content, step)
}
