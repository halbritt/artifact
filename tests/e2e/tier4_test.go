//go:build e2e

package e2e_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
)

// Tier 4: Real-World Workloads

// 1. Workload: Complete static web application publishing with relative asset resolution
func TestTier4_Workload_StaticSitePublishAndAssetResolution(t *testing.T) {
	d := startTestDaemon(t)
	siteDir := t.TempDir()

	siteFiles := map[string]string{
		"index.html": `<!doctype html>
<html>
<head><link rel="stylesheet" href="assets/style.css"></head>
<body>
  <h1>Analytics Dashboard</h1>
  <img src="images/logo.svg">
  <script src="assets/app.js"></script>
</body>
</html>`,
		"assets/style.css":  "body { font-family: sans-serif; }",
		"assets/app.js":     "console.log('Artifact App Loaded');",
		"images/logo.svg":   `<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><rect width="10" height="10" fill="blue"/></svg>`,
		"data/metrics.csv": "timestamp,cpu,mem\n2026-11-05T00:00:00Z,12.5,45.2\n",
	}

	for rel, content := range siteFiles {
		writeTempFile(t, siteDir, rel, content)
	}

	res := runCLI(t, nil, "publish", siteDir, "--title", "Production Analytics", "--server", d.URL, "--json")
	if res.ExitCode != 0 {
		t.Fatalf("publish static site failed: %s", res.Stderr)
	}

	var receipt struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		Entry       string `json:"entry"`
		URL         string `json:"url"`
		FilesURL    string `json:"files_url"`
		DownloadURL string `json:"download_url"`
		Files       []struct {
			Path        string `json:"path"`
			SHA256      string `json:"sha256"`
			URL         string `json:"url"`
			DownloadURL string `json:"download_url"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &receipt); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}

	if receipt.Entry != "index.html" {
		t.Fatalf("expected entry index.html, got %q", receipt.Entry)
	}

	// Fetch each file and verify content and SHA256
	for _, f := range receipt.Files {
		expectedContent, ok := siteFiles[f.Path]
		if !ok {
			t.Fatalf("unexpected file in receipt: %s", f.Path)
		}
		req, _ := http.NewRequest("GET", f.URL, nil)
		resp, body := fetchHTTP(t, nil, req)
		if resp.StatusCode != 200 || string(body) != expectedContent {
			t.Fatalf("file %s mismatch: status %d, got %q", f.Path, resp.StatusCode, string(body))
		}
		sum := fmt.Sprintf("%x", sha256.Sum256(body))
		if f.SHA256 != sum {
			t.Fatalf("checksum mismatch for %s: receipt=%s, computed=%s", f.Path, f.SHA256, sum)
		}
	}
}

// 2. Workload: Full ZIP bundle download and archive integrity check
func TestTier4_Workload_ZipBundleDownloadAndVerification(t *testing.T) {
	d := startTestDaemon(t)
	tmpDir := t.TempDir()

	contents := map[string]string{
		"manifest.txt":    "version 1.0\n",
		"lib/core.js":     "export const ready = true;",
		"docs/readme.md":  "# Readme Documentation",
	}
	for p, c := range contents {
		writeTempFile(t, tmpDir, p, c)
	}

	res := runCLI(t, nil, "publish", tmpDir, "--server", d.URL, "--json")
	if res.ExitCode != 0 {
		t.Fatalf("publish failed: %s", res.Stderr)
	}

	var receipt struct {
		DownloadURL string `json:"download_url"`
	}
	_ = json.Unmarshal([]byte(res.Stdout), &receipt)

	// Download ZIP
	req, _ := http.NewRequest("GET", receipt.DownloadURL, nil)
	resp, zipBytes := fetchHTTP(t, nil, req)
	if resp.StatusCode != 200 {
		t.Fatalf("download zip failed with %d", resp.StatusCode)
	}

	// Inspect ZIP entries
	reader, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("open zip reader: %v", err)
	}

	if len(reader.File) != len(contents) {
		t.Fatalf("expected %d files in zip, got %d", len(contents), len(reader.File))
	}

	for _, zf := range reader.File {
		expected, ok := contents[zf.Name]
		if !ok {
			t.Fatalf("unexpected file %s in zip", zf.Name)
		}
		rc, err := zf.Open()
		if err != nil {
			t.Fatalf("open zip entry %s: %v", zf.Name, err)
		}
		extracted, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || string(extracted) != expected {
			t.Fatalf("corrupted content for %s in zip: got %q", zf.Name, string(extracted))
		}
	}
}

// 3. Workload: Daemon restart and immutable snapshot recovery after input removal
func TestTier4_Workload_DaemonRestartSnapshotPersistence(t *testing.T) {
	d := startTestDaemon(t)
	inputDir := t.TempDir()

	docFile := writeTempFile(t, inputDir, "index.html", "<h1>Persistent Snapshot</h1>")
	res := runCLI(t, nil, "publish", docFile, "--server", d.URL, "--json")
	if res.ExitCode != 0 {
		t.Fatalf("publish failed: %s", res.Stderr)
	}

	var receipt struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	_ = json.Unmarshal([]byte(res.Stdout), &receipt)

	// Completely delete the original input directory
	_ = os.RemoveAll(inputDir)

	// Kill existing daemon
	_ = d.Cmd.Process.Kill()
	_ = d.Cmd.Wait()

	// Start a brand new daemon instance on a new port pointing to the existing data directory
	restarted := startTestDaemon(t, "--data", d.DataDir)

	// Fetch publication using the new daemon URL
	newArtifactURL := fmt.Sprintf("%s/%s/index.html", restarted.URL, receipt.ID)
	req, _ := http.NewRequest("GET", newArtifactURL, nil)
	resp, body := fetchHTTP(t, nil, req)
	if resp.StatusCode != 200 || string(body) != "<h1>Persistent Snapshot</h1>" {
		t.Fatalf("restarted daemon failed to serve snapshot: %d, body=%q", resp.StatusCode, string(body))
	}
}

// 4. Workload: Concurrent client publication isolation
func TestTier4_Workload_ConcurrentPublications(t *testing.T) {
	d := startTestDaemon(t)

	const numWorkers = 8
	var wg sync.WaitGroup
	ids := make(chan string, numWorkers)
	errs := make(chan error, numWorkers)

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			tmpDir := t.TempDir()
			fileName := fmt.Sprintf("worker_%d.txt", workerID)
			content := fmt.Sprintf("payload from worker %d", workerID)
			filePath := writeTempFile(t, tmpDir, fileName, content)

			res := runCLI(t, nil, "publish", filePath, "--server", d.URL, "--json")
			if res.ExitCode != 0 {
				errs <- fmt.Errorf("worker %d failed: %s", workerID, res.Stderr)
				return
			}
			var receipt struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(res.Stdout), &receipt); err != nil {
				errs <- fmt.Errorf("worker %d decode error: %v", workerID, err)
				return
			}
			ids <- receipt.ID
		}(i)
	}

	wg.Wait()
	close(ids)
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}

	seen := make(map[string]bool)
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate publication ID generated: %s", id)
		}
		seen[id] = true
	}

	if len(seen) != numWorkers {
		t.Fatalf("expected %d unique publication IDs, got %d", numWorkers, len(seen))
	}
}
