package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIPublishRejectsEmptyPath(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(secretPath, []byte("SECRET=12345"), 0600); err != nil {
		t.Fatal(err)
	}

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origDir) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	err = run([]string{"publish", "--server", "http://127.0.0.1:18888", ""})
	if err == nil {
		t.Fatal("expected run() with empty path to return error, got nil")
	}
	if !strings.Contains(err.Error(), "empty publication path") {
		t.Fatalf("expected 'empty publication path' in error, got: %v", err)
	}
}
