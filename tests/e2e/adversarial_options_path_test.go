//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/halbritt/artifact/internal/artifact"
)

type repeatReader struct {
	ch  byte
	rem int64
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.rem <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.rem {
		n = r.rem
	}
	for i := int64(0); i < n; i++ {
		p[i] = r.ch
	}
	r.rem -= n
	return int(n), nil
}

func assertCleanStaging(t *testing.T, dataDir string) {
	t.Helper()
	stagingDir := filepath.Join(dataDir, "staging")
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("failed to read staging dir %s: %v", stagingDir, err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("staging directory not clean, found %d residue entries: %v", len(entries), names)
	}
}

// 1. Single options part exceeding 90 MiB -> HTTP 413 and clean staging
func TestAdversarial_Options_SinglePartExceeding90MiB_HTTP413(t *testing.T) {
	d := startTestDaemon(t)
	pr, pw := io.Pipe()
	w := multipart.NewWriter(pw)

	go func() {
		part, err := w.CreateFormField("options")
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		// Stream 95 MiB of options
		_, err = io.Copy(part, &repeatReader{ch: 'A', rem: 95 << 20})
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		_ = w.Close()
		_ = pw.Close()
	}()

	req, err := http.NewRequest("POST", d.URL+"/api/artifacts", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, respBody := fetchHTTP(t, nil, req)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected HTTP 413, got %d: %s", resp.StatusCode, string(respBody))
	}
	assertCleanStaging(t, d.DataDir)
}

// 2. Cumulative stream crossing 90 MiB during options part -> HTTP 413 and clean staging
func TestAdversarial_Options_CumulativeCrossingDuringOptions_HTTP413(t *testing.T) {
	d := startTestDaemon(t)
	pr, pw := io.Pipe()
	w := multipart.NewWriter(pw)

	go func() {
		// Part 1: File of 80 MiB
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="file"; filename="large.bin"`)
		h.Set("Content-Type", "application/octet-stream")
		h.Set("X-Artifact-Path", "large.bin")
		h.Set("X-Artifact-Path-Escaped", "large.bin")
		filePart, err := w.CreatePart(h)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(filePart, &repeatReader{ch: 'X', rem: 80 << 20}); err != nil {
			pw.CloseWithError(err)
			return
		}

		// Part 2: Options of 15 MiB (cumulative 95 MiB, crosses 90 MiB limit during options)
		optsPart, err := w.CreateFormField("options")
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(optsPart, &repeatReader{ch: 'B', rem: 15 << 20}); err != nil {
			pw.CloseWithError(err)
			return
		}

		_ = w.Close()
		_ = pw.Close()
	}()

	req, err := http.NewRequest("POST", d.URL+"/api/artifacts", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, respBody := fetchHTTP(t, nil, req)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected HTTP 413 for cumulative limit crossing in options, got %d: %s", resp.StatusCode, string(respBody))
	}
	assertCleanStaging(t, d.DataDir)
}

// 3. Cumulative stream crossing 90 MiB during first 8192 bytes of options -> HTTP 413 and clean staging
func TestAdversarial_Options_CumulativeCrossingDuringLimitReader_HTTP413(t *testing.T) {
	d := startTestDaemon(t)
	pr, pw := io.Pipe()
	w := multipart.NewWriter(pw)

	go func() {
		// Part 1: File of (90 MiB - 2048 bytes)
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="file"; filename="file.bin"`)
		h.Set("Content-Type", "application/octet-stream")
		h.Set("X-Artifact-Path", "file.bin")
		h.Set("X-Artifact-Path-Escaped", "file.bin")
		filePart, err := w.CreatePart(h)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		fileSize := (90 << 20) - 2048
		if _, err := io.Copy(filePart, &repeatReader{ch: 'Z', rem: int64(fileSize)}); err != nil {
			pw.CloseWithError(err)
			return
		}

		// Part 2: Options of 4096 bytes (crosses 90 MiB within the first 8192 bytes read)
		optsPart, err := w.CreateFormField("options")
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(optsPart, &repeatReader{ch: 'C', rem: 4096}); err != nil {
			pw.CloseWithError(err)
			return
		}

		_ = w.Close()
		_ = pw.Close()
	}()

	req, err := http.NewRequest("POST", d.URL+"/api/artifacts", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, respBody := fetchHTTP(t, nil, req)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected HTTP 413, got %d: %s", resp.StatusCode, string(respBody))
	}
	assertCleanStaging(t, d.DataDir)
}

// 4. Options exceeding 8192 bytes but under 90 MiB -> HTTP 400 Bad Request and clean staging
func TestAdversarial_Options_Exceeds8192Under90MiB_HTTP400(t *testing.T) {
	d := startTestDaemon(t)

	sizes := []int{
		8193,    // exactly 1 byte over 8192 limit
		8500,    // boundary
		10000,   // 10 KB
		65536,   // 64 KB
		1 << 20, // 1 MiB
	}

	for _, size := range sizes {
		t.Run(fmt.Sprintf("Size_%d", size), func(t *testing.T) {
			var buf bytes.Buffer
			w := multipart.NewWriter(&buf)
			// Create options exceeding 8192 bytes
			optsData := strings.Repeat(" ", size-20) + `{"title": "padded"}`
			if len(optsData) < size {
				optsData += strings.Repeat(" ", size-len(optsData))
			}
			if err := w.WriteField("options", optsData); err != nil {
				t.Fatal(err)
			}
			h := make(textproto.MIMEHeader)
			h.Set("Content-Disposition", `form-data; name="file"; filename="sample.txt"`)
			h.Set("Content-Type", "text/plain")
			h.Set("X-Artifact-Path", "sample.txt")
			h.Set("X-Artifact-Path-Escaped", "sample.txt")
			part, _ := w.CreatePart(h)
			_, _ = part.Write([]byte("hello"))
			_ = w.Close()

			req, _ := http.NewRequest("POST", d.URL+"/api/artifacts", &buf)
			req.Header.Set("Content-Type", w.FormDataContentType())
			resp, respBody := fetchHTTP(t, nil, req)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected HTTP 400 for options of size %d, got %d: %s", size, resp.StatusCode, string(respBody))
			}
			assertCleanStaging(t, d.DataDir)
		})
	}
}

// 5. Options exactly 8192 bytes (valid JSON with padding) -> HTTP 201 Created
func TestAdversarial_Options_Exact8192Bytes_HTTP201(t *testing.T) {
	d := startTestDaemon(t)
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	baseJSON := `{"title": "Exact8192"}`
	padding := strings.Repeat(" ", 8192-len(baseJSON))
	opts8192 := baseJSON + padding
	if len(opts8192) != 8192 {
		t.Fatalf("opts length must be 8192, got %d", len(opts8192))
	}
	if err := w.WriteField("options", opts8192); err != nil {
		t.Fatal(err)
	}

	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="file.txt"`)
	h.Set("Content-Type", "text/plain")
	h.Set("X-Artifact-Path", "file.txt")
	h.Set("X-Artifact-Path-Escaped", "file.txt")
	part, _ := w.CreatePart(h)
	_, _ = part.Write([]byte("valid content"))
	_ = w.Close()

	req, _ := http.NewRequest("POST", d.URL+"/api/artifacts", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, respBody := fetchHTTP(t, nil, req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected HTTP 201 for exactly 8192 byte options, got %d: %s", resp.StatusCode, string(respBody))
	}

	var receipt struct {
		Title string `json:"title"`
	}
	_ = json.Unmarshal(respBody, &receipt)
	if receipt.Title != "Exact8192" {
		t.Fatalf("expected title Exact8192, got %q", receipt.Title)
	}
	assertCleanStaging(t, d.DataDir)
}

// 6. Malformed JSON options under 8192 bytes -> HTTP 400 Bad Request and clean staging
func TestAdversarial_Options_MalformedUnder8192_HTTP400(t *testing.T) {
	d := startTestDaemon(t)

	cases := []struct {
		name    string
		payload string
	}{
		{"TruncatedJSON", `{"title": "unclosed`},
		{"BareString", `just a bare string`},
		{"JSONArray", `["title", "invalid"]`},
		{"WrongTypes", `{"title": 123456}`},
		{"TitleTooLong", fmt.Sprintf(`{"title": "%s"}`, strings.Repeat("T", 501))},
		{"TrailingGarbage", `{"title": "ok"} trailing garbage`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := multipart.NewWriter(&buf)
			_ = w.WriteField("options", tc.payload)

			h := make(textproto.MIMEHeader)
			h.Set("Content-Disposition", `form-data; name="file"; filename="test.txt"`)
			h.Set("Content-Type", "text/plain")
			h.Set("X-Artifact-Path", "test.txt")
			h.Set("X-Artifact-Path-Escaped", "test.txt")
			part, _ := w.CreatePart(h)
			_, _ = part.Write([]byte("content"))
			_ = w.Close()

			req, _ := http.NewRequest("POST", d.URL+"/api/artifacts", &buf)
			req.Header.Set("Content-Type", w.FormDataContentType())
			resp, respBody := fetchHTTP(t, nil, req)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("[%s] expected HTTP 400, got %d: %s", tc.name, resp.StatusCode, string(respBody))
			}
			assertCleanStaging(t, d.DataDir)
		})
	}
}

// 7. Duplicate options parts -> HTTP 400 Bad Request and clean staging
func TestAdversarial_Options_DuplicateParts_HTTP400(t *testing.T) {
	d := startTestDaemon(t)
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("options", `{"title": "First"}`)
	_ = w.WriteField("options", `{"title": "Second"}`)

	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="test.txt"`)
	h.Set("Content-Type", "text/plain")
	h.Set("X-Artifact-Path", "test.txt")
	h.Set("X-Artifact-Path-Escaped", "test.txt")
	part, _ := w.CreatePart(h)
	_, _ = part.Write([]byte("content"))
	_ = w.Close()

	req, _ := http.NewRequest("POST", d.URL+"/api/artifacts", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, respBody := fetchHTTP(t, nil, req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for duplicate options, got %d: %s", resp.StatusCode, string(respBody))
	}
	assertCleanStaging(t, d.DataDir)
}

// 8. ARTIFACT-8: Path component limits (255 pass vs 256 fail vs 300 fail)
func TestAdversarial_Path_ComponentLengths_255_256_300(t *testing.T) {
	d := startTestDaemon(t)

	// A: Exactly 255 bytes (251 chars + ".txt") -> HTTP 201 Created
	name255 := strings.Repeat("p", 251) + ".txt"
	if len(name255) != 255 {
		t.Fatalf("expected length 255, got %d", len(name255))
	}
	content255 := []byte("payload for 255 component")
	parts255 := []MultipartFilePart{
		{
			FileName:   name255,
			PathHeader: name255,
			Data:       content255,
		},
	}
	body255, ct255 := buildCustomMultipart(t, nil, parts255)
	req255, _ := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(body255))
	req255.Header.Set("Content-Type", ct255)
	resp255, respBody255 := fetchHTTP(t, nil, req255)
	if resp255.StatusCode != http.StatusCreated {
		t.Fatalf("expected HTTP 201 for 255-byte component, got %d: %s", resp255.StatusCode, string(respBody255))
	}
	var receipt255 struct {
		Files []struct {
			Path string `json:"path"`
			URL  string `json:"url"`
		} `json:"files"`
	}
	_ = json.Unmarshal(respBody255, &receipt255)
	if len(receipt255.Files) != 1 || receipt255.Files[0].Path != name255 {
		t.Fatalf("receipt mismatch for 255-byte component: %+v", receipt255.Files)
	}

	// Verify download works
	getReq255, _ := http.NewRequest("GET", receipt255.Files[0].URL, nil)
	getResp255, getData255 := fetchHTTP(t, nil, getReq255)
	if getResp255.StatusCode != http.StatusOK || !bytes.Equal(getData255, content255) {
		t.Fatalf("download failed for 255-byte file: %d body=%q", getResp255.StatusCode, string(getData255))
	}
	assertCleanStaging(t, d.DataDir)

	// B: Exactly 256 bytes (252 chars + ".txt") -> HTTP 400 Bad Request
	name256 := strings.Repeat("q", 252) + ".txt"
	if len(name256) != 256 {
		t.Fatalf("expected length 256, got %d", len(name256))
	}
	parts256 := []MultipartFilePart{
		{
			FileName:   name256,
			PathHeader: name256,
			Data:       []byte("too long"),
		},
	}
	body256, ct256 := buildCustomMultipart(t, nil, parts256)
	req256, _ := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(body256))
	req256.Header.Set("Content-Type", ct256)
	resp256, respBody256 := fetchHTTP(t, nil, req256)
	if resp256.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for 256-byte component, got %d: %s", resp256.StatusCode, string(respBody256))
	}
	assertCleanStaging(t, d.DataDir)

	// C: 300 bytes -> HTTP 400 Bad Request
	name300 := strings.Repeat("r", 296) + ".txt"
	parts300 := []MultipartFilePart{
		{
			FileName:   name300,
			PathHeader: name300,
			Data:       []byte("300 bytes component"),
		},
	}
	body300, ct300 := buildCustomMultipart(t, nil, parts300)
	req300, _ := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(body300))
	req300.Header.Set("Content-Type", ct300)
	resp300, respBody300 := fetchHTTP(t, nil, req300)
	if resp300.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for 300-byte component, got %d: %s", resp300.StatusCode, string(respBody300))
	}
	assertCleanStaging(t, d.DataDir)

	// D: Directory component with 256 bytes -> HTTP 400 Bad Request
	dir256Path := strings.Repeat("d", 256) + "/file.txt"
	partsDir256 := []MultipartFilePart{
		{
			FileName:   "file.txt",
			PathHeader: dir256Path,
			Data:       []byte("nested inside 256 byte dir"),
		},
	}
	bodyDir256, ctDir256 := buildCustomMultipart(t, nil, partsDir256)
	reqDir256, _ := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(bodyDir256))
	reqDir256.Header.Set("Content-Type", ctDir256)
	respDir256, respBodyDir256 := fetchHTTP(t, nil, reqDir256)
	if respDir256.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for 256-byte directory component, got %d: %s", respDir256.StatusCode, string(respBodyDir256))
	}
	assertCleanStaging(t, d.DataDir)
}

// 9. ARTIFACT-8: Nested directories where each component is 255 bytes (total path > 1000 bytes) -> HTTP 201 Created
func TestAdversarial_Path_Nested255Components_Over1000Bytes(t *testing.T) {
	d := startTestDaemon(t)

	// 4 components, each exactly 255 bytes
	c1 := strings.Repeat("a", 255)
	c2 := strings.Repeat("b", 255)
	c3 := strings.Repeat("c", 255)
	c4 := strings.Repeat("d", 251) + ".txt" // 255 bytes
	nestedPath := c1 + "/" + c2 + "/" + c3 + "/" + c4
	totalLen := len(nestedPath)
	if totalLen != 1023 {
		t.Fatalf("expected total length 1023, got %d", totalLen)
	}

	content := []byte("content at 1023 bytes path")
	parts := []MultipartFilePart{
		{
			FileName:   "nested.txt",
			PathHeader: nestedPath,
			Data:       content,
		},
	}
	body, ct := buildCustomMultipart(t, nil, parts)
	req, _ := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	resp, respBody := fetchHTTP(t, nil, req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected HTTP 201 for nested 255-byte components (len %d), got %d: %s", totalLen, resp.StatusCode, string(respBody))
	}

	var receipt struct {
		Files []struct {
			Path string `json:"path"`
			URL  string `json:"url"`
		} `json:"files"`
		DownloadURL string `json:"download_url"`
	}
	_ = json.Unmarshal(respBody, &receipt)
	if len(receipt.Files) != 1 || receipt.Files[0].Path != nestedPath {
		t.Fatalf("receipt path mismatch: got %+v", receipt.Files)
	}

	// Verify download
	getReq, _ := http.NewRequest("GET", receipt.Files[0].URL, nil)
	getResp, getData := fetchHTTP(t, nil, getReq)
	if getResp.StatusCode != http.StatusOK || !bytes.Equal(getData, content) {
		t.Fatalf("download failed for 1023-byte path: %d body=%q", getResp.StatusCode, string(getData))
	}
	assertCleanStaging(t, d.DataDir)
}

// 10. ARTIFACT-8: Deeply nested directory exceeding Linux PATH_MAX (>4096 bytes) -> HTTP 400 Bad Request
func TestAdversarial_Path_TotalExceedingPathMax4096_HTTP400(t *testing.T) {
	d := startTestDaemon(t)

	// 25 components of 200 bytes each -> 5000+ bytes total
	comps := make([]string, 25)
	for i := range comps {
		comps[i] = fmt.Sprintf("dir_%03d_%s", i, strings.Repeat("x", 190))
	}
	hugePath := strings.Join(comps, "/") + "/file.txt"
	if len(hugePath) < 4500 {
		t.Fatalf("expected path length > 4500, got %d", len(hugePath))
	}

	parts := []MultipartFilePart{
		{
			FileName:   "file.txt",
			PathHeader: hugePath,
			Data:       []byte("huge path data"),
		},
	}
	body, ct := buildCustomMultipart(t, nil, parts)
	req, _ := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	resp, respBody := fetchHTTP(t, nil, req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for path exceeding PATH_MAX (%d bytes), got %d: %s", len(hugePath), resp.StatusCode, string(respBody))
	}
	assertCleanStaging(t, d.DataDir)
}

// 11. Genuine server storage errors return HTTP 500 (NOT HTTP 400)
func TestAdversarial_Storage_GenuineServerError_ReturnsHTTP500(t *testing.T) {
	root := t.TempDir()
	store, err := artifact.NewStore(root, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}

	// Make staging directory read-only so MkdirTemp fails with permission denied
	stagingDir := filepath.Join(root, "staging")
	if err := os.Chmod(stagingDir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(stagingDir, 0755)
	})

	server := httptest.NewServer(store.Handler())
	defer server.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="test.txt"`)
	h.Set("Content-Type", "text/plain")
	h.Set("X-Artifact-Path", "test.txt")
	h.Set("X-Artifact-Path-Escaped", "test.txt")
	part, _ := w.CreatePart(h)
	_, _ = part.Write([]byte("some data"))
	_ = w.Close()

	req, err := http.NewRequest("POST", server.URL+"/api/artifacts", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 for server storage error, got %d: %s", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), "could not store artifact") {
		t.Fatalf("expected 500 response body to contain 'could not store artifact', got: %s", string(body))
	}
}

// 12. Escaped path malformations return HTTP 400 Bad Request
func TestAdversarial_Path_InvalidEscapeSequences_HTTP400(t *testing.T) {
	d := startTestDaemon(t)

	badEscapes := []string{
		"invalid%ZZpath.txt",
		"truncated%",
		"middle%2path.txt",
	}

	for _, esc := range badEscapes {
		t.Run(esc, func(t *testing.T) {
			var buf bytes.Buffer
			w := multipart.NewWriter(&buf)
			h := make(textproto.MIMEHeader)
			h.Set("Content-Disposition", `form-data; name="file"; filename="file.txt"`)
			h.Set("Content-Type", "text/plain")
			h.Set("X-Artifact-Path", "file.txt")
			h.Set("X-Artifact-Path-Escaped", esc)
			part, _ := w.CreatePart(h)
			_, _ = part.Write([]byte("test"))
			_ = w.Close()

			req, _ := http.NewRequest("POST", d.URL+"/api/artifacts", &buf)
			req.Header.Set("Content-Type", w.FormDataContentType())
			resp, respBody := fetchHTTP(t, nil, req)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected HTTP 400 for bad escape %q, got %d: %s", esc, resp.StatusCode, string(respBody))
			}
			assertCleanStaging(t, d.DataDir)
		})
	}
}
