//go:build e2e

package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// Tier 3: Cross-Feature Combinations

// 1. Cross: Empty path mixed with valid Unicode paths and spaced filenames (ARTIFACT-1 + 3 + 4)
func TestTier3_Cross_EmptyMixedWithUnicodeAndSpaces(t *testing.T) {
	d := startTestDaemon(t)
	tmpDir := t.TempDir()

	validUnicode := writeTempFile(t, tmpDir, "résumé.html", "<h1>Résumé</h1>")
	validSpaced := writeTempFile(t, tmpDir, " notes .txt", "some notes")

	// Empty path must abort entire operation before upload
	res := runCLI(t, nil, "publish", "", validUnicode, validSpaced, "--server", d.URL)
	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit when empty path is mixed with valid files, got 0")
	}

	// Verify nothing was staged or published
	for _, sub := range []string{"staging", "published"} {
		entries, _ := os.ReadDir(filepath.Join(d.DataDir, sub))
		if len(entries) != 0 {
			t.Fatalf("expected 0 entries in %s, got %d", sub, len(entries))
		}
	}
}

// 2. Cross: Valid Unicode + Spaced Names + Custom Entry & Title (ARTIFACT-3 + ARTIFACT-4)
func TestTier3_Cross_UnicodeAndSpacedWithEntry(t *testing.T) {
	d := startTestDaemon(t)
	tmpDir := t.TempDir()

	writeTempFile(t, tmpDir, "index_ü.html", "<!doctype html><title>Ü</title><link rel='stylesheet' href=' style .css'>")
	writeTempFile(t, tmpDir, " style .css", "body { color: red; }")

	res := runCLI(t, nil, "publish", tmpDir, "--entry", "index_ü.html", "--title", "Unicode & Spaces Test", "--server", d.URL, "--json")
	if res.ExitCode != 0 {
		t.Fatalf("publish failed: %d: %s", res.ExitCode, res.Stderr)
	}

	var receipt struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Entry string `json:"entry"`
		URL   string `json:"url"`
		Files []struct {
			Path string `json:"path"`
			URL  string `json:"url"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &receipt); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}

	if receipt.Entry != "index_ü.html" {
		t.Fatalf("expected entry index_ü.html, got %q", receipt.Entry)
	}

	foundSpaced := false
	for _, f := range receipt.Files {
		if f.Path == " style .css" {
			foundSpaced = true
			req, _ := http.NewRequest("GET", f.URL, nil)
			resp, body := fetchHTTP(t, nil, req)
			if resp.StatusCode != 200 || string(body) != "body { color: red; }" {
				t.Fatalf("failed fetching spaced file: %d %s", resp.StatusCode, string(body))
			}
		}
	}
	if !foundSpaced {
		t.Fatal("did not find ' style .css' with preserved spaces in receipt files")
	}
}

// 3. Cross: Range requests on spaced and Unicode files with no-store check (ARTIFACT-4 + ARTIFACT-9)
func TestTier3_Cross_RangeOnSpacedFile(t *testing.T) {
	d := startTestDaemon(t)
	tmpDir := t.TempDir()

	spacedFile := writeTempFile(t, tmpDir, " spaced_data .bin", "abcdefghijklmnopqrstuvwxyz")
	res := runCLI(t, nil, "publish", spacedFile, "--server", d.URL, "--json")
	if res.ExitCode != 0 {
		t.Fatalf("publish failed: %s", res.Stderr)
	}

	var receipt struct {
		Files []struct {
			URL string `json:"url"`
		} `json:"files"`
	}
	_ = json.Unmarshal([]byte(res.Stdout), &receipt)
	targetURL := receipt.Files[0].URL

	// 1. Valid range request
	req206, _ := http.NewRequest("GET", targetURL, nil)
	req206.Header.Set("Range", "bytes=0-4")
	resp206, body206 := fetchHTTP(t, nil, req206)
	if resp206.StatusCode != http.StatusPartialContent || string(body206) != "abcde" {
		t.Fatalf("expected 206 'abcde', got %d %q", resp206.StatusCode, string(body206))
	}
	if resp206.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("206 missing Cache-Control: no-store, got %q", resp206.Header.Get("Cache-Control"))
	}

	// 2. Invalid range request
	req416, _ := http.NewRequest("GET", targetURL, nil)
	req416.Header.Set("Range", "bytes=100-200")
	resp416, _ := fetchHTTP(t, nil, req416)
	if resp416.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("expected 416, got %d", resp416.StatusCode)
	}
	if resp416.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("416 missing Cache-Control: no-store, got %q", resp416.Header.Get("Cache-Control"))
	}
}

// 4. Cross: Bare delimiter rejected when uploading multiple files with options (ARTIFACT-5 + options)
func TestTier3_Cross_BareDelimiterWithFullOptions(t *testing.T) {
	d := startTestDaemon(t)
	tmpDir := t.TempDir()
	f := writeTempFile(t, tmpDir, "index.html", "<h1>Hello</h1>")

	badServer := d.URL + "/?"
	res := runCLI(t, nil, "publish", f, "--server", badServer, "--title", "Test Title", "--entry", "index.html", "--json")
	if res.ExitCode == 0 {
		t.Fatalf("expected exit error for server %q, got 0", badServer)
	}
}

// 5. Cross: Large inventory with explicit entry point (ARTIFACT-2)
func TestTier3_Cross_LargeInventoryWithExplicitEntry(t *testing.T) {
	d := startTestDaemon(t)
	tmpDir := t.TempDir()

	writeTempFile(t, tmpDir, "main.html", "<h1>Main Entry</h1>")
	for i := 0; i < 200; i++ {
		writeTempFile(t, tmpDir, fmt.Sprintf("sub/item_%03d.txt", i), "item")
	}

	res := runCLI(t, nil, "publish", tmpDir, "--entry", "main.html", "--server", d.URL, "--json")
	if res.ExitCode != 0 {
		t.Fatalf("publish failed: %s", res.Stderr)
	}

	var receipt struct {
		Entry string `json:"entry"`
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	_ = json.Unmarshal([]byte(res.Stdout), &receipt)
	if receipt.Entry != "main.html" || len(receipt.Files) != 201 {
		t.Fatalf("unexpected receipt: entry=%q, files=%d", receipt.Entry, len(receipt.Files))
	}
}
