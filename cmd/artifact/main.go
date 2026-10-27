package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/halbritt/artifact/internal/artifact"
)

func run(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Println("Usage: artifact publish FILE_OR_DIR... [--title TITLE] [--entry index.html] [--json] [--server URL]\n\nPublishes publicly without authentication. Returns preview, file-list and ZIP URLs.\nA single folder preserves its contents relative to the publication root.\nDefault server: https://artifact.harm.org (override with ARTIFACT_URL).")
		return nil
	}
	if args[0] != "publish" {
		return fmt.Errorf("unknown command %q; use artifact --help", args[0])
	}
	flags := flag.NewFlagSet("publish", flag.ContinueOnError)
	base := os.Getenv("ARTIFACT_URL")
	if base == "" {
		base = artifact.DefaultURL
	}
	flags.StringVar(&base, "server", base, "server origin")
	title := flags.String("title", "", "publication title")
	entry := flags.String("entry", "", "entry file relative to publication root")
	asJSON := flags.Bool("json", false, "print JSON receipt")
	// Accept flags before or after filenames, matching the documented shell workflow.
	var opts, paths []string
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			paths = append(paths, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") {
			paths = append(paths, arg)
			continue
		}
		opts = append(opts, arg)
		if arg == "--title" || arg == "--entry" || arg == "--server" || arg == "-title" || arg == "-entry" || arg == "-server" {
			i++
			if i >= len(args) {
				return fmt.Errorf("missing value for %s", arg)
			}
			opts = append(opts, args[i])
		}
	}
	if err := flags.Parse(opts); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("provide at least one file or directory")
	}
	files, err := artifact.Collect(paths)
	if err != nil {
		return err
	}
	receipt, err := artifact.PublishFiles(context.Background(), &http.Client{Timeout: 5 * time.Minute}, base, files, artifact.Options{Title: *title, Entry: *entry})
	if err != nil {
		return err
	}
	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(receipt)
	}
	fmt.Println(receipt.URL)
	fmt.Println("Files:   " + receipt.FilesURL)
	fmt.Println("ZIP:     " + receipt.DownloadURL)
	return nil
}
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "artifact:", err)
		os.Exit(1)
	}
}
