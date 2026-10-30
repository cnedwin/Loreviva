package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	example "loreviva.local/loreviva/examples/skills/source-aware-reply"
)

func TestExampleWriterNeverOverwritesAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "example.wepkg")
	if e := run([]string{path}); e != nil {
		t.Fatal(e)
	}
	actual, e := os.ReadFile(path)
	expected, _, build := example.Build()
	if e != nil || build != nil || !bytes.Equal(actual, expected) {
		t.Fatal("writer output differs from original source", e, build)
	}
	if e = run([]string{path}); e == nil {
		t.Fatal("existing user file overwritten")
	}
	after, e := os.ReadFile(path)
	if e != nil || !bytes.Equal(after, actual) {
		t.Fatal("failed second write changed file", e)
	}
}
