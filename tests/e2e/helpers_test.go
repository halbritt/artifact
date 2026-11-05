//go:build e2e

package e2e_test

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var (
	buildOnce     sync.Once
	artifactBin   string
	artifactdBin  string
	buildErr      error
	projectRoot   string
)

func init() {
	cwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	// Locate repository root from tests/e2e
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			projectRoot = dir
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("could not locate project root containing go.mod")
		}
		dir = parent
	}
}

func ensureBinaries(t *testing.T) (string, string) {
	t.Helper()
	buildOnce.Do(func() {
		binDir := filepath.Join(projectRoot, "bin")
		if err := os.MkdirAll(binDir, 0755); err != nil {
			buildErr = err
			return
		}
		artifactBin = filepath.Join(binDir, "artifact")
		artifactdBin = filepath.Join(binDir, "artifactd")

		cmdCLI := exec.Command("go", "build", "-trimpath", "-o", artifactBin, "./cmd/artifact")
		cmdCLI.Dir = projectRoot
		if out, err := cmdCLI.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build artifact failed: %v: %s", err, string(out))
			return
		}

		cmdDaemon := exec.Command("go", "build", "-trimpath", "-o", artifactdBin, "./cmd/artifactd")
		cmdDaemon.Dir = projectRoot
		if out, err := cmdDaemon.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build artifactd failed: %v: %s", err, string(out))
			return
		}
	})
	if buildErr != nil {
		t.Fatalf("ensureBinaries: %v", buildErr)
	}
	return artifactBin, artifactdBin
}

type TestDaemon struct {
	URL     string
	Port    int
	DataDir string
	Cmd     *exec.Cmd
}

func getFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("getFreePort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startTestDaemon(t *testing.T, extraArgs ...string) *TestDaemon {
	t.Helper()
	_, dBin := ensureBinaries(t)
	port := getFreePort(t)
	dataDir := t.TempDir()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	baseURL := fmt.Sprintf("http://%s", addr)

	args := []string{"--listen", addr, "--data", dataDir, "--base-url", baseURL}
	args = append(args, extraArgs...)

	cmd := exec.Command(dBin, args...)
	cmd.Dir = projectRoot
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}

	d := &TestDaemon{
		URL:     baseURL,
		Port:    port,
		DataDir: dataDir,
		Cmd:     cmd,
	}

	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	// Wait for healthz
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	started := false
	for time.Now().Before(deadline) {
		resp, err := client.Get(baseURL + "/healthz")
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			started = true
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !started {
		_ = cmd.Process.Kill()
		t.Fatalf("daemon failed to start at %s. Stderr: %s", baseURL, stderr.String())
	}

	return d
}

type CLIResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}

func runCLI(t *testing.T, env []string, args ...string) CLIResult {
	t.Helper()
	cBin, _ := ensureBinaries(t)
	cmd := exec.Command(cBin, args...)
	cmd.Dir = projectRoot
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	return CLIResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		Err:      err,
	}
}

func writeTempFile(t *testing.T, dir, relPath, content string) string {
	t.Helper()
	fullPath := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		t.Fatalf("mkdir for %s: %v", relPath, err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		t.Fatalf("write file %s: %v", relPath, err)
	}
	return fullPath
}

func fetchHTTP(t *testing.T, client *http.Client, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("fetchHTTP %s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, body
}

type MultipartFilePart struct {
	HeaderName string
	PathHeader string
	FileName   string
	Data       []byte
}

func buildCustomMultipart(t *testing.T, optionsJSON *string, parts []MultipartFilePart) ([]byte, string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	if optionsJSON != nil {
		if err := w.WriteField("options", *optionsJSON); err != nil {
			t.Fatalf("write options field: %v", err)
		}
	}
	for _, p := range parts {
		h := make(textproto.MIMEHeader)
		fn := p.FileName
		if fn == "" {
			fn = "file"
		}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, fn))
		h.Set("Content-Type", "application/octet-stream")
		if p.PathHeader != "" {
			h.Set("X-Artifact-Path", p.PathHeader)
		}
		partWriter, err := w.CreatePart(h)
		if err != nil {
			t.Fatalf("create part: %v", err)
		}
		if _, err := partWriter.Write(p.Data); err != nil {
			t.Fatalf("write part data: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return b.Bytes(), w.FormDataContentType()
}
