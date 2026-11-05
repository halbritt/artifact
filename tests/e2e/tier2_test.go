//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tier 2: Boundary & Corner Cases

// 1. Boundary: Empty path rejection variants and whitespace handling
func TestTier2_Boundary_EmptyPath(t *testing.T) {
	d := startTestDaemon(t)
	tmpDir := t.TempDir()

	// Whitespace filename (not empty, e.g. "   .txt") is a valid name if filesystem supports it
	spacedFile := writeTempFile(t, tmpDir, "   .txt", "content")
	resSpaced := runCLI(t, nil, "publish", spacedFile, "--server", d.URL)
	if resSpaced.ExitCode != 0 {
		t.Fatalf("whitespace filename rejected unexpectedly: %s", resSpaced.Stderr)
	}

	// Double quotes empty string argument
	resEmpty := runCLI(t, nil, "publish", "", "--server", d.URL)
	if resEmpty.ExitCode == 0 {
		t.Fatal("empty string argument succeeded unexpectedly")
	}

	// Multiple empty arguments
	resMultiEmpty := runCLI(t, nil, "publish", "", "", "--server", d.URL)
	if resMultiEmpty.ExitCode == 0 {
		t.Fatal("multiple empty string arguments succeeded unexpectedly")
	}
}

// 2. Boundary: UTF-8 sequences (valid complex multi-byte vs invalid byte patterns)
func TestTier2_Boundary_UTF8Sequences(t *testing.T) {
	d := startTestDaemon(t)

	// Valid Unicode: Japanese (3-byte), Emoji (4-byte), Cyrillic (2-byte)
	validNames := []string{
		"ドキュメント.txt",
		"report_📊_2026.html",
		"документ.csv",
	}

	for _, name := range validNames {
		parts := []MultipartFilePart{
			{
				FileName:   name,
				PathHeader: name,
				Data:       []byte("unicode data for " + name),
			},
		}
		body, ct := buildCustomMultipart(t, nil, parts)
		req, _ := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(body))
		req.Header.Set("Content-Type", ct)
		resp, respBody := fetchHTTP(t, nil, req)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("valid unicode %q rejected: %d %s", name, resp.StatusCode, string(respBody))
		}
	}

	// Invalid UTF-8 sequences: broken continuation byte, overlong NUL (\xc0\x80)
	invalidSequences := [][]byte{
		[]byte("bad_\xff_byte.txt"),
		[]byte("overlong_\xc0\x80.txt"),
		[]byte("truncated_\xe2\x82.txt"),
	}

	for _, raw := range invalidSequences {
		parts := []MultipartFilePart{
			{
				FileName:   string(raw),
				PathHeader: string(raw),
				Data:       []byte("invalid utf8 data"),
			},
		}
		body, ct := buildCustomMultipart(t, nil, parts)
		req, _ := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(body))
		req.Header.Set("Content-Type", ct)
		resp, _ := fetchHTTP(t, nil, req)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected HTTP 400 for invalid UTF-8 sequence %q, got %d", string(raw), resp.StatusCode)
		}
	}
}

// 3. Boundary: Whitespace preservation across nested paths
func TestTier2_Boundary_WhitespaceFilenames(t *testing.T) {
	d := startTestDaemon(t)

	complexSpacedPath := "  folder with spaces  /  file name with spaces  .txt"
	parts := []MultipartFilePart{
		{
			FileName:   "file.txt",
			PathHeader: complexSpacedPath,
			Data:       []byte("content with spaces in path"),
		},
	}
	body, ct := buildCustomMultipart(t, nil, parts)
	req, _ := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	resp, respBody := fetchHTTP(t, nil, req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for complex spaced path, got %d: %s", resp.StatusCode, string(respBody))
	}

	var receipt struct {
		Files []struct {
			Path string `json:"path"`
			URL  string `json:"url"`
		} `json:"files"`
	}
	_ = json.Unmarshal(respBody, &receipt)
	if len(receipt.Files) == 0 || receipt.Files[0].Path != complexSpacedPath {
		t.Fatalf("expected path %q, got %+v", complexSpacedPath, receipt.Files)
	}

	// Verify exact file download
	fReq, _ := http.NewRequest("GET", receipt.Files[0].URL, nil)
	fResp, fData := fetchHTTP(t, nil, fReq)
	if fResp.StatusCode != 200 || string(fData) != "content with spaces in path" {
		t.Fatalf("file content check failed: %d, body=%q", fResp.StatusCode, string(fData))
	}
}

// 4. Boundary: Origin URL delimiter variations
func TestTier2_Boundary_OriginDelimiters(t *testing.T) {
	d := startTestDaemon(t)
	tmpDir := t.TempDir()
	sample := writeTempFile(t, tmpDir, "a.txt", "a")

	badOrigins := []string{
		d.URL + "?",
		d.URL + "/?",
		d.URL + "#",
		d.URL + "/#",
		d.URL + "?query=1",
		d.URL + "/#section",
		d.URL + "/subpath",
	}

	for _, origin := range badOrigins {
		res := runCLI(t, nil, "publish", sample, "--server", origin)
		if res.ExitCode == 0 {
			t.Fatalf("expected origin %q to be rejected, got exit 0", origin)
		}
	}

	validOrigins := []string{
		d.URL,
		d.URL + "/",
	}

	for _, origin := range validOrigins {
		res := runCLI(t, nil, "publish", sample, "--server", origin)
		if res.ExitCode != 0 {
			t.Fatalf("expected origin %q to succeed, got %d: %s", origin, res.ExitCode, res.Stderr)
		}
	}
}

// 5. Boundary: Options payload limits (HTTP 413 vs HTTP 400)
func TestTier2_Boundary_OptionsPayloadLimit(t *testing.T) {
	d := startTestDaemon(t)

	// Malformed JSON options under upload limit -> 400 Bad Request
	badJSON := `{invalid json`
	parts := []MultipartFilePart{{FileName: "f.txt", Data: []byte("hi")}}
	body, ct := buildCustomMultipart(t, &badJSON, parts)
	req, _ := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	resp, _ := fetchHTTP(t, nil, req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for malformed options JSON, got %d", resp.StatusCode)
	}

	// Oversized options crossing 90 MiB -> 413 Request Entity Too Large
	oversized := fmt.Sprintf(`{"title": %q}`, strings.Repeat("B", 92<<20))
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("options", oversized)
	_ = w.Close()

	req413, _ := http.NewRequest("POST", d.URL+"/api/artifacts", &buf)
	req413.Header.Set("Content-Type", w.FormDataContentType())
	resp413, _ := fetchHTTP(t, nil, req413)
	if resp413.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected HTTP 413 for options crossing body limit, got %d", resp413.StatusCode)
	}
}

// 6. Boundary: Path component length limits (255 valid vs 256 unrepresentable)
func TestTier2_Boundary_PathComponentLimits(t *testing.T) {
	d := startTestDaemon(t)

	// 255-byte component: standard Linux filesystem limit (valid)
	name255 := strings.Repeat("k", 251) + ".txt" // 255 bytes total
	parts255 := []MultipartFilePart{{FileName: name255, PathHeader: name255, Data: []byte("valid len")}}
	body255, ct255 := buildCustomMultipart(t, nil, parts255)
	req255, _ := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(body255))
	req255.Header.Set("Content-Type", ct255)
	resp255, _ := fetchHTTP(t, nil, req255)
	if resp255.StatusCode != http.StatusCreated {
		t.Fatalf("expected HTTP 201 for 255-byte component, got %d", resp255.StatusCode)
	}

	// 256-byte component: unrepresentable on ext4/tmpfs (must return HTTP 400, not 500)
	name256 := strings.Repeat("x", 252) + ".txt" // 256 bytes total
	parts256 := []MultipartFilePart{{FileName: name256, PathHeader: name256, Data: []byte("too long")}}
	body256, ct256 := buildCustomMultipart(t, nil, parts256)
	req256, _ := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(body256))
	req256.Header.Set("Content-Type", ct256)
	resp256, _ := fetchHTTP(t, nil, req256)
	if resp256.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for 256-byte component, got %d", resp256.StatusCode)
	}
}

// 7. Boundary: Range request header variations
func TestTier2_Boundary_RangeHeaders(t *testing.T) {
	d := startTestDaemon(t)
	tmpDir := t.TempDir()
	sample := writeTempFile(t, tmpDir, "sample.txt", "0123456789")

	res := runCLI(t, nil, "publish", sample, "--server", d.URL, "--json")
	if res.ExitCode != 0 {
		t.Fatalf("publish failed: %s", res.Stderr)
	}

	var receipt struct {
		Files []struct {
			URL string `json:"url"`
		} `json:"files"`
	}
	_ = json.Unmarshal([]byte(res.Stdout), &receipt)
	fileURL := receipt.Files[0].URL

	// Verify security headers on 416
	req416, _ := http.NewRequest("GET", fileURL, nil)
	req416.Header.Set("Range", "bytes=5000-6000")
	resp416, _ := fetchHTTP(t, nil, req416)
	if resp416.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("expected 416, got %d", resp416.StatusCode)
	}
	if resp416.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("416 missing Cache-Control: no-store, got %q", resp416.Header.Get("Cache-Control"))
	}
	if resp416.Header.Get("X-Robots-Tag") != "noindex, nofollow, noarchive" {
		t.Fatalf("416 missing X-Robots-Tag, got %q", resp416.Header.Get("X-Robots-Tag"))
	}
}

// 8. Boundary: Duplicate and manipulated receipt inventory validation
func TestTier2_Boundary_ReceiptDuplicates(t *testing.T) {
	d := startTestDaemon(t)
	tmpDir := t.TempDir()
	f1 := writeTempFile(t, tmpDir, "f1.txt", "content1")
	f2 := writeTempFile(t, tmpDir, "f2.txt", "content2")

	// Regular publication succeeds
	res := runCLI(t, nil, "publish", f1, f2, "--server", d.URL, "--json")
	if res.ExitCode != 0 {
		t.Fatalf("regular publish failed: %s", res.Stderr)
	}

	var receipt struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &receipt); err != nil {
		t.Fatalf("receipt unmarshal: %v", err)
	}
	if len(receipt.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(receipt.Files))
	}
	if receipt.Files[0].Path == receipt.Files[1].Path {
		t.Fatalf("duplicate paths detected in receipt files: %v", receipt.Files)
	}
}

// 9. Boundary: Clean staging on client disconnect during streaming
func TestTier2_Boundary_ClientDisconnectCleanup(t *testing.T) {
	d := startTestDaemon(t)

	// Stream truncated body to cause unexpected EOF / disconnect
	body, ct := buildCustomMultipart(t, nil, []MultipartFilePart{
		{FileName: "abort.bin", PathHeader: "abort.bin", Data: []byte("partial data before abort")},
	})
	// Truncate the multipart body
	truncated := body[:len(body)-10]
	req, _ := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(truncated))
	req.Header.Set("Content-Type", ct)
	resp, _ := fetchHTTP(t, nil, req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for truncated upload, got %d", resp.StatusCode)
	}

	// Verify staging is immediately clean
	entries, _ := os.ReadDir(filepath.Join(d.DataDir, "staging"))
	if len(entries) != 0 {
		t.Fatalf("leftover staging directory after client disconnect: %d found", len(entries))
	}
}
