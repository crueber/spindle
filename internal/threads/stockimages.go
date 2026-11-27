package threads

// Stock toolchain images (PAR-177).
//
// Threads publishes no hosted image registry of its own; the documented stock
// set is the upstream toolchain image each label maps to, declared the same way
// every label→image mapping is (PAR-171):
//
//	spindle runner serve \
//	  --labels self-hosted,linux \
//	           ubuntu-latest:docker://ubuntu:24.04 \
//	           node:docker://node:lts \
//	           python:docker://python:3.13 \
//	           go:docker://golang:1.25 \
//	           rust:docker://rust:1 \
//	           docker:docker://docker:27-cli
//
// The catalog is data, not magic: `spindle runner images` prints it (and the
// exact `--labels` line), docs/threads.md §8.2 carries the same table, and the
// job runs in the image only when the workflow named no `container.image` of
// its own. An operator who wants pinned/digest images changes the mapping (or
// adds `--image-require-digest`); the runner never invents an image.
//
// "Published" is the upstream projects' publication: spindle does not mirror
// them. A private mirror is `--image-prefix` (§8.6), which is how an air-gapped
// instance serves the same stock set.

// StockImage is one documented toolchain image.
type StockImage struct {
	// Label is the runs-on label the image serves (a normal label the job must
	// name; the hosted expansion still applies to ubuntu-*).
	Label string `json:"label"`
	// Image is the docker reference a runner declares after docker://.
	Image string `json:"image"`
	// Toolchain names what it provides, for the CLI/page table.
	Toolchain string `json:"toolchain"`
}

// StockImages is the documented set, ordered as the docs print it.
func StockImages() []StockImage {
	return []StockImage{
		{Label: "ubuntu-latest", Image: "ubuntu:24.04", Toolchain: "base Ubuntu 24.04 (git, curl, ca-certificates preinstalled by the operator's layer)"},
		{Label: "node", Image: "node:lts", Toolchain: "Node.js LTS + npm"},
		{Label: "python", Image: "python:3.13", Toolchain: "CPython 3.13 + pip"},
		{Label: "go", Image: "golang:1.25", Toolchain: "Go 1.25 toolchain"},
		{Label: "rust", Image: "rust:1", Toolchain: "Rust stable + cargo"},
		{Label: "docker", Image: "docker:27-cli", Toolchain: "docker CLI + buildx (daemonless builds, §4.1a)"},
	}
}

// StockImageLabels returns the label part of the catalog (the CLI's column).
func StockImageLabels() []string {
	out := make([]string, 0, len(StockImages()))
	for _, img := range StockImages() {
		out = append(out, img.Label)
	}
	return out
}

// StockImageLabelArgs renders the `--labels` value for the whole catalog
// (comma-joined `label:docker://image` entries), for the CLI's copy-paste line.
// An operator normally trims it to the toolchains the host actually needs.
func StockImageLabelArgs() []string {
	out := make([]string, 0, len(StockImages()))
	for _, img := range StockImages() {
		out = append(out, img.Label+":docker://"+img.Image)
	}
	return out
}
