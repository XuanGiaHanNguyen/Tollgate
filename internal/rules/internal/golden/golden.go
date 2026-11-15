// Package golden compares test output with files under testdata/.
// Run `go test ./internal/rules/... -update` to rewrite them, then review
// the diff before committing.
package golden

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// Check compares got with testdata/<name>.
func Check(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (run with -update): %v", path, err)
	}
	if string(want) != got {
		t.Errorf("%s mismatch\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

// Inputs lists testdata/*.tg files.
func Inputs(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "*.tg"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no testdata/*.tg inputs: %v", err)
	}
	return files
}

// Base strips the directory and .tg extension.
func Base(path string) string { return strings.TrimSuffix(filepath.Base(path), ".tg") }
