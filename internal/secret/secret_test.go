package secret

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// Chmod, so the umask does not narrow the mode under test.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	for content, want := range map[string]string{
		"s3cret":       "s3cret",
		"s3cret\n":     "s3cret",
		"s3cret\r\n\n": "s3cret",
		" s3cret \t\n": " s3cret \t",
	} {
		path := filepath.Join(dir, "secret")
		write(t, path, content, 0o600)
		if got, err := ReadFile(path); err != nil || got != want {
			t.Errorf("ReadFile(%q) = %q, %v; want %q", content, got, err, want)
		}
	}
}

func TestReadFileErrors(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	write(t, empty, " \n", 0o600)
	large := filepath.Join(dir, "large")
	write(t, large, strings.Repeat("x", maxSize+1), 0o600)
	for name, path := range map[string]string{
		"missing":   filepath.Join(dir, "none"),
		"directory": dir,
		"empty":     empty,
		"too large": large,
	} {
		if _, err := ReadFile(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCredentialReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	write(t, path, "one\n", 0o600)
	c, err := NewFileCredential(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Value() != "one" {
		t.Fatalf("Value = %q", c.Value())
	}
	if changed, err := c.Reload(); changed || err != nil {
		t.Errorf("unchanged file: Reload = %v, %v", changed, err)
	}
	write(t, path, "two", 0o600)
	if changed, err := c.Reload(); !changed || err != nil || c.Value() != "two" {
		t.Errorf("rotated file: Reload = %v, %v; Value %q", changed, err, c.Value())
	}
	write(t, path, "", 0o600)
	if changed, err := c.Reload(); changed || err == nil || c.Value() != "two" {
		t.Errorf("emptied file: Reload = %v, %v; Value %q, want the previous secret kept", changed, err, c.Value())
	}
	if _, err := NewFileCredential(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestLooseMode(t *testing.T) {
	for mode, want := range map[os.FileMode]bool{
		0o600: false,
		0o400: false,
		0o604: true,
		0o644: true,
		// The file's group is this process's own, as a Kubernetes fsGroup mount is.
		0o640: false,
		0o440: false,
	} {
		// A file per mode: a read-only one cannot be rewritten for the next.
		path := filepath.Join(t.TempDir(), "secret")
		write(t, path, "s", mode)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := LooseMode(info); got != want {
			t.Errorf("mode %#o: LooseMode = %v, want %v", mode, got, want)
		}
	}
}
