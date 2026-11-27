package actions

// Action metadata parsing (PAR-071/072/074). The parser is deliberately strict,
// like the workflow parser: an unknown key, a missing `using:`, a composite run
// step without `shell:`, a node action without `main:`, or a path escaping the
// action directory is a named error on the JOB (resolution runs before any
// runner or container), never a silently ignored typo.
//
// This parser exists beside internal/threads/workflow's local-action parser
// rather than replacing it because the remote shape is wider: `using: node20` /
// `node24` with pre/post hooks, `image: Dockerfile` (a build), and a nested
// remote `uses:` inside a composite (PAR-074). It is the ONE parser for fetched
// action metadata, so a remote composite and a remote node action are validated
// by the same rules.

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Using is an action's execution runtime.
const (
	UsingComposite = "composite"
	UsingDocker    = "docker"
	UsingNode20    = "node20"
	UsingNode24    = "node24"
)

// Input is one declared action input. Default is the YAML scalar text so an
// unquoted `default: true` reads "true" (GitHub's rule: inputs are strings in
// the action's environment).
type Input struct {
	Description string
	Required    bool
	Default     string
	HasDefault  bool
}

// Action is one resolved remote action.
type Action struct {
	// Ref is what the workflow named; Commit is the SHA it resolved to.
	Ref    Ref
	Commit string
	// Digest is the sha256 of Name/Digest() over the action's file set.
	Digest string
	// Name/Description come from the metadata (display only).
	Name        string
	Description string
	Using       string
	Inputs      map[string]Input
	// Outputs maps a composite output to its value template. A node action's
	// declared outputs carry no template (the program writes them through
	// $GITHUB_OUTPUT).
	Outputs map[string]string
	// Steps is the composite body.
	Steps []Step
	// Image is a docker:// reference (registry image) OR a Dockerfile path
	// inside the action ("Dockerfile").
	Image      string
	Entrypoint []string
	Args       []string
	Env        map[string]string
	// Main/Pre/Post are action-directory-relative program paths (node actions).
	Main, Pre, Post string
	// PreIf/PostIf are the hook conditions ("" means always()).
	PreIf, PostIf string
	// Files is the bounded action content, paths relative to the action
	// directory.
	Files []File
	// DirName is the stable in-container directory name the runner
	// materializes the content into (derived from the digest).
	DirName string
}

// IsDockerfile reports whether the docker action builds its own image.
func (a *Action) IsDockerfile() bool {
	return a != nil && a.Using == UsingDocker && !strings.Contains(a.Image, "://")
}

// Step is one composite step (the same vocabulary as a workflow step, minus
// the job-only controls a composite may not use).
type Step struct {
	ID               string
	Name             string
	Run              string
	Uses             string
	With             map[string]string
	Env              map[string]string
	If               string
	Shell            string
	WorkingDirectory string
	ContinueOnError  bool
	TimeoutMinutes   int
}

// File is one action-content file.
type File struct {
	Path string
	// Mode is 0o644 or 0o755 (git's blob modes); a symlink or submodule is
	// refused before this point.
	Mode uint32
	Data []byte
}

// metadataYAML mirrors action.yml. It is a separate struct from Action because
// the parser accepts the GitHub key set (author/branding are display-only and
// dropped) and needs scalar-or-list and scalar-map decoding.
type metadataYAML struct {
	Name        string                `yaml:"name"`
	Description string                `yaml:"description"`
	Author      string                `yaml:"author"`
	Branding    map[string]any        `yaml:"branding"`
	Inputs      map[string]inputYAML  `yaml:"inputs"`
	Outputs     map[string]outputYAML `yaml:"outputs"`
	Runs        runsYAML              `yaml:"runs"`
}

type inputYAML struct {
	Description string  `yaml:"description"`
	Required    bool    `yaml:"required"`
	Default     *scalar `yaml:"default"`
}

type outputYAML struct {
	Description string `yaml:"description"`
	Value       string `yaml:"value"`
}

type runsYAML struct {
	Using      string         `yaml:"using"`
	Steps      []stepYAML     `yaml:"steps"`
	Image      string         `yaml:"image"`
	Main       string         `yaml:"main"`
	Pre        string         `yaml:"pre"`
	Post       string         `yaml:"post"`
	PreIf      string         `yaml:"pre-if"`
	PostIf     string         `yaml:"post-if"`
	Entrypoint scalarOrList   `yaml:"entrypoint"`
	Args       scalarOrList   `yaml:"args"`
	Env        map[string]any `yaml:"env"`
}

type stepYAML struct {
	ID               string         `yaml:"id"`
	Name             string         `yaml:"name"`
	Run              string         `yaml:"run"`
	Uses             string         `yaml:"uses"`
	With             map[string]any `yaml:"with"`
	Env              map[string]any `yaml:"env"`
	If               string         `yaml:"if"`
	Shell            string         `yaml:"shell"`
	WorkingDirectory string         `yaml:"working-directory"`
	ContinueOnError  bool           `yaml:"continue-on-error"`
	TimeoutMinutes   int            `yaml:"timeout-minutes"`
	// The job-only controls are decoded so their presence can be REFUSED by
	// name (a KnownFields error would call them "unknown", which is less
	// useful than "not accepted inside a composite action").
	Background bool   `yaml:"background"`
	Wait       string `yaml:"wait"`
	WaitAll    bool   `yaml:"wait-all"`
	Cancel     string `yaml:"cancel"`
	Parallel   []any  `yaml:"parallel"`
}

// scalar decodes any YAML scalar as its text (GitHub's rule for defaults).
type scalar string

func (s *scalar) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*s = scalar(n.Value)
		return nil
	case yaml.AliasNode:
		return s.UnmarshalYAML(n.Alias)
	default:
		return fmt.Errorf("expected a scalar value")
	}
}

// scalarOrList accepts GitHub's string-or-list spelling (entrypoint/args).
type scalarOrList []string

func (s *scalarOrList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
			*s = nil
			return nil
		}
		*s = []string{n.Value}
		return nil
	case yaml.SequenceNode:
		out := make([]string, 0, len(n.Content))
		for _, item := range n.Content {
			if item.Kind != yaml.ScalarNode {
				return fmt.Errorf("entries must be strings")
			}
			out = append(out, item.Value)
		}
		*s = out
		return nil
	default:
		return fmt.Errorf("expected a string or a list of strings")
	}
}

// ParseAction decodes and validates one action metadata file.
func ParseAction(src []byte) (*Action, error) {
	if strings.TrimSpace(string(src)) == "" {
		return nil, fmt.Errorf("action metadata is empty")
	}
	dec := yaml.NewDecoder(strings.NewReader(string(src)))
	dec.KnownFields(true)
	var m metadataYAML
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("action metadata: %w", err)
	}
	var extra metadataYAML
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("action metadata: more than one document")
	}
	a := &Action{
		Name: m.Name, Description: m.Description,
		Inputs:  map[string]Input{},
		Outputs: map[string]string{},
	}
	for name, in := range m.Inputs {
		if !validInputName(name) {
			return nil, fmt.Errorf("action.inputs: %q is not a valid input name", name)
		}
		entry := Input{Description: in.Description, Required: in.Required}
		if in.Default != nil {
			entry.Default, entry.HasDefault = string(*in.Default), true
		}
		a.Inputs[name] = entry
	}
	for name, out := range m.Outputs {
		if !validInputName(name) {
			return nil, fmt.Errorf("action.outputs: %q is not a valid output name", name)
		}
		a.Outputs[name] = out.Value
	}
	a.Using = strings.ToLower(strings.TrimSpace(m.Runs.Using))
	switch a.Using {
	case UsingComposite:
		if err := parseComposite(a, m.Runs); err != nil {
			return nil, err
		}
	case UsingDocker:
		if err := parseDocker(a, m.Runs); err != nil {
			return nil, err
		}
	case UsingNode20, UsingNode24:
		if err := parseNode(a, m.Runs); err != nil {
			return nil, err
		}
	case "node16", "node12":
		return nil, fmt.Errorf("action.runs.using: %q is an end-of-life Node runtime; spindle runs node20 and node24 actions (use a maintained release)", m.Runs.Using)
	case "":
		return nil, fmt.Errorf("action.runs.using is required (composite, docker, node20 or node24)")
	default:
		return nil, fmt.Errorf("action.runs.using: %q is not supported (spindle runs composite, docker, node20 and node24 actions)", m.Runs.Using)
	}
	return a, nil
}

// parseComposite validates the composite body.
func parseComposite(a *Action, runs runsYAML) error {
	if runs.Image != "" || runs.Main != "" || runs.Pre != "" || runs.Post != "" {
		return fmt.Errorf("action.runs: a composite action has using and steps (image/main/pre/post belong to docker or node actions)")
	}
	if len(runs.Entrypoint) > 0 || len(runs.Args) > 0 || len(runs.Env) > 0 || runs.PreIf != "" || runs.PostIf != "" {
		return fmt.Errorf("action.runs: a composite action has using and steps (entrypoint/args/env/pre-if/post-if belong to docker or node actions)")
	}
	if len(runs.Steps) == 0 {
		return fmt.Errorf("action.runs.steps: a composite action needs at least one step")
	}
	a.Steps = make([]Step, 0, len(runs.Steps))
	declared := map[string]bool{}
	for i, in := range runs.Steps {
		where := fmt.Sprintf("action.runs.steps[%d]", i+1)
		if in.Background || in.Wait != "" || in.WaitAll || in.Cancel != "" || in.Parallel != nil {
			return fmt.Errorf("%s: background, wait/cancel and parallel steps are not accepted inside a composite action", where)
		}
		st := Step{
			ID: strings.TrimSpace(in.ID), Name: in.Name, Run: in.Run, Uses: strings.TrimSpace(in.Uses),
			If: in.If, Shell: strings.TrimSpace(in.Shell),
			WorkingDirectory: in.WorkingDirectory, ContinueOnError: in.ContinueOnError,
			TimeoutMinutes: in.TimeoutMinutes,
			With:           stringMap(in.With), Env: stringMap(in.Env),
		}
		switch {
		case st.Run != "" && st.Uses != "":
			return fmt.Errorf("%s: a step has both run and uses", where)
		case st.Run == "" && st.Uses == "":
			return fmt.Errorf("%s: a step needs run or uses", where)
		case st.Run != "":
			if st.Shell == "" {
				return fmt.Errorf("%s: a run step in a composite action must name its shell (shell: bash|sh|python)", where)
			}
		default:
			kind, rest, err := classifyUses(st.Uses)
			if err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
			switch kind {
			case usesLocal:
				if _, err := cleanActionPath(strings.TrimPrefix(rest, "./"), true); err != nil {
					return fmt.Errorf("%s: %w", where, err)
				}
			case usesDocker:
				if strings.TrimSpace(rest) == "" {
					return fmt.Errorf("%s: uses: docker:// needs an image reference", where)
				}
			case usesRemote:
				if _, err := ParseRef(st.Uses); err != nil {
					return fmt.Errorf("%s: %w", where, err)
				}
			}
		}
		if st.ID != "" {
			if !validStepID(st.ID) {
				return fmt.Errorf("%s: id %q is not a valid step id", where, st.ID)
			}
			if declared[strings.ToLower(st.ID)] {
				return fmt.Errorf("%s: duplicate step id %q", where, st.ID)
			}
			declared[strings.ToLower(st.ID)] = true
		}
		a.Steps = append(a.Steps, st)
	}
	// Every composite output needs a value template; a node action's outputs
	// are written at run time, so this check is composite-only.
	names := make([]string, 0, len(a.Outputs))
	for name := range a.Outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.TrimSpace(a.Outputs[name]) == "" {
			return fmt.Errorf("action.outputs.%s: value is required (a ${{ steps.<id>.outputs.<x> }} reference)", name)
		}
	}
	return nil
}

// parseDocker validates a docker action: a registry image (`docker://…`) or a
// Dockerfile inside the action directory (PAR-072).
func parseDocker(a *Action, runs runsYAML) error {
	if len(runs.Steps) > 0 || runs.Main != "" || runs.Pre != "" || runs.Post != "" {
		return fmt.Errorf("action.runs: a docker action has using, image, entrypoint, args and env")
	}
	if len(a.Outputs) > 0 {
		return fmt.Errorf("action.outputs: a docker action cannot declare outputs (only a composite action has steps to read them from)")
	}
	image := strings.TrimSpace(runs.Image)
	switch {
	case image == "":
		return fmt.Errorf("action.runs.image: a docker action needs image: docker://<image>[:tag|@digest] or a Dockerfile path")
	case strings.HasPrefix(strings.ToLower(image), "docker://"):
		image = strings.TrimSpace(image[len("docker://"):])
		if image == "" {
			return fmt.Errorf("action.runs.image: docker:// needs an image reference")
		}
		a.Image = "docker://" + image
	default:
		rel, err := cleanActionPath(image, false)
		if err != nil {
			return fmt.Errorf("action.runs.image: %v (a registry image needs the docker:// prefix)", err)
		}
		if !isDockerfileName(rel) {
			return fmt.Errorf("action.runs.image: %q is neither a docker:// image nor a Dockerfile inside the action (e.g. image: Dockerfile)", image)
		}
		a.Image = rel
	}
	a.Entrypoint = append([]string{}, runs.Entrypoint...)
	a.Args = append([]string{}, runs.Args...)
	a.Env = stringMap(runs.Env)
	return nil
}

// parseNode validates a JavaScript action (`using: node20|node24`).
func parseNode(a *Action, runs runsYAML) error {
	if len(runs.Steps) > 0 || runs.Image != "" || len(runs.Entrypoint) > 0 || len(runs.Args) > 0 {
		return fmt.Errorf("action.runs: a node action has using, main, and optionally pre/post/pre-if/post-if")
	}
	main, err := cleanActionPath(runs.Main, false)
	if err != nil {
		return fmt.Errorf("action.runs.main: %v", err)
	}
	if !strings.HasSuffix(strings.ToLower(main), ".js") && !strings.HasSuffix(strings.ToLower(main), ".cjs") {
		return fmt.Errorf("action.runs.main: %q must be a .js or .cjs file", runs.Main)
	}
	a.Main = main
	for field, value := range map[string]string{"pre": runs.Pre, "post": runs.Post} {
		if strings.TrimSpace(value) == "" {
			continue
		}
		rel, perr := cleanActionPath(value, false)
		if perr != nil {
			return fmt.Errorf("action.runs.%s: %v", field, perr)
		}
		if field == "pre" {
			a.Pre = rel
		} else {
			a.Post = rel
		}
	}
	a.PreIf, a.PostIf = strings.TrimSpace(runs.PreIf), strings.TrimSpace(runs.PostIf)
	if a.PreIf != "" && a.Pre == "" {
		return fmt.Errorf("action.runs.pre-if: pre-if needs a pre hook")
	}
	if a.PostIf != "" && a.Post == "" {
		return fmt.Errorf("action.runs.post-if: post-if needs a post hook")
	}
	a.Env = stringMap(runs.Env)
	return nil
}

// usesKind classifies a composite step's `uses:` value.
type usesKind int

const (
	usesBuiltin usesKind = iota
	usesLocal
	usesDocker
	usesRemote
)

// classifyUses decides which resolution a `uses:` value needs. A bare name is
// a built-in action (the runner's table); anything containing '/' and '@' is a
// remote reference, and a full URL is always remote.
func classifyUses(uses string) (usesKind, string, error) {
	trimmed := strings.TrimSpace(uses)
	switch {
	case trimmed == "":
		return usesBuiltin, "", fmt.Errorf("uses is empty")
	case strings.HasPrefix(trimmed, "./"):
		return usesLocal, trimmed, nil
	case strings.HasPrefix(strings.ToLower(trimmed), "docker://"):
		return usesDocker, trimmed[len("docker://"):], nil
	case strings.Contains(trimmed, "://"):
		return usesRemote, trimmed, nil
	case strings.Contains(trimmed, "@"):
		return usesRemote, trimmed, nil
	case strings.Contains(trimmed, "/"):
		return usesRemote, trimmed, nil
	default:
		return usesBuiltin, trimmed, fmt.Errorf("uses: %q is not a supported action reference (expected owner/repo@ref, ./, docker://, or a built-in action)", uses)
	}
}

// cleanActionPath validates a metadata-relative file path. It never escapes the
// action directory: this path is a container path after materialization.
func cleanActionPath(raw string, allowEmpty bool) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		if allowEmpty {
			return "", nil
		}
		return "", fmt.Errorf("path is empty")
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || strings.Contains(p, "\x00") {
		return "", fmt.Errorf("path %q must be action-directory-relative", raw)
	}
	cleaned := strings.TrimPrefix(p, "./")
	segs := strings.Split(cleaned, "/")
	out := make([]string, 0, len(segs))
	for _, seg := range segs {
		switch seg {
		case "", ".":
			continue
		case "..":
			return "", fmt.Errorf("path %q escapes the action directory", raw)
		}
		out = append(out, seg)
	}
	if len(out) == 0 {
		if allowEmpty {
			return "", nil
		}
		return "", fmt.Errorf("path %q escapes the action directory", raw)
	}
	return strings.Join(out, "/"), nil
}

// isDockerfileName reports whether p names a Dockerfile (GitHub accepts any
// path ending in Dockerfile, commonly "Dockerfile" or "docker/Dockerfile").
func isDockerfileName(p string) bool {
	base := p
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	return strings.HasPrefix(base, "Dockerfile") || strings.HasPrefix(base, "dockerfile")
}

// validInputName matches GitHub's input/output name shape: a letter or digit,
// then letters, digits, '-', '_' or '.'.
func validInputName(name string) bool {
	if name == "" || len(name) > 100 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
		if !ok {
			return false
		}
	}
	return true
}

// validStepID is a GitHub-style step id (letters, digits, '-', '_').
func validStepID(id string) bool {
	if id == "" || len(id) > 100 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}

// stringMap coerces a YAML mapping of scalars to string values (an unquoted
// `true` or `3` becomes its text, GitHub's rule for `with:`/`env:`).
func stringMap(in map[string]any) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		switch val := v.(type) {
		case nil:
			out[k] = ""
		case string:
			out[k] = val
		case bool:
			out[k] = fmt.Sprint(val)
		case int:
			out[k] = fmt.Sprint(val)
		case int64:
			out[k] = fmt.Sprint(val)
		case uint64:
			out[k] = fmt.Sprint(val)
		case float64:
			out[k] = strings.TrimSuffix(fmt.Sprint(val), ".0")
		default:
			out[k] = fmt.Sprint(val)
		}
	}
	return out
}
