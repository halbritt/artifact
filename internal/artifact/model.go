package artifact

import (
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"
)

const MaxUploadBytes int64 = 90 << 20
const MaxFiles = 1000
const DefaultURL = "https://artifact.harm.org"
const metadataDir = "_artifact"

type Options struct {
	Title string `json:"title"`
	Entry string `json:"entry,omitempty"`
}
type File struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	URL         string `json:"url"`
	DownloadURL string `json:"download_url"`
}
type Receipt struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	CreatedAt   time.Time `json:"created_at"`
	Entry       string    `json:"entry,omitempty"`
	URL         string    `json:"url"`
	FilesURL    string    `json:"files_url"`
	DownloadURL string    `json:"download_url"`
	ManifestURL string    `json:"manifest_url"`
	Files       []File    `json:"files"`
}

func validPath(name string) bool {
	if name == "" || name == "." || strings.HasPrefix(name, "/") || path.Clean(name) != name || strings.Contains(name, "\\") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." || part == "." || part == metadataDir {
			return false
		}
	}
	return !strings.ContainsFunc(name, unicode.IsControl)
}
func fileURL(base, id, name string) string {
	u := strings.TrimRight(base, "/") + "/" + id + "/"
	parts := strings.Split(name, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return u + strings.Join(parts, "/")
}
func ValidateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("server must be an http(s) origin, got %q", raw)
	}
	return nil
}
