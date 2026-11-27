package threads

import (
	"fmt"
	"strings"
)

// Stock toolchain images (PAR-177, D-26).
//
// Threads publishes no hosted image registry of its own; the documented stock
// set is the upstream toolchain image each label maps to, declared the same way
// every label→image mapping is (PAR-171):
//
//	spindle runner serve \
//	  --labels self-hosted,linux \
//	           ubuntu-latest:docker://ubuntu:24.04 \
//	           node:docker://node:22-bookworm \
//	           python:docker://python:3.13-bookworm \
//	           go:docker://golang:1.25-bookworm \
//	           rust:docker://rust:1-bookworm \
//	           docker:docker://docker:27-cli
//
// Every image carries its @sha256: digest, so a stock label resolves to bytes
// the upstream project actually published rather than a mutable tag. The
// catalogue is data, not magic: `spindle runner images` prints it (and the
// exact `--labels` line), `spindle runner images --label go` resolves one
// label and refuses an unknown label by name, docs/threads.md §8.2 carries the
// same table, and the job runs in the image only when the workflow named no
// `container.image` of its own. A runner's own `label:docker://image` mapping
// still wins over the stock default (PAR-171), and an air-gapped instance
// reaches the same bytes through `--image-prefix` (§8.6).
//
// "Published" is the upstream projects' publication: spindle does not mirror
// them. The digest pins are the catalogue's contract; refresh one only by
// replacing the digest with the upstream manifest digest for the named tag.

// StockImage is one documented toolchain image.
type StockImage struct {
	// Label is the runs-on label the image serves (a normal label the job must
	// name; the hosted expansion still applies to ubuntu-*).
	Label string `json:"label"`
	// Image is the docker reference a runner declares after docker://, pinned
	// by digest.
	Image string `json:"image"`
	// Toolchain names what it provides, for the CLI/page table.
	Toolchain string `json:"toolchain"`
}

// StockImages is the documented set, ordered as the docs print it. The digests
// are the upstream multi-arch manifest digests (`docker buildx imagetools
// inspect`), so the same reference resolves on amd64 and arm64.
func StockImages() []StockImage {
	return []StockImage{
		{Label: "ubuntu-latest", Image: "ubuntu:24.04@sha256:562111b88caa79c71391c0c4d3c2114a7d5c35915c20561fd893cb1e07929e6b", Toolchain: "base Ubuntu 24.04 (git, curl, ca-certificates preinstalled by the operator's layer)"},
		{Label: "node", Image: "node:22-bookworm@sha256:7c400d78797d8cbe3f6bb21d2227b86af4cd24b0014cc9f62ff989cc16de988e", Toolchain: "Node.js 22 LTS + npm"},
		{Label: "python", Image: "python:3.13-bookworm@sha256:ee1a30c36c5cf43df939eb664489fb167e1bbe07cd884ea694054f6aff4dd9d9", Toolchain: "CPython 3.13 + pip"},
		{Label: "go", Image: "golang:1.25-bookworm@sha256:3b4a11519ad929d1e1d261a12cff056f0c85b735253d7d861346b9c6f8b36437", Toolchain: "Go 1.25 toolchain"},
		{Label: "rust", Image: "rust:1-bookworm@sha256:75d26c23c24cc6ea1cdabf0714a6bff8fd7bf3248716043fd7ae86dac7f41b94", Toolchain: "Rust stable + cargo"},
		{Label: "docker", Image: "docker:27-cli@sha256:851f91d241214e7c6db86513b270d58776379aacc5eb9c4a87e5b47115e3065c", Toolchain: "docker CLI + buildx (daemonless builds, §4.1a)"},
	}
}

// StockImageFor resolves one stock label to its pinned image. An unknown label
// is refused by name with the catalogue in the error: a typo must not silently
// fall back to a mutable tag or another toolchain.
func StockImageFor(label string) (StockImage, error) {
	key := strings.ToLower(strings.TrimSpace(label))
	for _, img := range StockImages() {
		if img.Label == key {
			return img, nil
		}
	}
	return StockImage{}, fmt.Errorf("unknown stock label %q; the catalogue is %s", label, strings.Join(StockImageLabels(), ", "))
}

// stockImageForRunsOn resolves the first stock label in a job's authored
// runs-on list. The first match wins, so the declaration order is the
// tie-break exactly like runnerImageFor.
func stockImageForRunsOn(runsOn []string) (StockImage, bool) {
	for _, label := range runsOn {
		if img, err := StockImageFor(label); err == nil {
			return img, true
		}
	}
	return StockImage{}, false
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
