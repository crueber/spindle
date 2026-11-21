package config

// setup_backup_test.go — the /setup canonical-write contract (11 §3.7): the
// editor writes the whole document, so the FIRST rewrite of an existing file
// keeps the operator's text in spindle.toml.bak-<utc>; a no-op save is
// byte-identical and makes no backup, and later saves never add another.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveSetupKeepsOneBackupOfExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	// A hand-written file: the canonical document plus an operator comment.
	body, err := MarshalTOML(FirstRunDefaults(dir))
	if err != nil {
		t.Fatal(err)
	}
	handWritten := append([]byte("# operator note: keep this line across saves\n"), body...)
	if err := os.WriteFile(path, handWritten, 0o600); err != nil {
		t.Fatal(err)
	}

	base, err := LoadSetupBase(dir, []string{path})
	if err != nil {
		t.Fatal(err)
	}
	backup, err := SaveSetup(base, dir)
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	if backup == "" || !strings.HasPrefix(backup, ConfigFileName+".bak-") {
		t.Fatalf("first canonicalising save returned backup %q", backup)
	}
	kept, err := os.ReadFile(filepath.Join(dir, backup))
	if err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	if !bytes.Equal(kept, handWritten) {
		t.Fatalf("backup is not the operator's original text:\n%s", kept)
	}
	// The rewrite is canonical and value-identical.
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "operator note") {
		t.Fatal("the canonical rewrite kept a comment the TOML encoder cannot represent")
	}
	after, err := LoadSetupBase(dir, []string{path})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := MarshalTOML(base)
	got, _ := MarshalTOML(after)
	if !bytes.Equal(want, got) {
		t.Fatalf("the canonical rewrite changed values:\n--- before ---\n%s\n--- after ---\n%s", want, got)
	}

	// A second save is byte-identical: no file write, no second backup.
	backup2, err := SaveSetup(after, dir)
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if backup2 != "" {
		t.Fatalf("a no-op save made another backup: %q", backup2)
	}
	saved2, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(saved2, saved) {
		t.Fatalf("a no-op save changed the file (err=%v)", err)
	}
	if all, _ := filepath.Glob(path + ".bak-*"); len(all) != 1 {
		t.Fatalf("backups = %v, want exactly one", all)
	}

	// A later real edit rewrites the canonical file and adds no backup.
	changed := *after
	changed.Server.Listen = "127.0.0.1:9099"
	backup3, err := SaveSetup(&changed, dir)
	if err != nil {
		t.Fatalf("changed save: %v", err)
	}
	if backup3 != "" {
		t.Fatalf("later save added a backup: %q", backup3)
	}
	if all, _ := filepath.Glob(path + ".bak-*"); len(all) != 1 {
		t.Fatalf("backups = %v, want exactly one", all)
	}
}

func TestSaveSetupFreshAndEmptyFilesMakeNoBackup(t *testing.T) {
	dir := t.TempDir()
	c := FirstRunDefaults(dir)
	if backup, err := SaveSetup(c, dir); err != nil || backup != "" {
		t.Fatalf("fresh save = %q err=%v, want no backup", backup, err)
	}
	// An existing empty file is not operator text worth keeping.
	dir2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir2, ConfigFileName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if backup, err := SaveSetup(FirstRunDefaults(dir2), dir2); err != nil || backup != "" {
		t.Fatalf("empty-file save = %q err=%v, want no backup", backup, err)
	}
}
