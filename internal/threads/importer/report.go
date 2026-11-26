package importer

// FileReport is the machine-readable result for one source file.
type FileReport struct {
	// Source is the source path, repo-relative.
	Source string `json:"source"`
	// Output is the target path, repo-relative.
	Output string `json:"output"`
	// Translated counts the constructs carried across.
	Translated int `json:"translated"`
	// Parses is always true for a file that was produced: the importer
	// validates every output with workflow.Parse and treats a failure as an
	// error, never as a warning.
	Parses bool `json:"parses"`
	// Untranslated lists every construct that was refused. Each has a
	// matching `# UNTRANSLATED:` comment in the output.
	Untranslated []Item `json:"untranslated,omitempty"`
	// Notes lists the exact mechanical rewrites worth reviewing.
	Notes []Item `json:"notes,omitempty"`
}

// Report is one import run (one repository, one platform).
type Report struct {
	Platform Platform `json:"platform"`
	// Repo is the repository directory the run read.
	Repo string `json:"repo"`
	// Applied reports whether the converted files were committed.
	Applied bool `json:"applied"`
	// Branch is the new branch `--apply` created.
	Branch string `json:"branch,omitempty"`
	// Partial is true when at least one construct was refused anywhere.
	Partial bool         `json:"partial"`
	Files   []FileReport `json:"files"`
	// Outputs carries the converted bytes, keyed by target path. It is not
	// marshalled: the JSON report is a report, and the files are written with
	// --out or --apply.
	Outputs map[string][]byte `json:"-"`
}

// OK reports whether every file was fully translated. A partial conversion
// still writes (the report names what was lost), but the command exits non-zero
// so CI cannot mistake it for success.
func (r *Report) OK() bool {
	if r == nil {
		return false
	}
	for _, f := range r.Files {
		if !f.Parses || len(f.Untranslated) > 0 {
			return false
		}
	}
	return true
}

// Untranslated counts the refused constructs across every file.
func (r *Report) Untranslated() int {
	n := 0
	for _, f := range r.Files {
		n += len(f.Untranslated)
	}
	return n
}

// Translated counts the carried constructs across every file.
func (r *Report) Translated() int {
	n := 0
	for _, f := range r.Files {
		n += f.Translated
	}
	return n
}
