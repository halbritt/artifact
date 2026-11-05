//go:build e2e

package e2e_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"
)

type AdversarialTestCase struct {
	Name     string
	RawBytes []byte
	Desc     string
}

func checkDirectoryEmpty(t *testing.T, dirPath string, desc string) {
	t.Helper()
	entries, err := os.ReadDir(dirPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("failed to read %s (%s): %v", desc, dirPath, err)
	}
	if len(entries) > 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory %s (%s) is not empty: found residual entries: %v", desc, dirPath, names)
	}
}

func TestAdversarialUTF8_RejectionsAndResidue(t *testing.T) {
	testCases := []AdversarialTestCase{
		// 1. High byte sequences
		{"HighByte_0x80", []byte("file_\x80.txt"), "bare continuation byte 0x80"},
		{"HighByte_0xBF", []byte("file_\xbf.txt"), "bare continuation byte 0xBF"},
		{"Overlong_C0_AF", []byte("dir\xc0\xaffile.txt"), "overlong 2-byte ASCII slash (0xC0 0xAF)"},
		{"Overlong_E0_80_AF", []byte("dir\xe0\x80\xaffile.txt"), "overlong 3-byte ASCII slash (0xE0 0x80 0xAF)"},
		{"Overlong_F0_80_80_AF", []byte("dir\xf0\x80\x80\xaffile.txt"), "overlong 4-byte ASCII slash (0xF0 0x80 0x80 0xAF)"},
		{"Overlong_NUL_C0_80", []byte("file_\xc0\x80.txt"), "overlong NUL (0xC0 0x80)"},
		{"Overlong_C1_BF", []byte("file_\xc1\xbf.txt"), "overlong 2-byte sequence (0xC1 0xBF)"},
		{"Overlong_E0_9F_BF", []byte("file_\xe0\x9f\xbf.txt"), "overlong 3-byte boundary (0xE0 0x9F 0xBF)"},
		{"Overlong_F0_8F_BF_BF", []byte("file_\xf0\x8f\xbf\xbf.txt"), "overlong 4-byte boundary (0xF0 0x8F 0xBF 0xBF)"},

		// 2. Lone surrogate halves
		{"Surrogate_U+D800_LeadingStart", []byte("file_\xed\xa0\x80.txt"), "lone leading surrogate start U+D800 (0xED 0xA0 0x80)"},
		{"Surrogate_U+DB7F_LeadingEnd", []byte("file_\xed\xad\xbf.txt"), "lone leading surrogate end U+DB7F (0xED 0xAD 0xBF)"},
		{"Surrogate_U+DB80_PrivateStart", []byte("file_\xed\xae\x80.txt"), "lone private high surrogate start U+DB80 (0xED 0xAE 0x80)"},
		{"Surrogate_U+DBFF_PrivateEnd", []byte("file_\xed\xaf\xbf.txt"), "lone private high surrogate end U+DBFF (0xED 0xAF 0xBF)"},
		{"Surrogate_U+DC00_TrailingStart", []byte("file_\xed\xb0\x80.txt"), "lone trailing surrogate start U+DC00 (0xED 0xB0 0x80)"},
		{"Surrogate_U+DFFF_TrailingEnd", []byte("file_\xed\xbf\xbf.txt"), "lone trailing surrogate end U+DFFF (0xED 0xBF 0xBF)"},
		{"CESU-8_SurrogatePair", []byte("file_\xed\xa0\x80\xed\xb0\x80.txt"), "CESU-8 surrogate pair in UTF-8"},

		// 3. Out-of-range codepoints (> 0x10FFFF)
		{"OutOfRange_U+110000", []byte("file_\xf4\x90\x80\x80.txt"), "codepoint immediately above max Unicode U+110000 (0xF4 0x90 0x80 0x80)"},
		{"OutOfRange_F5_80_80_80", []byte("file_\xf5\x80\x80\x80.txt"), "out-of-range codepoint (0xF5 0x80 0x80 0x80)"},
		{"OutOfRange_F7_BF_BF_BF", []byte("file_\xf7\xbf\xbf\xbf.txt"), "out-of-range codepoint (0xF7 0xBF 0xBF 0xBF)"},
		{"OutOfRange_5Byte_F8", []byte("file_\xf8\x88\x80\x80\x80.txt"), "5-byte UTF-8 sequence (0xF8 0x88 0x80 0x80 0x80)"},
		{"OutOfRange_6Byte_FC", []byte("file_\xfc\x84\x80\x80\x80\x80.txt"), "6-byte UTF-8 sequence (0xFC 0x84 0x80 0x80 0x80 0x80)"},
		{"InvalidByte_FE", []byte("file_\xfe.txt"), "invalid byte 0xFE"},
		{"InvalidByte_FF", []byte("file_\xff.txt"), "invalid byte 0xFF"},

		// 4. Truncated UTF-8
		{"Truncated_2Byte_EOF", []byte("file_\xc3"), "2-byte header followed by EOF (0xC3)"},
		{"Truncated_2Byte_ASCII", []byte("file_\xc3 .txt"), "2-byte header followed by ASCII space (0xC3 0x20)"},
		{"Truncated_3Byte_EOF", []byte("file_\xe2\x82"), "3-byte header followed by EOF (0xE2 0x82)"},
		{"Truncated_3Byte_Continuation", []byte("file_\xe2\x82 .txt"), "3-byte header followed by ASCII space (0xE2 0x82 0x20)"},
		{"Truncated_4Byte_EOF", []byte("file_\xf0\x9f\x98"), "4-byte header followed by EOF (0xF0 0x9F 0x98)"},
		{"Truncated_4Byte_Continuation", []byte("file_\xf0\x9f\x98 .txt"), "4-byte header followed by ASCII space (0xF0 0x9F 0x98 0x20)"},

		// 5. Mixed valid and invalid UTF-8 in single path or nested paths
		{"Mixed_DirCafE9", []byte("dir/caf\xe9.txt"), "valid directory with invalid Latin-1 filename (dir/caf\\xe9.txt)"},
		{"Mixed_CafE9Dir", []byte("caf\xe9/valid.txt"), "invalid directory with valid filename (caf\\xe9/valid.txt)"},
		{"Mixed_DeepNested", []byte("valid1/caf\xe9/valid2/test.txt"), "deep nested path with invalid middle directory"},
		{"Mixed_HighByteNested", []byte("assets/\x80/style.css"), "nested path with high byte directory"},
	}

	for _, tc := range testCases {
		t.Run(tc.Name+"_ViaPathHeader", func(t *testing.T) {
			d := startTestDaemon(t)
			stagingDir := filepath.Join(d.DataDir, "staging")
			publishedDir := filepath.Join(d.DataDir, "published")

			parts := []MultipartFilePart{
				{
					FileName:   "fallback.txt",
					PathHeader: string(tc.RawBytes),
					Data:       []byte("adversarial test data for " + tc.Desc),
				},
			}
			body, ct := buildCustomMultipart(t, nil, parts)
			req, err := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("failed to create request: %v", err)
			}
			req.Header.Set("Content-Type", ct)
			resp, respBody := fetchHTTP(t, nil, req)

			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("[%s] expected HTTP 400 Bad Request, got %d: %s", tc.Name, resp.StatusCode, string(respBody))
			}

			// Verify ZERO residual files in staging
			checkDirectoryEmpty(t, stagingDir, "staging")
			// Verify ZERO published directories created
			checkDirectoryEmpty(t, publishedDir, "published")
		})

		// For FileName tests: if tc.RawBytes contains path separators ('/'),
		// Note that Go's mime/multipart Part.FileName() applies filepath.Base(),
		// stripping directory components. Therefore, test FileName with the base name
		// or test full path when testing PathHeader.
		baseName := filepath.Base(string(tc.RawBytes))
		t.Run(tc.Name+"_ViaFileName", func(t *testing.T) {
			d := startTestDaemon(t)
			stagingDir := filepath.Join(d.DataDir, "staging")
			publishedDir := filepath.Join(d.DataDir, "published")

			parts := []MultipartFilePart{
				{
					FileName: string(tc.RawBytes),
					Data:     []byte("adversarial test data for " + tc.Desc),
				},
			}
			body, ct := buildCustomMultipart(t, nil, parts)
			req, err := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("failed to create request: %v", err)
			}
			req.Header.Set("Content-Type", ct)
			resp, respBody := fetchHTTP(t, nil, req)

			// If the base name itself has invalid UTF-8, it MUST be rejected with HTTP 400.
			// If baseName is valid UTF-8 (because the invalid bytes were only in directory components
			// stripped by Go's stdlib Part.FileName()), document whether stdlib behavior applies.
			if !utf8.ValidString(baseName) {
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("[%s] expected HTTP 400 Bad Request via FileName, got %d: %s", tc.Name, resp.StatusCode, string(respBody))
				}
				checkDirectoryEmpty(t, stagingDir, "staging")
				checkDirectoryEmpty(t, publishedDir, "published")
			}
		})
	}
}

func TestAdversarialUTF8_MultiFilePartialStagingCleanup(t *testing.T) {
	d := startTestDaemon(t)
	stagingDir := filepath.Join(d.DataDir, "staging")
	publishedDir := filepath.Join(d.DataDir, "published")

	// Multi-file upload where File 1 is valid, File 2 is invalid UTF-8, File 3 is valid
	parts := []MultipartFilePart{
		{
			FileName:   "valid1.txt",
			PathHeader: "valid1.txt",
			Data:       []byte("valid content file 1"),
		},
		{
			FileName:   "bad.txt",
			PathHeader: "bad_dir/invalid_\xff_file.txt",
			Data:       []byte("bad content file 2"),
		},
		{
			FileName:   "valid2.txt",
			PathHeader: "valid2.txt",
			Data:       []byte("valid content file 3"),
		},
	}

	body, ct := buildCustomMultipart(t, nil, parts)
	req, err := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", ct)
	resp, respBody := fetchHTTP(t, nil, req)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 Bad Request for mixed multi-file upload, got %d: %s", resp.StatusCode, string(respBody))
	}

	// Verify ZERO residual files in staging (valid1.txt must have been removed when stage was cleaned up)
	checkDirectoryEmpty(t, stagingDir, "staging")
	// Verify ZERO published directories created
	checkDirectoryEmpty(t, publishedDir, "published")
}

func TestAdversarialUTF8_ValidUnicodeFunctionalFullSuite(t *testing.T) {
	d := startTestDaemon(t)

	validFiles := map[string][]byte{
		// French accents
		"café.html":         []byte("<h1>Café et résumé</h1>"),
		"docs/résumé.txt":   []byte("Mon curriculum vitae avec crème brûlée"),
		"crème_brûlée.json": []byte(`{"dessert": "crème brûlée", "rating": 5}`),

		// German umlauts & eszett
		"münchen.txt":    []byte("Willkommen in München"),
		"straße.html":    []byte("<h2>Große Straße 42</h2>"),
		"österreich.md": []byte("# Schöne Grüße aus Österreich"),

		// Japanese Kanji & Kana
		"日本語/ドキュメント.txt": []byte("こんにちは世界。日本語のドキュメントです。"),
		"東京/index.html":     []byte("<html><body><h1>東京都千代田区</h1></body></html>"),

		// Russian Cyrillic
		"документ.csv":          []byte("имя,город,балл\nИван,Москва,100\nАнна,Киев,95"),
		"отчёты/2026_январь.txt": []byte("Отчёт за январь 2026 года"),

		// Emojis (4-byte UTF-8 codepoints)
		"🎉/rocket 🚀.json":  []byte(`{"event": "launch", "emoji": "🚀", "status": "success"}`),
		"✨/star ⭐.txt":      []byte("Sparkles ✨ and Star ⭐"),
		"deep/🚀/🛸/planet 🪐.dat": []byte("Deep space exploration payload"),

		// Arabic / RTL
		"مرحبا.txt": []byte("أهلاً وسهلاً بالعالم"),

		// Chinese Simplified & Traditional
		"中文/简体/测试.txt": []byte("这是一个中文简体测试文件。"),
		"繁體/測試.md":     []byte("# 繁體中文測試"),

		// Multi-level complex path
		"европа/münchen/café/🎉.html": []byte("<!doctype html><p>All combined</p>"),
	}

	var parts []MultipartFilePart
	for p, content := range validFiles {
		parts = append(parts, MultipartFilePart{
			FileName:   filepath.Base(p),
			PathHeader: p,
			Data:       content,
		})
	}

	optJSON := `{"title":"Comprehensive Unicode Test"}`
	body, ct := buildCustomMultipart(t, &optJSON, parts)

	req, err := http.NewRequest("POST", d.URL+"/api/artifacts", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", ct)
	resp, respBody := fetchHTTP(t, nil, req)

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected HTTP 201 Created for valid Unicode files, got %d: %s", resp.StatusCode, string(respBody))
	}

	var receipt struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		URL         string `json:"url"`
		FilesURL    string `json:"files_url"`
		DownloadURL string `json:"download_url"`
		ManifestURL string `json:"manifest_url"`
		Files       []struct {
			Path        string `json:"path"`
			Size        int64  `json:"size"`
			SHA256      string `json:"sha256"`
			URL         string `json:"url"`
			DownloadURL string `json:"download_url"`
		} `json:"files"`
	}

	if err := json.Unmarshal(respBody, &receipt); err != nil {
		t.Fatalf("failed to parse receipt JSON: %v", err)
	}

	if len(receipt.Files) != len(validFiles) {
		t.Fatalf("expected %d files in receipt, got %d", len(validFiles), len(receipt.Files))
	}

	// 1. Verify exact bytes preserved in manifest and no replacement characters
	for _, f := range receipt.Files {
		if bytes.Contains([]byte(f.Path), []byte("\ufffd")) {
			t.Fatalf("receipt path contains replacement character U+FFFD: %q", f.Path)
		}
		expectedData, ok := validFiles[f.Path]
		if !ok {
			t.Fatalf("receipt contains unknown path: %q", f.Path)
		}
		if f.Size != int64(len(expectedData)) {
			t.Fatalf("path %q size mismatch: got %d, want %d", f.Path, f.Size, len(expectedData))
		}

		// 2. Verify HTTP 200 on file URLs and body matches exact bytes
		reqFile, _ := http.NewRequest("GET", f.URL, nil)
		respFile, dataFile := fetchHTTP(t, nil, reqFile)
		if respFile.StatusCode != http.StatusOK {
			t.Fatalf("GET file URL %q returned status %d", f.URL, respFile.StatusCode)
		}
		if !bytes.Equal(dataFile, expectedData) {
			t.Fatalf("GET file URL %q content mismatch for %s", f.URL, f.Path)
		}

		// Verify download URL also returns exact bytes
		reqDL, _ := http.NewRequest("GET", f.DownloadURL, nil)
		respDL, dataDL := fetchHTTP(t, nil, reqDL)
		if respDL.StatusCode != http.StatusOK {
			t.Fatalf("GET download URL %q returned status %d", f.DownloadURL, respDL.StatusCode)
		}
		if !bytes.Equal(dataDL, expectedData) {
			t.Fatalf("GET download URL %q content mismatch for %s", f.DownloadURL, f.Path)
		}

		// 3. Verify on disk in published directory
		diskPath := filepath.Join(d.DataDir, "published", receipt.ID, filepath.FromSlash(f.Path))
		diskData, err := os.ReadFile(diskPath)
		if err != nil {
			t.Fatalf("failed to read file on disk %s: %v", diskPath, err)
		}
		if !bytes.Equal(diskData, expectedData) {
			t.Fatalf("file on disk %s content mismatch", diskPath)
		}
	}

	// 4. Verify ZIP download and extraction round-trip
	reqZip, _ := http.NewRequest("GET", receipt.DownloadURL, nil)
	respZip, dataZip := fetchHTTP(t, nil, reqZip)
	if respZip.StatusCode != http.StatusOK {
		t.Fatalf("GET ZIP bundle %q returned status %d", receipt.DownloadURL, respZip.StatusCode)
	}

	zipReader, err := zip.NewReader(bytes.NewReader(dataZip), int64(len(dataZip)))
	if err != nil {
		t.Fatalf("failed to open zip archive: %v", err)
	}

	if len(zipReader.File) != len(validFiles) {
		t.Fatalf("expected %d files in zip archive, got %d", len(validFiles), len(zipReader.File))
	}

	for _, zf := range zipReader.File {
		expectedData, ok := validFiles[zf.Name]
		if !ok {
			t.Fatalf("unexpected file in ZIP archive: %q", zf.Name)
		}
		rc, err := zf.Open()
		if err != nil {
			t.Fatalf("failed to open zip file %s: %v", zf.Name, err)
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("failed to read zip file %s: %v", zf.Name, err)
		}
		if !bytes.Equal(content, expectedData) {
			t.Fatalf("zip file %s content mismatch", zf.Name)
		}
	}
}

func TestAdversarialUTF8_CLIPublish(t *testing.T) {
	d := startTestDaemon(t)
	tmpDir := t.TempDir()

	// 1. Invalid UTF-8 file in CLI
	badName := "bad_\xff_byte.txt"
	badFile := filepath.Join(tmpDir, badName)
	if err := os.WriteFile(badFile, []byte("content"), 0644); err != nil {
		t.Skipf("filesystem rejected creating invalid UTF-8 filename: %v", err)
	}

	res := runCLI(t, nil, "publish", badFile, "--server", d.URL)
	if res.ExitCode == 0 {
		t.Fatalf("expected CLI publish of invalid UTF-8 file to fail, got exit 0: stdout: %s", res.Stdout)
	}

	// 2. Folder containing invalid UTF-8 file
	badSubdir := filepath.Join(tmpDir, "subfolder")
	if err := os.MkdirAll(badSubdir, 0755); err == nil {
		_ = os.WriteFile(filepath.Join(badSubdir, "caf\xe9.txt"), []byte("data"), 0644)
		resDir := runCLI(t, nil, "publish", badSubdir, "--server", d.URL)
		if resDir.ExitCode == 0 {
			t.Fatalf("expected CLI publish of folder with invalid UTF-8 to fail, got exit 0")
		}
	}

	// 3. Valid Unicode files in CLI
	validUnicodeDir := filepath.Join(tmpDir, "valid_unicode")
	if err := os.MkdirAll(validUnicodeDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(validUnicodeDir, "café.html"), []byte("<h1>Café</h1>"), 0644)
	_ = os.WriteFile(filepath.Join(validUnicodeDir, "日本語.txt"), []byte("こんにちは"), 0644)
	_ = os.WriteFile(filepath.Join(validUnicodeDir, "🚀.json"), []byte(`{"rocket":true}`), 0644)

	resValid := runCLI(t, nil, "publish", validUnicodeDir, "--server", d.URL)
	if resValid.ExitCode != 0 {
		t.Fatalf("expected CLI publish of valid Unicode files to succeed, got exit %d: stderr: %s", resValid.ExitCode, resValid.Stderr)
	}
}

