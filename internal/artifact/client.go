package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type SourceFile struct{ Path, Local string }

func Collect(inputs []string) ([]SourceFile, error) {
	files := map[string]string{}
	var total int64
	add := func(local, name string, info os.FileInfo) error {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("not a regular file: %s", local)
		}
		name = filepath.ToSlash(name)
		if !validPath(name) {
			return fmt.Errorf("invalid publication path: %s", name)
		}
		if _, exists := files[name]; exists {
			return fmt.Errorf("duplicate publication path: %s", name)
		}
		files[name] = local
		total += info.Size()
		if len(files) > MaxFiles || total > MaxUploadBytes-(1<<20) {
			return errors.New("publication exceeds 89 MiB or 1000 files")
		}
		return nil
	}
	for _, input := range inputs {
		if input == "" {
			return nil, errors.New("empty publication path")
		}
		input = filepath.Clean(input)
		info, err := os.Lstat(input)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if err := add(input, filepath.Base(input), info); err != nil {
				return nil, err
			}
			continue
		}
		// A single folder publishes its contents at the URL root; multiple inputs retain folder names.
		err = filepath.Walk(input, func(local string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(input, local)
			if err != nil {
				return err
			}
			if len(inputs) > 1 {
				rel = filepath.Join(filepath.Base(input), rel)
			}
			return add(local, rel, info)
		})
		if err != nil {
			return nil, err
		}
	}
	if len(files) == 0 {
		return nil, errors.New("no files to publish")
	}
	out := make([]SourceFile, 0, len(files))
	for name, local := range files {
		out = append(out, SourceFile{Path: name, Local: local})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// PublishFiles spools a bounded multipart snapshot, so uploads have a Content-Length
// and local read errors cannot silently produce a successful partial publication.
func PublishFiles(ctx context.Context, client *http.Client, base string, files []SourceFile, options Options) (*Receipt, error) {
	if err := ValidateBaseURL(base); err != nil {
		return nil, err
	}
	body, err := os.CreateTemp("", "artifact-upload-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(body.Name())
	defer body.Close()
	writer := multipart.NewWriter(body)
	encoded, err := json.Marshal(options)
	if err != nil {
		return nil, err
	}
	if err := writer.WriteField("options", string(encoded)); err != nil {
		return nil, err
	}
	hashes := map[string]string{}
	for _, src := range files {
		info, err := os.Lstat(src.Local)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("not a regular file: %s", src.Local)
		}
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": filepath.Base(src.Path)}))
		header.Set("Content-Type", "application/octet-stream")
		header.Set("X-Artifact-Path", src.Path)
		part, err := writer.CreatePart(header)
		if err != nil {
			return nil, err
		}
		f, err := os.Open(src.Local)
		if err != nil {
			return nil, err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(io.MultiWriter(part, hash), io.LimitReader(f, MaxUploadBytes+1))
		closeErr := f.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		hashes[src.Path] = fmt.Sprintf("%x", hash.Sum(nil))
		stat, err := body.Stat()
		if err != nil {
			return nil, err
		}
		if stat.Size() > MaxUploadBytes {
			return nil, errors.New("upload exceeds 90 MiB")
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	stat, err := body.Stat()
	if err != nil {
		return nil, err
	}
	if stat.Size() > MaxUploadBytes {
		return nil, errors.New("upload exceeds 90 MiB")
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(base, "/")+"/api/artifacts", body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = stat.Size()
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("publish: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("publish: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(message)))
	}
	var receipt Receipt
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&receipt); err != nil {
		return nil, err
	}
	if len(receipt.Files) != len(hashes) {
		return nil, errors.New("server receipt has unexpected file count")
	}
	for _, f := range receipt.Files {
		if hashes[f.Path] != f.SHA256 {
			return nil, fmt.Errorf("server checksum mismatch for %s", f.Path)
		}
	}
	check, err := http.NewRequestWithContext(ctx, "GET", receipt.ManifestURL, nil)
	if err != nil {
		return nil, err
	}
	verified, err := client.Do(check)
	if err != nil {
		return nil, fmt.Errorf("published at %s but verification failed: %w", receipt.URL, err)
	}
	defer verified.Body.Close()
	data, err := io.ReadAll(io.LimitReader(verified.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	expected, _ := json.Marshal(receipt)
	var compact bytes.Buffer
	if verified.StatusCode != 200 || json.Compact(&compact, data) != nil || !bytes.Equal(compact.Bytes(), expected) {
		return nil, fmt.Errorf("published at %s but manifest verification failed", receipt.URL)
	}
	return &receipt, nil
}
