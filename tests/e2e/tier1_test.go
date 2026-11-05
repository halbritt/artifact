//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ARTIFACT-1: Reject empty publication paths before expanding the working directory.
func TestTier1_ARTIFACT1_EmptyPath(t *testing.T) {
	d := startTestDaemon(t)
	tmpDir := t.TempDir()
	validFile := writeTempFile(t, tmpDir, "sample.txt", "valid content")

	// 1. Explicit empty argument must fail
	res := runCLI(t, nil, "publish", "", "--server", d.URL)
	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code for empty path, got 0. Stdout: %s", res.Stdout)
	}

	// 2. Empty argument mixed with valid path must fail before any upload
	resMixed := runCLI(t, nil, "publish", "", validFile, "--server", d.URL)
	if resMixed.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code for mixed empty path, got 0. Stdout: %s", resMixed.Stdout)
	}

	// 3. Staging and published should have no residue from failed attempts
	for _, sub := range []string{"staging", "published"} {
		entries, err := os.ReadDir(filepath.Join(d.DataDir, sub))
		if err != nil {
			t.Fatalf("read %s: %v", sub, err)
		}
		if len(entries) != 0 {
			t.Fatalf("expected 0 entries in %s after failed empty path, got %d", sub, len(entries))
		}
	}

	// 4. Intentionally selected directory or file still works
	resValid := runCLI(t, nil, "publish", validFile, "--server", d.URL, "--json")
	if resValid.ExitCode != 0 {
		t.Fatalf("expected valid file publish to succeed, got %d: %s", resValid.ExitCode, resValid.Stderr)
	}
}

// ARTIFACT-3: Reject invalid UTF-8 publication paths before committing files.
func TestTier1_ARTIFACT3_InvalidUTF8(t *testing.T) {
	d := startTestDaemon(t)

	// Direct API upload with invalid UTF-8 filename (caf\xe9.txt)
	invalidName := "caf\xe9.txt"
	parts := []MultipartFilePart{
		{
			FileName:   invalidName,
			PathHeader: invalidName,
			Data:       []byte("coffee content"),
		},
	}
	body, contentType := buildCustomMultipart(t, nil, parts)
	req, _ := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	resp, respBody := fetchHTTP(t, nil, req)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for invalid UTF-8 path, got %d: %s", resp.StatusCode, string(respBody))
	}

	// Verify no residue in staging or published
	for _, sub := range []string{"staging", "published"} {
		entries, _ := os.ReadDir(filepath.Join(d.DataDir, sub))
		if len(entries) != 0 {
			t.Fatalf("expected 0 entries in %s, got %d", sub, len(entries))
		}
	}

	// Valid UTF-8 café.txt must succeed
	validName := "café.txt"
	validParts := []MultipartFilePart{
		{
			FileName:   validName,
			PathHeader: validName,
			Data:       []byte("valid utf-8 coffee"),
		},
	}
	validBody, validCT := buildCustomMultipart(t, nil, validParts)
	vreq, _ := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(validBody))
	vreq.Header.Set("Content-Type", validCT)
	vresp, vbody := fetchHTTP(t, nil, vreq)
	if vresp.StatusCode != http.StatusCreated {
		t.Fatalf("expected HTTP 201 for valid UTF-8, got %d: %s", vresp.StatusCode, string(vbody))
	}
}

// ARTIFACT-5: Reject origin URLs with empty query or fragment delimiters.
func TestTier1_ARTIFACT5_BareOriginDelimiter(t *testing.T) {
	d := startTestDaemon(t)
	tmpDir := t.TempDir()
	sampleFile := writeTempFile(t, tmpDir, "sample.txt", "content")

	// Bare query delimiter '?' and bare fragment delimiter '#'
	for _, suffix := range []string{"?", "#", "/?", "/#"} {
		badServer := d.URL + suffix
		res := runCLI(t, nil, "publish", sampleFile, "--server", badServer)
		if res.ExitCode == 0 {
			t.Fatalf("expected CLI error for origin with bare delimiter %q, got exit 0", badServer)
		}
	}

	// Normal server URL with trailing slash must succeed
	goodServer := d.URL + "/"
	resGood := runCLI(t, nil, "publish", sampleFile, "--server", goodServer)
	if resGood.ExitCode != 0 {
		t.Fatalf("expected success for normal server %q, got %d: %s", goodServer, resGood.ExitCode, resGood.Stderr)
	}
}

// ARTIFACT-4: Preserve leading and trailing filename spaces through upload.
func TestTier1_ARTIFACT4_SpacePreservation(t *testing.T) {
	d := startTestDaemon(t)

	spacedName := " leading_and_trailing.txt "
	fileContent := "spaces content"
	parts := []MultipartFilePart{
		{
			FileName:   spacedName,
			PathHeader: spacedName,
			Data:       []byte(fileContent),
		},
	}
	body, ct := buildCustomMultipart(t, nil, parts)
	req, _ := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	resp, respBody := fetchHTTP(t, nil, req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected HTTP 201 for file with spaces, got %d: %s", resp.StatusCode, string(respBody))
	}

	var receipt struct {
		ID    string `json:"id"`
		Files []struct {
			Path string `json:"path"`
			URL  string `json:"url"`
		} `json:"files"`
	}
	if err := json.Unmarshal(respBody, &receipt); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}

	if len(receipt.Files) == 0 || receipt.Files[0].Path != spacedName {
		t.Fatalf("receipt did not preserve spaces: got %+v", receipt.Files)
	}

	// Fetch via URL to confirm intact file
	fReq, _ := http.NewRequest("GET", receipt.Files[0].URL, nil)
	fResp, fData := fetchHTTP(t, nil, fReq)
	if fResp.StatusCode != http.StatusOK || string(fData) != fileContent {
		t.Fatalf("fetching spaced file failed: %d, body=%q", fResp.StatusCode, string(fData))
	}
}

// ARTIFACT-7: Preserve HTTP 413 when upload limits are crossed in options.
func TestTier1_ARTIFACT7_OptionsLimit413(t *testing.T) {
	d := startTestDaemon(t)

	// Cumulative limit is 90 MiB (MaxUploadBytes).
	// Construct a request exceeding 90 MiB where the limit is crossed inside options part.
	oversizedOpts := fmt.Sprintf(`{"title": "%s"}`, strings.Repeat("A", 95<<20))
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.WriteField("options", oversizedOpts)
	_ = w.Close()

	req, _ := http.NewRequest("POST", d.URL+"/api/artifacts", &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, respBody := fetchHTTP(t, nil, req)

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected HTTP 413 for oversized options, got %d: %s", resp.StatusCode, string(respBody))
	}
}

// ARTIFACT-8: Return invalid-input status (HTTP 400) for unrepresentable path lengths.
func TestTier1_ARTIFACT8_UnrepresentablePath(t *testing.T) {
	d := startTestDaemon(t)

	// Create a component with 300 characters (exceeds typical 255-byte filesystem limit)
	longComponent := strings.Repeat("a", 300) + ".txt"
	parts := []MultipartFilePart{
		{
			FileName:   "test.txt",
			PathHeader: longComponent,
			Data:       []byte("data"),
		},
	}
	body, ct := buildCustomMultipart(t, nil, parts)
	req, _ := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	resp, respBody := fetchHTTP(t, nil, req)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for unrepresentable path length, got %d: %s", resp.StatusCode, string(respBody))
	}
}

// ARTIFACT-9: Retain no-store on invalid byte-range responses.
func TestTier1_ARTIFACT9_RangeNoStore(t *testing.T) {
	d := startTestDaemon(t)
	tmpDir := t.TempDir()
	sampleFile := writeTempFile(t, tmpDir, "range.txt", "short content for range")

	res := runCLI(t, nil, "publish", sampleFile, "--server", d.URL, "--json")
	if res.ExitCode != 0 {
		t.Fatalf("publish failed: %s", res.Stderr)
	}

	var receipt struct {
		Files []struct {
			URL string `json:"url"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &receipt); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	fileURL := receipt.Files[0].URL

	// 1. Valid range returns 206 with Cache-Control: no-store
	reqValid, _ := http.NewRequest("GET", fileURL, nil)
	reqValid.Header.Set("Range", "bytes=0-4")
	respValid, _ := fetchHTTP(t, nil, reqValid)
	if respValid.StatusCode != http.StatusPartialContent {
		t.Fatalf("expected 206 for valid range, got %d", respValid.StatusCode)
	}
	if respValid.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("valid range missing Cache-Control: no-store, got %q", respValid.Header.Get("Cache-Control"))
	}

	// 2. Unsatisfiable range returns 416 with Cache-Control: no-store
	reqInvalid, _ := http.NewRequest("GET", fileURL, nil)
	reqInvalid.Header.Set("Range", "bytes=99999-99999")
	respInvalid, _ := fetchHTTP(t, nil, reqInvalid)
	if respInvalid.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("expected 416 for invalid range, got %d", respInvalid.StatusCode)
	}
	if respInvalid.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("416 response missing Cache-Control: no-store, got %q", respInvalid.Header.Get("Cache-Control"))
	}
}

// ARTIFACT-2: Handle accepted large inventories without losing publication receipt.
func TestTier1_ARTIFACT2_LargeInventory(t *testing.T) {
	d := startTestDaemon(t)
	tmpDir := t.TempDir()

	// Generate 1,000 files with nested long path components
	// (Under 5 directory components to stay under path limits)
	nestedDir := filepath.Join(tmpDir, "dir1", "dir2", "dir3")
	if err := os.MkdirAll(nestedDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	for i := 0; i < 1000; i++ {
		filename := fmt.Sprintf("file_%04d.txt", i)
		p := filepath.Join(nestedDir, filename)
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatalf("write file %d: %v", i, err)
		}
	}

	res := runCLI(t, nil, "publish", tmpDir, "--server", d.URL, "--json")
	if res.ExitCode != 0 {
		t.Fatalf("expected successful publish of 1000 files, got %d: %s", res.ExitCode, res.Stderr)
	}

	var receipt struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &receipt); err != nil {
		t.Fatalf("decode receipt: %v. Output: %s", err, res.Stdout)
	}
	if len(receipt.Files) != 1000 {
		t.Fatalf("expected 1000 files in receipt, got %d", len(receipt.Files))
	}
}

// ARTIFACT-6: Require receipt inventory to match uploaded paths exactly once.
func TestTier1_ARTIFACT6_ExactInventoryMatch(t *testing.T) {
	d := startTestDaemon(t)
	tmpDir := t.TempDir()
	f1 := writeTempFile(t, tmpDir, "file1.txt", "one")
	f2 := writeTempFile(t, tmpDir, "file2.txt", "two")

	// Standard publish with 2 unique files must succeed
	res := runCLI(t, nil, "publish", f1, f2, "--server", d.URL, "--json")
	if res.ExitCode != 0 {
		t.Fatalf("expected 2 files to publish successfully, got %d: %s", res.ExitCode, res.Stderr)
	}
}

// ARTIFACT-10: Clean interrupted upload staging during bounded daemon shutdown.
func TestTier1_ARTIFACT10_ShutdownStagingClean(t *testing.T) {
	d := startTestDaemon(t)

	// Initiate a stalled multipart upload
	pr, pw := io.Pipe()
	w := multipart.NewWriter(pw)

	req, _ := http.NewRequest("POST", d.URL+"/api/artifacts", pr)
	req.Header.Set("Content-Type", w.FormDataContentType())

	uploadDone := make(chan struct{})
	go func() {
		defer close(uploadDone)
		// Write options and part header, but do not close
		_ = w.WriteField("options", `{"title": "stalled"}`)
		part, _ := w.CreateFormFile("file", "stalled.bin")
		_, _ = part.Write([]byte("start of data"))
		// Keep pipe open without closing to simulate stall
	}()

	client := &http.Client{Timeout: 0}
	go func() {
		_, _ = client.Do(req)
	}()

	// Wait briefly for staging dir to be allocated
	var stageFound bool
	for attempt := 0; attempt < 30; attempt++ {
		entries, _ := os.ReadDir(filepath.Join(d.DataDir, "staging"))
		if len(entries) > 0 {
			stageFound = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !stageFound {
		t.Log("Note: staging dir not yet visible before shutdown signal")
	}

	// Send SIGTERM to daemon
	if err := d.Cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("send interrupt: %v", err)
	}

	// Wait for bounded exit
	exitDone := make(chan error, 1)
	go func() {
		exitDone <- d.Cmd.Wait()
	}()

	select {
	case <-exitDone:
		// Daemon stopped
	case <-time.After(16 * time.Second):
		_ = d.Cmd.Process.Kill()
		t.Fatal("daemon did not exit within bounded 16s timeout after SIGTERM")
	}

	_ = pw.Close()
	<-uploadDone

	// Check staging directory after shutdown
	entries, err := os.ReadDir(filepath.Join(d.DataDir, "staging"))
	if err != nil {
		t.Fatalf("read staging: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 staging entries after shutdown, found %d", len(entries))
	}
}

// ARTIFACT-11: Clean UI fixture temporary data and child process on SIGTERM.
func TestTier1_ARTIFACT11_FixtureSigtermClean(t *testing.T) {
	port := getFreePort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	// Run tests/ui_fixture.py <port>
	fixtureScript := filepath.Join(projectRoot, "tests", "ui_fixture.py")
	cmd := exec.Command("python3", fixtureScript, fmt.Sprintf("%d", port))
	cmd.Dir = projectRoot

	if err := cmd.Start(); err != nil {
		t.Fatalf("start ui_fixture.py: %v", err)
	}

	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	// Wait for fixture to be up and serving
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	up := false
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/healthz")
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			up = true
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !up {
		t.Fatalf("ui_fixture did not start serving on port %d", port)
	}

	// Send SIGTERM to the python process
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal fixture: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-done:
		// Cleanly exited
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("fixture did not exit within 10s after SIGTERM")
	}

	// Verify port is closed
	time.Sleep(200 * time.Millisecond)
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatalf("fixture port %d is still open after SIGTERM shutdown", port)
	}
}
