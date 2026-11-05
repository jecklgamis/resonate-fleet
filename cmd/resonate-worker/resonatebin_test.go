package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSiblingBin(t *testing.T) {
	dir := t.TempDir()
	if got := siblingBin(dir, "resonate"); got != "resonate" {
		t.Errorf("no sibling: got %q, want PATH lookup", got)
	}
	want := filepath.Join(dir, "resonate")
	if err := os.WriteFile(want, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := siblingBin(dir, "resonate"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
