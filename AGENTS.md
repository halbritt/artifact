# Artifact

The owner explicitly chose public, unauthenticated publishing and reads at
artifact.harm.org. Preserve that contract. Short random URLs with original
filenames are intentional; do not introduce accounts, tokens, or private defaults.

- CLI: `cmd/artifact`; daemon: `cmd/artifactd`; shared behavior: `internal/artifact`.
- Keep dependencies in the Go standard library unless a concrete need warrants more.
- Publications are immutable snapshots. No global listing, automatic expiration,
  or public mutation/deletion API is part of the current contract.
- Preserve relative asset paths and arbitrary static HTML/JavaScript behavior.
- Tests: `make test build`. UI: `.uipass.yaml` uses disposable local fixtures.
- Production configuration belongs to `~/git/infra/hosts/proximal/config/artifact`.
- Commit and push verified work to main. Never leave dirty or unpushed work.
