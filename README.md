# Artifact

Publish files and folders at **https://artifact.harm.org**. HTML renders directly,
relative assets keep working, and each publication has individual downloads and
a ZIP bundle. Both publishing and viewing are public and require no authentication.

```sh
artifact publish report.html
artifact publish ./results/ --entry index.html --title "Benchmark results"
artifact publish board.step measurements.csv --json
```

A link looks like `https://artifact.harm.org/k320s7/report.html`. IDs contain six
cryptographically random lowercase letters/digits, omitting `i`, `l`, and `o`.
The homepage does not enumerate publications. IDs are allocated without replacing
existing publications. Each publish creates a new immutable snapshot.

## Install the CLI

Go 1.23 or newer:

```sh
go install github.com/halbritt/artifact/cmd/artifact@latest
```

Or build the checkout:

```sh
make install
```

This installs `artifact` and `artifactd` into `~/.local/bin`.

## Publishing

A single file keeps its filename. A single folder publishes its contents directly
under the ID; multiple inputs keep each folder's basename. Filenames, including
spaces and Unicode, are preserved and URL-encoded when necessary. Symlinks,
special files, duplicate paths, and any `_artifact` path component are rejected.

The entry page is `--entry`, otherwise `index.html`, otherwise the sole file.
Multiple files without an entry page get a generated file list. Relative assets
should use relative paths, such as `assets/chart.svg`, rather than `/assets/chart.svg`.
Uploaded HTML and JavaScript run as ordinary web content; there is no HTML sanitizer.
This hosts static files, not backend processes or server-side code.

Flags work before or after paths. Use `--` before a filename beginning with `-`.

```sh
artifact publish report.html --title "Routing review"
artifact publish ./output/ --entry reports/summary.html
ARTIFACT_URL=http://127.0.0.1:8879 artifact publish report.html
artifact publish --server http://127.0.0.1:8879 -- --report.html
```

Normal output prints the entry URL, file-list URL, and ZIP URL. `--json` prints a
receipt with `id`, `title`, `created_at`, `entry`, `url`, `files_url`, `download_url`,
`manifest_url`, and `files` (path, size, SHA-256, URL, download URL).

The CLI snapshots the multipart request into a temporary file, verifies the
server's file checksums, and fetches the public manifest before reporting success.
If the network fails after a publish, its outcome can be uncertain; there is no
automatic retry that could silently create duplicate publications.

The service accepts at most **90 MiB per HTTP upload and 1,000 files**. The CLI
reserves 1 MiB for multipart overhead and accepts at most 89 MiB of file contents.
Uploaded files have **no automatic expiration**. There is no public deletion or
mutation endpoint. To remove a publication, the host operator removes its
`published/ID` directory. Keep the returned receipt if you need to find it later.

## HTTP API

```sh
curl -fsS https://artifact.harm.org/api/artifacts \
  -F 'options={"title":"Report","entry":"report.html"}' \
  -F 'file=@report.html'
```

`POST /api/artifacts` accepts multipart `options` JSON and repeated `file` parts;
returns `201` and the receipt. Use the `X-Artifact-Path` **part header** to preserve
nested paths; otherwise the part filename is used. Invalid input returns `400`,
an oversized request `413`, and storage failures `500`.

| Route | Result |
| --- | --- |
| `GET /` | Instructions; no publication catalog |
| `GET /healthz` | Process health |
| `GET /ID/` | Redirect to entry, or render file list |
| `GET /ID/path/to/file` | Original bytes with appropriate media type |
| `GET /ID/path/to/file?download=1` | Attachment download |
| `GET /ID/_artifact/` | File list and download links |
| `GET /ID/_artifact/manifest.json` | Publication receipt |
| `GET /ID/_artifact/bundle.zip` | ZIP containing original files |

HEAD and byte-range downloads are supported. Requests do not enable CORS.
Responses use `no-store`, `noindex`, and `no-referrer`.

## Run the service

```sh
make build
bin/artifactd --listen 127.0.0.1:8879 \
  --data "$HOME/.local/share/artifact" \
  --base-url https://artifact.harm.org
```

Run one writer process per store. Incomplete uploads live in `staging/` and are
removed when the request exits. Each completed publication is atomically renamed
into `published/ID/` after validation and ZIP creation. No database is needed;
restart and backup recovery use the files and manifests directly. A process or
host crash can leave staging directories: stop the service before clearing them.
Back up `published/` to retain artifact contents. Publication uses filesystem
atomic rename, not an fsync-based power-loss durability guarantee.

Production systemd and Cloudflare configuration is maintained in
[infra](https://github.com/halbritt/infra/tree/main/hosts/proximal/config/artifact).

## Development

```sh
make test build
```

Tests use real temporary files and HTTP servers to verify anonymous publishing,
HTML/assets, byte-exact downloads and archives, restart behavior, concurrent
publication, rejected paths, and cleanup after interrupted requests.

`tests/ui_fixture.py` serves synthetic publications for `.uipass.yaml`; no production
artifacts are read by the UI checks. The implementation uses only Go's standard
library.
