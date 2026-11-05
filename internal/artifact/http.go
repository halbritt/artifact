package artifact

import (
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

var page = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{if .}}{{.Title}}{{else}}Artifact{{end}}</title>
<style>body{font:17px/1.6 system-ui,sans-serif;max-width:850px;margin:60px auto;padding:0 24px;color:#213244;background:#f5f7fa}a{color:#075cb5}h1{line-height:1.2}code{background:#e6ebf0;padding:3px 6px;border-radius:4px}pre{padding:16px;background:#e6ebf0;overflow:auto;border-radius:8px}li{margin:12px 0;overflow-wrap:anywhere}.size{color:#566574;font-size:14px}footer{margin-top:40px;color:#566574}</style>
<main>{{if .}}<p><a href="/">Artifact</a></p><h1>{{.Title}}</h1><p>{{if .Entry}}<a href="{{.URL}}">Open artifact</a> · {{end}}<a href="{{.DownloadURL}}">Download ZIP</a> · <a href="{{.ManifestURL}}">Manifest</a></p><ul>{{range .Files}}<li><a href="{{.URL}}">{{.Path}}</a> <span class="size">({{.Size}} bytes)</span> · <a href="{{.DownloadURL}}">download</a></li>{{end}}</ul><footer>Published {{.CreatedAt.Format "2006-01-02 15:04 UTC"}} · {{.ID}}</footer>{{else}}<h1>Artifact</h1><p>Publish a file or folder. Get a link.</p><pre role="region" tabindex="0" aria-label="Publishing commands">artifact publish report.html
artifact publish ./results/ --entry index.html
artifact publish chart.svg results.csv --json</pre><p>HTML opens in your browser. Every publication includes individual downloads and a ZIP bundle.</p><p><a href="https://github.com/halbritt/artifact">CLI and documentation</a></p>{{end}}</main></html>`))

type noStoreResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *noStoreResponseWriter) applyHeaders() {
	if !w.wroteHeader {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Robots-Tag", "noindex, nofollow, noarchive")
		h.Set("Cache-Control", "no-store")
		w.wroteHeader = true
	}
}

func (w *noStoreResponseWriter) WriteHeader(code int) {
	w.applyHeaders()
	w.ResponseWriter.WriteHeader(code)
}

func (w *noStoreResponseWriter) Write(b []byte) (int, error) {
	w.applyHeaders()
	return w.ResponseWriter.Write(b)
}

func (w *noStoreResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *noStoreResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *noStoreResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	w.applyHeaders()
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}

func (s *Store) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/artifacts", s.upload)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /", s.get)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(&noStoreResponseWriter{ResponseWriter: w}, r)
	})
}
func (s *Store) upload(w http.ResponseWriter, r *http.Request) {
	s.wg.Add(1)
	defer s.wg.Done()
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "expected multipart/form-data", 400)
		return
	}
	receipt, err := s.Publish(mr)
	if err != nil {
		var input *InputError
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "upload exceeds 90 MiB", http.StatusRequestEntityTooLarge)
		} else if errors.As(err, &input) {
			http.Error(w, input.Error(), 400)
		} else {
			log.Printf("publish: %v", err)
			http.Error(w, "could not store artifact", 500)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Location", receipt.URL)
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(receipt)
}
func renderPage(w http.ResponseWriter, receipt *Receipt) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.Execute(w, receipt); err != nil {
		log.Printf("render page: %v", err)
	}
}
func (s *Store) get(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		renderPage(w, nil)
		return
	}
	id, rel, hasSlash := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	receipt, err := s.Load(id)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("load %q: %v", id, err)
		}
		http.NotFound(w, r)
		return
	}
	if !hasSlash {
		http.Redirect(w, r, "/"+id+"/", http.StatusTemporaryRedirect)
		return
	}
	if rel == "" {
		if receipt.Entry != "" {
			http.Redirect(w, r, receipt.URL, http.StatusTemporaryRedirect)
		} else {
			renderPage(w, receipt)
		}
		return
	}
	if rel == metadataDir+"/" || rel == metadataDir {
		renderPage(w, receipt)
		return
	}
	allowed := rel == metadataDir+"/manifest.json" || rel == metadataDir+"/bundle.zip"
	if strings.HasSuffix(rel, "/") {
		rel += "index.html"
	}
	for _, f := range receipt.Files {
		if f.Path == rel {
			allowed = true
			break
		}
	}
	if !allowed {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(filepath.Join(s.Root, "published", id, filepath.FromSlash(rel)))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		http.Error(w, "cannot read artifact", 500)
		return
	}
	if r.URL.Query().Get("download") == "1" || rel == metadataDir+"/bundle.zip" {
		name := path.Base(rel)
		if rel == metadataDir+"/bundle.zip" {
			name = id + ".zip"
		}
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}
