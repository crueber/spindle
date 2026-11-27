package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRunnerFileMultiScope(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runner.toml")
	body := `
[[runner]]
name = "org-build"
server = "https://spindle.example"
token_file = "/etc/spindle/org.token"
labels = ["self-hosted", "linux"]
capacity = 2

[[runner]]
name = "repo-build"
token_file = "/etc/spindle/repo.token"
labels = ["linux", "gpu"]
capacity = 1
ephemeral = true
max_cpus = 4
max_memory = "16GiB"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := loadRunnerFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(entries) != 2 || entries[0].Name != "org-build" || entries[1].Server != "" {
		t.Fatalf("entries = %+v", entries)
	}
	if !entries[1].Ephemeral || entries[1].MaxCPUs != 4 || entries[1].MaxMemory != "16GiB" {
		t.Fatalf("second entry = %+v", entries[1])
	}
}

func TestLoadRunnerFileRefusesBadEntries(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "r.toml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := map[string]string{
		"no entries":    ``,
		"no credential": "[[runner]]\nname=\"x\"\n",
		"two sources":   "[[runner]]\nname=\"x\"\ntoken_file=\"a\"\ntoken=\"b\"\n",
		"bad capacity":  "[[runner]]\nname=\"x\"\ntoken_file=\"a\"\ncapacity=99\n",
		"ephemeral cap": "[[runner]]\nname=\"x\"\ntoken_file=\"a\"\ncapacity=2\nephemeral=true\n",
		"bad max cpus":  "[[runner]]\nname=\"x\"\ntoken_file=\"a\"\nmax_cpus=-1\n",
	}
	for name, body := range cases {
		if _, err := loadRunnerFile(write(body)); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestReadSecretFileRequires0600(t *testing.T) {
	dir := t.TempDir()
	open := filepath.Join(dir, "open")
	if err := os.WriteFile(open, []byte("wrt_x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretFile(open); err == nil {
		t.Fatal("group/world-readable credential accepted")
	}
	closed := filepath.Join(dir, "closed")
	if err := writeSecretFile(closed, "wrt_y"); err != nil {
		t.Fatal(err)
	}
	got, err := readSecretFile(closed)
	if err != nil || got != "wrt_y" {
		t.Fatalf("read = %q, %v", got, err)
	}
}

func TestRunnerPoolRefusalNamesD14(t *testing.T) {
	c := isolateCLI(t, "http://127.0.0.1:1")
	code := runRunnerPool(t.Context(), c, nil)
	if code == exitOK {
		t.Fatal("pool command succeeded")
	}
}
