package artifact

import (
	"archive/zip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime/multipart"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Store struct {
	Root, BaseURL string
	mu            sync.Mutex
}
type InputError struct {
	Message string
	Cause   error
}

func (e *InputError) Error() string        { return e.Message }
func (e *InputError) Unwrap() error        { return e.Cause }
func bad(format string, args ...any) error { return &InputError{Message: fmt.Sprintf(format, args...)} }

func NewStore(root, base string) (*Store, error) {
	if err := ValidateBaseURL(base); err != nil {
		return nil, err
	}
	for _, dir := range []string{"staging", "published"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			return nil, err
		}
	}
	return &Store{Root: root, BaseURL: strings.TrimRight(base, "/")}, nil
}

// Publish writes only to staging until every part, manifest and archive is complete.
func (s *Store) Publish(reader *multipart.Reader) (*Receipt, error) {
	stage, err := os.MkdirTemp(filepath.Join(s.Root, "staging"), "upload-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	options := Options{}
	seenOptions := false
	files := map[string]File{}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, &InputError{Message: fmt.Sprintf("incomplete upload: %v", err), Cause: err}
		}
		switch part.FormName() {
		case "options":
			if seenOptions {
				return nil, bad("duplicate options")
			}
			data, err := io.ReadAll(io.LimitReader(part, 8193))
			if err != nil || len(data) > 8192 {
				return nil, bad("invalid options")
			}
			if err := json.Unmarshal(data, &options); err != nil {
				return nil, bad("invalid options JSON")
			}
			if len(options.Title) > 500 {
				return nil, bad("title exceeds 500 bytes")
			}
			seenOptions = true
		case "file":
			name := part.Header.Get("X-Artifact-Path")
			if name == "" {
				name = part.FileName()
			}
			if !validPath(name) {
				return nil, bad("invalid file path %q", name)
			}
			for existing := range files {
				if name == existing || strings.HasPrefix(name, existing+"/") || strings.HasPrefix(existing, name+"/") {
					return nil, bad("duplicate or conflicting file %q", name)
				}
			}
			if len(files) >= MaxFiles {
				return nil, bad("maximum %d files", MaxFiles)
			}
			target := filepath.Join(stage, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return nil, err
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
			if err != nil {
				return nil, err
			}
			hash := sha256.New()
			n, copyErr := io.Copy(io.MultiWriter(f, hash), part)
			closeErr := f.Close()
			if copyErr != nil {
				var diskError *os.PathError
				if errors.As(copyErr, &diskError) {
					return nil, copyErr
				}
				return nil, &InputError{Message: fmt.Sprintf("incomplete file %q: %v", name, copyErr), Cause: copyErr}
			}
			if closeErr != nil {
				return nil, closeErr
			}
			files[name] = File{Path: name, Size: n, SHA256: fmt.Sprintf("%x", hash.Sum(nil))}
		default:
			return nil, bad("unknown form part %q", part.FormName())
		}
		if err := part.Close(); err != nil {
			return nil, bad("incomplete part: %v", err)
		}
	}
	if len(files) == 0 {
		return nil, bad("at least one file is required")
	}
	if options.Entry == "" {
		if _, ok := files["index.html"]; ok {
			options.Entry = "index.html"
		} else if len(files) == 1 {
			for name := range files {
				options.Entry = name
			}
		}
	}
	if options.Entry != "" {
		if _, ok := files[options.Entry]; !ok {
			return nil, bad("entry %q is not in upload", options.Entry)
		}
	}
	if options.Title == "" {
		options.Title = "Artifact"
	}
	meta := filepath.Join(stage, metadataDir)
	if err := os.Mkdir(meta, 0755); err != nil {
		return nil, err
	}
	archiveFiles := make([]File, 0, len(files))
	for _, f := range files {
		archiveFiles = append(archiveFiles, f)
	}
	sort.Slice(archiveFiles, func(i, j int) bool { return archiveFiles[i].Path < archiveFiles[j].Path })
	if err := makeArchive(stage, archiveFiles); err != nil {
		return nil, err
	}
	// Serialize allocation and final rename, so colliding IDs never replace a publication.
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.availableID()
	if err != nil {
		return nil, err
	}
	base := s.BaseURL + "/" + id + "/"
	receipt := &Receipt{ID: id, Title: options.Title, Entry: options.Entry, CreatedAt: time.Now().UTC(), URL: base, FilesURL: base + metadataDir + "/", DownloadURL: base + metadataDir + "/bundle.zip", ManifestURL: base + metadataDir + "/manifest.json"}
	if options.Entry != "" {
		receipt.URL = fileURL(s.BaseURL, id, options.Entry)
	}
	for name, f := range files {
		f.URL = fileURL(s.BaseURL, id, name)
		f.DownloadURL = f.URL + "?download=1"
		receipt.Files = append(receipt.Files, f)
	}
	sort.Slice(receipt.Files, func(i, j int) bool { return receipt.Files[i].Path < receipt.Files[j].Path })
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(meta, "manifest.json"), data, 0644); err != nil {
		return nil, err
	}
	if err := os.Rename(stage, filepath.Join(s.Root, "published", id)); err != nil {
		return nil, err
	}
	return receipt, nil
}
func (s *Store) availableID() (string, error) {
	const alphabet = "0123456789abcdefghjkmnpqrstuvwxyz"
	for attempt := 0; attempt < 100; attempt++ {
		b := make([]byte, 6)
		for i := range b {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
			if err != nil {
				return "", err
			}
			b[i] = alphabet[n.Int64()]
		}
		id := string(b)
		_, err := os.Stat(filepath.Join(s.Root, "published", id))
		if errors.Is(err, os.ErrNotExist) {
			return id, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate an artifact ID")
}
func makeArchive(root string, files []File) error {
	f, err := os.Create(filepath.Join(root, metadataDir, "bundle.zip"))
	if err != nil {
		return err
	}
	z := zip.NewWriter(f)
	for _, entry := range files {
		w, err := z.Create(entry.Path)
		if err == nil {
			var src *os.File
			src, err = os.Open(filepath.Join(root, filepath.FromSlash(entry.Path)))
			if err == nil {
				_, err = io.Copy(w, src)
				src.Close()
			}
		}
		if err != nil {
			z.Close()
			f.Close()
			return err
		}
	}
	if err := z.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
func (s *Store) Load(id string) (*Receipt, error) {
	if len(id) != 6 || strings.ContainsAny(id, "/\\.") {
		return nil, os.ErrNotExist
	}
	data, err := os.ReadFile(filepath.Join(s.Root, "published", id, metadataDir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var receipt Receipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return nil, err
	}
	return &receipt, nil
}
