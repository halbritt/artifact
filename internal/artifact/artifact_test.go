package artifact

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
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
	"sync"
	"testing"
)

func fixture(t *testing.T) (*Store, *httptest.Server) {
	t.Helper()
	store, err := NewStore(t.TempDir(), "http://example.test")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(store.Handler())
	t.Cleanup(server.Close)
	store.BaseURL = server.URL
	return store, server
}
func writeFile(t *testing.T, root, name, data string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}
func fetch(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, data
}

func TestPublishFolderRendersAssetsAndDownloadsExactBundle(t *testing.T) {
	store, server := fixture(t)
	dir := t.TempDir()
	contents := map[string]string{"index.html": "<!doctype html><script>window.artifactReady=true</script><img src='assets/plot.svg'>", "assets/plot.svg": "<svg xmlns='http://www.w3.org/2000/svg'/>", "data/results #1.csv": "x,y\n1,2\n"}
	for name, data := range contents {
		writeFile(t, dir, name, data)
	}
	files, err := Collect([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := PublishFiles(context.Background(), server.Client(), server.URL, files, Options{Title: "Review <one>"})
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.ID) != 6 || receipt.Entry != "index.html" {
		t.Fatalf("bad receipt: %+v", receipt)
	}
	for _, f := range receipt.Files {
		resp, data := fetch(t, f.URL)
		if resp.StatusCode != 200 || string(data) != contents[f.Path] {
			t.Fatalf("bad file %s: %d %s", f.Path, resp.StatusCode, data)
		}
		if f.Path == "index.html" && !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			t.Fatal("HTML not rendered")
		}
		sum := sha256.Sum256(data)
		if f.SHA256 != fmt.Sprintf("%x", sum) {
			t.Fatal("checksum mismatch")
		}
		resp, data = fetch(t, f.DownloadURL)
		if !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment;") || string(data) != contents[f.Path] {
			t.Fatal("bad attachment")
		}
	}
	resp, html := fetch(t, receipt.FilesURL)
	if resp.StatusCode != 200 || !bytes.Contains(html, []byte("Review &lt;one&gt;")) {
		t.Fatalf("bad listing: %s", html)
	}
	resp, data := fetch(t, receipt.DownloadURL)
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.File) != len(contents) {
		t.Fatal("archive file count")
	}
	for _, f := range archive.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil || string(data) != contents[f.Name] {
			t.Fatalf("bad archive file %s", f.Name)
		}
	}
	// A restarted server reads publications directly from disk; there is no in-memory catalog dependency.
	restarted, err := NewStore(store.Root, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := restarted.Load(receipt.ID)
	if err != nil || saved.URL != receipt.URL {
		t.Fatalf("restart lost artifact: %v", err)
	}
	// Original inputs can disappear without affecting publication.
	os.RemoveAll(dir)
	_, data = fetch(t, receipt.URL)
	if string(data) != contents["index.html"] {
		t.Fatal("not a snapshot")
	}
	req, _ := http.NewRequest("GET", receipt.Files[0].URL, nil)
	req.Header.Set("Range", "bytes=0-3")
	partial, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	partial.Body.Close()
	if partial.StatusCode != 206 {
		t.Fatal("range download unavailable")
	}
}

func multipartBody(t *testing.T, names []string, opts Options) ([]byte, string) {
	t.Helper()
	var b bytes.Buffer
	writer := multipart.NewWriter(&b)
	data, _ := json.Marshal(opts)
	if err := writer.WriteField("options", string(data)); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", `form-data; name="file"; filename="file"`)
		header.Set("X-Artifact-Path", name)
		p, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		p.Write([]byte("data"))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes(), writer.FormDataContentType()
}
func TestRejectedUploadsLeaveNoVisibleOrStagedPublication(t *testing.T) {
	cases := []struct {
		name     string
		paths    []string
		entry    string
		truncate bool
	}{
		{"traversal", []string{"../outside"}, "", false}, {"absolute", []string{"/outside"}, "", false}, {"reserved metadata", []string{"_artifact/manifest.json"}, "", false}, {"duplicate", []string{"a", "a"}, "", false}, {"file directory collision", []string{"a", "a/b"}, "", false}, {"missing entry", []string{"a"}, "missing.html", false}, {"empty", nil, "", false}, {"interrupted", []string{"a", "b"}, "", true},
		{"invalid utf8", []string{"caf\xe9.txt"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, server := fixture(t)
			body, contentType := multipartBody(t, tc.paths, Options{Entry: tc.entry})
			if tc.truncate {
				body = body[:len(body)-30]
			}
			resp, err := http.Post(server.URL+"/api/artifacts", contentType, bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 400 {
				t.Fatalf("got %d", resp.StatusCode)
			}
			for _, dir := range []string{"published", "staging"} {
				items, err := os.ReadDir(filepath.Join(store.Root, dir))
				if err != nil || len(items) != 0 {
					t.Fatalf("partial upload remains in %s: %v", dir, err)
				}
			}
		})
	}
}
func TestCollectRejectsSymlinksAndDuplicateNames(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "target", "data")
	if err := os.Symlink(filepath.Join(dir, "target"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := Collect([]string{dir}); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := Collect([]string{filepath.Join(dir, "target"), filepath.Join(dir, "target")}); err == nil {
		t.Fatal("duplicate accepted")
	}
}
func TestConcurrentAnonymousPublicationsRemainDistinct(t *testing.T) {
	_, server := fixture(t)
	body, kind := multipartBody(t, []string{"report.html"}, Options{})
	var wg sync.WaitGroup
	ids := make(chan string, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(server.URL+"/api/artifacts", kind, bytes.NewReader(body))
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			var r Receipt
			if resp.StatusCode != 201 {
				t.Errorf("status %d", resp.StatusCode)
				return
			}
			if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
				t.Error(err)
				return
			}
			ids <- r.ID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatal("ID reused")
		}
		seen[id] = true
	}
	if len(seen) != 12 {
		t.Fatal("lost publication")
	}
	_, root := fetch(t, server.URL+"/")
	for id := range seen {
		if strings.Contains(string(root), id) {
			t.Fatal("root exposes publications")
		}
	}
}
func TestDownloadOnlyPublicationGetsListingAndExplicitEntryWorks(t *testing.T) {
	_, server := fixture(t)
	dir := t.TempDir()
	writeFile(t, dir, "one.csv", "1")
	writeFile(t, dir, "nested/report.html", "<h1>Report</h1>")
	files, err := Collect([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"", "nested/report.html"} {
		receipt, err := PublishFiles(context.Background(), server.Client(), server.URL, files, Options{Entry: entry})
		if err != nil {
			t.Fatal(err)
		}
		resp, data := fetch(t, receipt.URL)
		if resp.StatusCode != 200 {
			t.Fatal(resp.Status)
		}
		if entry == "" && !bytes.Contains(data, []byte("one.csv")) {
			t.Fatal("missing listing")
		}
		if entry != "" && string(data) != "<h1>Report</h1>" {
			t.Fatal("wrong entry")
		}
	}
}

// An over-limit body must fail before publication, including when the client streams it.
func TestOversizedUploadIsRejectedAndCleaned(t *testing.T) {
	store, server := fixture(t)
	var prefix bytes.Buffer
	writer := multipart.NewWriter(&prefix)
	if _, err := writer.CreateFormFile("file", "large.bin"); err != nil {
		t.Fatal(err)
	}
	body := io.MultiReader(bytes.NewReader(prefix.Bytes()), io.LimitReader(zeroReader{}, MaxUploadBytes+1))
	req, err := http.NewRequest("POST", server.URL+"/api/artifacts", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatalf("oversized upload returned %d", resp.StatusCode)
	}
	for _, dir := range []string{"published", "staging"} {
		entries, err := os.ReadDir(filepath.Join(store.Root, dir))
		if err != nil || len(entries) != 0 {
			t.Fatalf("leftover upload in %s: %v", dir, err)
		}
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestCollectRejectsEmptyPath(t *testing.T) {
	if _, err := Collect([]string{""}); err == nil || err.Error() != "empty publication path" {
		t.Fatalf("expected 'empty publication path', got %v", err)
	}

	dir := t.TempDir()
	writeFile(t, dir, "hello.txt", "world")
	validFile := filepath.Join(dir, "hello.txt")
	if _, err := Collect([]string{validFile, ""}); err == nil || err.Error() != "empty publication path" {
		t.Fatalf("expected 'empty publication path' in mixed list, got %v", err)
	}

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origDir) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	files, err := Collect([]string{"."})
	if err != nil {
		t.Fatalf("failed to collect '.': %v", err)
	}
	if len(files) != 1 || files[0].Path != "hello.txt" {
		t.Fatalf("unexpected files for '.': %+v", files)
	}

	writeFile(t, dir, "path with spaces/file name.txt", "data")
	spaceFiles, err := Collect([]string{filepath.Join(dir, "path with spaces/file name.txt")})
	if err != nil {
		t.Fatalf("failed to collect file with spaces: %v", err)
	}
	if len(spaceFiles) != 1 || spaceFiles[0].Path != "file name.txt" {
		t.Fatalf("unexpected spaceFiles: %+v", spaceFiles)
	}
}

func TestValidPathUTF8Validation(t *testing.T) {
	valid := []string{
		"file.txt",
		"café.txt",
		"münchen.txt",
		"日本語.txt",
		"中文.txt",
		"🎉.txt",
		"sub/folder/café.txt",
		"dir/ünicode_123.bin",
		"hello world.txt",
	}
	for _, p := range valid {
		if !validPath(p) {
			t.Errorf("validPath(%q) = false; want true", p)
		}
	}

	invalid := []string{
		"caf\xe9.txt",
		"\xff\xfe",
		"\xed\xa0\x80",
		"\xc0\xaf",
		"data/\x80abc",
		"file\xfe.bin",
	}
	for _, p := range invalid {
		if validPath(p) {
			t.Errorf("validPath(%q) = true; want false", p)
		}
	}
}

func TestValidateBaseURL(t *testing.T) {
	valid := []string{
		"http://example.com",
		"http://example.com/",
		"https://artifact.harm.org",
		"https://artifact.harm.org/",
		"http://127.0.0.1:8879",
		"http://127.0.0.1:8879/",
		"http://[::1]:8879",
		"http://[::1]:8879/",
		"https://sub.domain.co.uk:443",
		"https://sub.domain.co.uk:443/",
	}
	for _, raw := range valid {
		if err := ValidateBaseURL(raw); err != nil {
			t.Errorf("ValidateBaseURL(%q) returned unexpected error: %v", raw, err)
		}
	}

	invalid := []string{
		"http://example.com?",
		"http://example.com/?",
		"http://example.com#",
		"http://example.com/#",
		"http://example.com?query=1",
		"http://example.com/?query=1",
		"http://example.com#fragment",
		"http://example.com/#fragment",
		"http://example.com?#",
		"http://example.com#?",
		"ftp://example.com",
		"javascript:alert(1)",
		"http://user:pass@example.com",
		"http://example.com/some/path",
		"",
		"not-a-url",
	}
	for _, raw := range invalid {
		if err := ValidateBaseURL(raw); err == nil {
			t.Errorf("ValidateBaseURL(%q) = nil; want error", raw)
		}
	}
}

func TestBaseURLBareDelimitersRejectedByStoreAndClient(t *testing.T) {
	badURLs := []string{"http://example.com?", "http://example.com#"}
	for _, u := range badURLs {
		if _, err := NewStore(t.TempDir(), u); err == nil {
			t.Errorf("NewStore accepted invalid base URL %q", u)
		}
		if _, err := PublishFiles(context.Background(), http.DefaultClient, u, nil, Options{}); err == nil {
			t.Errorf("PublishFiles accepted invalid base URL %q", u)
		}
	}
}

func TestCollectRejectsInvalidUTF8Path(t *testing.T) {
	dir := t.TempDir()
	badName := "caf\xe9.txt"
	badPath := filepath.Join(dir, badName)
	if err := os.WriteFile(badPath, []byte("test"), 0644); err != nil {
		t.Skipf("filesystem rejected writing invalid UTF-8 filename: %v", err)
	}
	_, err := Collect([]string{dir})
	if err == nil || !strings.Contains(err.Error(), "invalid publication path") {
		t.Fatalf("expected Collect to reject invalid UTF-8 path, got: %v", err)
	}
}

func TestPublishUnicodePathsRoundTrip(t *testing.T) {
	_, server := fixture(t)
	dir := t.TempDir()
	files := map[string]string{
		"café.html":        "<h1>Café</h1>",
		"日本語/ドキュメント.txt":   "こんにちは世界",
		"🎉/rocket 🚀.json":  `{"status":"ok"}`,
	}
	for name, data := range files {
		writeFile(t, dir, name, data)
	}

	collected, err := Collect([]string{dir})
	if err != nil {
		t.Fatalf("Collect failed for unicode files: %v", err)
	}
	if len(collected) != len(files) {
		t.Fatalf("expected %d collected files, got %d", len(files), len(collected))
	}

	receipt, err := PublishFiles(context.Background(), server.Client(), server.URL, collected, Options{Title: "Unicode Test"})
	if err != nil {
		t.Fatalf("PublishFiles failed for unicode paths: %v", err)
	}
	if len(receipt.Files) != len(files) {
		t.Fatalf("expected %d files in receipt, got %d", len(files), len(receipt.Files))
	}

	for _, f := range receipt.Files {
		if strings.Contains(f.Path, "\ufffd") {
			t.Fatalf("receipt file path contains replacement character: %q", f.Path)
		}
		expectedContent, ok := files[f.Path]
		if !ok {
			t.Fatalf("unexpected path in receipt: %q", f.Path)
		}
		resp, data := fetch(t, f.URL)
		if resp.StatusCode != 200 || string(data) != expectedContent {
			t.Fatalf("fetch %s failed: %d, data: %s", f.Path, resp.StatusCode, data)
		}
	}

	resp, data := fetch(t, receipt.DownloadURL)
	if resp.StatusCode != 200 {
		t.Fatalf("download zip failed: %d", resp.StatusCode)
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.File) != len(files) {
		t.Fatalf("archive file count: expected %d, got %d", len(files), len(archive.File))
	}
	for _, f := range archive.File {
		expectedContent, ok := files[f.Name]
		if !ok {
			t.Fatalf("unexpected archive entry: %q", f.Name)
		}
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(r)
		r.Close()
		if err != nil || string(content) != expectedContent {
			t.Fatalf("bad content in archive for %s", f.Name)
		}
	}
}

func TestEscapePath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple.txt", "simple.txt"},
		{"a/b/c.txt", "a/b/c.txt"},
		{" leading .txt ", "%20leading%20.txt%20"},
		{"  folder / sub / file  .txt ", "%20%20folder%20/%20sub%20/%20file%20%20.txt%20"},
		{"hello world/test#1.txt", "hello%20world/test%231.txt"},
	}
	for _, tt := range tests {
		if got := EscapePath(tt.input); got != tt.want {
			t.Errorf("EscapePath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestPublishPreservesLeadingAndTrailingSpaces(t *testing.T) {
	_, server := fixture(t)
	dir := t.TempDir()
	files := map[string]string{
		" leading_and_trailing.txt ":             "space content 1",
		"  folder with spaces  /  file  .txt  ": "space content 2",
	}
	for name, data := range files {
		writeFile(t, dir, name, data)
	}
	collected, err := Collect([]string{dir})
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	receipt, err := PublishFiles(context.Background(), server.Client(), server.URL, collected, Options{Title: "Spaces Test"})
	if err != nil {
		t.Fatalf("PublishFiles failed: %v", err)
	}
	if len(receipt.Files) != len(files) {
		t.Fatalf("expected %d files, got %d", len(files), len(receipt.Files))
	}
	for _, f := range receipt.Files {
		expectedContent, ok := files[f.Path]
		if !ok {
			t.Fatalf("unexpected path in receipt: %q", f.Path)
		}
		resp, data := fetch(t, f.URL)
		if resp.StatusCode != 200 || string(data) != expectedContent {
			t.Fatalf("fetch %q failed: %d, data: %s", f.Path, resp.StatusCode, data)
		}
	}
	resp, data := fetch(t, receipt.DownloadURL)
	if resp.StatusCode != 200 {
		t.Fatalf("download zip failed: %d", resp.StatusCode)
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.File) != len(files) {
		t.Fatalf("archive file count: expected %d, got %d", len(files), len(archive.File))
	}
	for _, f := range archive.File {
		expectedContent, ok := files[f.Name]
		if !ok {
			t.Fatalf("unexpected archive entry: %q", f.Name)
		}
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(r)
		r.Close()
		if err != nil || string(content) != expectedContent {
			t.Fatalf("bad content in archive for %s", f.Name)
		}
	}
}

func TestPublishOptionsUploadLimitPreservesHTTP413(t *testing.T) {
	_, server := fixture(t)
	var prefix bytes.Buffer
	writer := multipart.NewWriter(&prefix)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="options"`)
	if _, err := writer.CreatePart(h); err != nil {
		t.Fatal(err)
	}
	body := io.MultiReader(bytes.NewReader(prefix.Bytes()), io.LimitReader(zeroReader{}, MaxUploadBytes+1000))
	req, err := http.NewRequest("POST", server.URL+"/api/artifacts", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected HTTP 413 for oversized options, got %d", resp.StatusCode)
	}
}

func TestPublishOptionsMalformedUnderLimitReturns400(t *testing.T) {
	_, server := fixture(t)
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	largeBadOptions := strings.Repeat("x", 10000)
	if err := writer.WriteField("options", largeBadOptions); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", server.URL+"/api/artifacts", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for malformed options under limit, got %d", resp.StatusCode)
	}
}

func TestPublishUnrepresentablePathReturns400(t *testing.T) {
	_, server := fixture(t)
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	tooLong := strings.Repeat("a", 300) + ".txt"
	h.Set("Content-Disposition", `form-data; name="file"; filename="file.txt"`)
	h.Set("Content-Type", "application/octet-stream")
	h.Set("X-Artifact-Path-Escaped", EscapePath(tooLong))
	part, err := writer.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("content")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", server.URL+"/api/artifacts", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for unrepresentable path, got %d", resp.StatusCode)
	}
}

func TestRangeRequestsPreserveNoStoreAndSecurityHeaders(t *testing.T) {
	_, server := fixture(t)
	dir := t.TempDir()
	writeFile(t, dir, "data.txt", "0123456789abcdefghijklmnopqrstuvwxyz")
	files, err := Collect([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := PublishFiles(context.Background(), server.Client(), server.URL, files, Options{Title: "Range Test"})
	if err != nil {
		t.Fatal(err)
	}
	targetURL := receipt.Files[0].URL

	checkSecurityHeaders := func(t *testing.T, resp *http.Response) {
		t.Helper()
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control: got %q, want 'no-store'", got)
		}
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options: got %q, want 'nosniff'", got)
		}
		if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("Referrer-Policy: got %q, want 'no-referrer'", got)
		}
		if got := resp.Header.Get("X-Robots-Tag"); got != "noindex, nofollow, noarchive" {
			t.Errorf("X-Robots-Tag: got %q, want 'noindex, nofollow, noarchive'", got)
		}
	}

	// 1. Full GET (200)
	req200, _ := http.NewRequest("GET", targetURL, nil)
	resp200, err := server.Client().Do(req200)
	if err != nil {
		t.Fatal(err)
	}
	resp200.Body.Close()
	if resp200.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp200.StatusCode)
	}
	checkSecurityHeaders(t, resp200)

	// 2. Partial Content (206)
	req206, _ := http.NewRequest("GET", targetURL, nil)
	req206.Header.Set("Range", "bytes=0-9")
	resp206, err := server.Client().Do(req206)
	if err != nil {
		t.Fatal(err)
	}
	data206, _ := io.ReadAll(resp206.Body)
	resp206.Body.Close()
	if resp206.StatusCode != http.StatusPartialContent || string(data206) != "0123456789" {
		t.Fatalf("expected 206 with '0123456789', got %d %q", resp206.StatusCode, string(data206))
	}
	checkSecurityHeaders(t, resp206)

	// 3. Suffix Range (206)
	reqSuffix, _ := http.NewRequest("GET", targetURL, nil)
	reqSuffix.Header.Set("Range", "bytes=-6")
	respSuffix, err := server.Client().Do(reqSuffix)
	if err != nil {
		t.Fatal(err)
	}
	dataSuffix, _ := io.ReadAll(respSuffix.Body)
	respSuffix.Body.Close()
	if respSuffix.StatusCode != http.StatusPartialContent || string(dataSuffix) != "uvwxyz" {
		t.Fatalf("expected 206 with 'uvwxyz', got %d %q", respSuffix.StatusCode, string(dataSuffix))
	}
	checkSecurityHeaders(t, respSuffix)

	// 4. Unsatisfiable Range (416)
	req416, _ := http.NewRequest("GET", targetURL, nil)
	req416.Header.Set("Range", "bytes=1000-2000")
	resp416, err := server.Client().Do(req416)
	if err != nil {
		t.Fatal(err)
	}
	resp416.Body.Close()
	if resp416.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("expected 416, got %d", resp416.StatusCode)
	}
	checkSecurityHeaders(t, resp416)
}
