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
