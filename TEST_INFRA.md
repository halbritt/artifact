# Artifact E2E Testing Infrastructure

## 1. Testing Philosophy

The Artifact E2E test suite is designed around the following foundational principles:
- **Opaque-Box & Requirement-Driven**: Tests interact with compiled binaries (`bin/artifact`, `bin/artifactd`) or network interfaces (HTTP) exactly as external users and clients do. Internal implementation details are never stubbed or peered into.
- **Authoritative Specifications**: Every test case derives expected behaviors, inputs, and outputs directly from the issue contracts documented in `PROJECT.md` and Plane work items (`ARTIFACT-1` through `ARTIFACT-11`).
- **Zero External Dependencies**: The test harness relies exclusively on the Go standard library and existing repository fixtures (`tests/ui_fixture.py`).
- **Strict Isolation & Concurrency Safety**: Each test provisions its own ephemeral daemon process, isolated filesystem store root, and unique loopback port. Tests clean up all child processes, sockets, and temporary directories upon exit.
- **Progressive Testability**: Tests clearly demarcate baseline behavior from milestone requirements, surfacing implementation regressions or uncompleted features cleanly.

## 2. Test Architecture

The E2E test suite resides in `tests/e2e/` with the Go build tag `//go:build e2e`.

### Components
1. **Test Harness (`helpers_test.go`)**:
   - `buildBinaries(t)`: Ensures `bin/artifact` and `bin/artifactd` binaries are compiled via `go build`.
   - `startDaemon(t, extraArgs...)`: Starts an ephemeral `artifactd` instance on an allocated TCP loopback port using a temporary directory, waits for `/healthz`, and registers teardown hooks via `t.Cleanup`.
   - `runCLI(t, args...)`: Executes `bin/artifact` with specified arguments, returning stdout, stderr, and exit code.
   - `makeMultipartUpload(...)`: Constructs compliant or adversarial multipart payloads for direct HTTP API verification.
2. **Four Test Tiers**:
   - **Tier 1 (`tier1_test.go`)**: Isolated contract validation for each feature (ARTIFACT-1 through ARTIFACT-11).
   - **Tier 2 (`tier2_test.go`)**: Adversarial boundary conditions, protocol malformations, and edge cases.
   - **Tier 3 (`tier3_test.go`)**: Cross-feature interactions and composite scenarios.
   - **Tier 4 (`tier4_test.go`)**: Real-world publishing workloads, end-to-end asset flows, ZIP downloads, and restart recovery.

## 3. Feature Inventory & Mapping

| Feature ID | Scope / Target | Tier 1 (Contract) | Tier 2 (Boundary) | Tier 3 (Cross-Feature) | Tier 4 (Workload) |
|---|---|---|---|---|---|
| ARTIFACT-1 | Empty path rejection | `TestTier1_ARTIFACT1_EmptyPath` | `TestTier2_Boundary_EmptyPath` | `TestTier3_EmptyAndValidMixed` | `TestTier4_Workload_Deployment` |
| ARTIFACT-2 | Large inventory receipt | `TestTier1_ARTIFACT2_LargeInventory` | `TestTier2_Boundary_ReceiptLimit` | `TestTier3_LargeTreeDeepNesting` | `TestTier4_Workload_Deployment` |
| ARTIFACT-3 | Invalid UTF-8 rejection | `TestTier1_ARTIFACT3_InvalidUTF8` | `TestTier2_Boundary_UTF8Sequences` | `TestTier3_UTF8AndSpacePaths` | `TestTier4_Workload_Deployment` |
| ARTIFACT-4 | Filename space preservation | `TestTier1_ARTIFACT4_SpacePreservation` | `TestTier2_Boundary_WhitespaceFilenames` | `TestTier3_UTF8AndSpacePaths` | `TestTier4_Workload_Deployment` |
| ARTIFACT-5 | Bare origin delimiter rejection | `TestTier1_ARTIFACT5_BareOriginDelimiter` | `TestTier2_Boundary_OriginDelimiters` | `TestTier3_ServerOriginVariations` | `TestTier4_Workload_Deployment` |
| ARTIFACT-6 | Exact-once receipt inventory | `TestTier1_ARTIFACT6_ExactInventoryMatch` | `TestTier2_Boundary_ReceiptDuplicates` | `TestTier3_ManipulatedReceipts` | `TestTier4_Workload_Deployment` |
| ARTIFACT-7 | HTTP 413 on options overflow | `TestTier1_ARTIFACT7_OptionsLimit413` | `TestTier2_Boundary_OptionsPayloadLimit` | `TestTier3_LargeOptionsAndFiles` | `TestTier4_Workload_Deployment` |
| ARTIFACT-8 | HTTP 400 on unrepresentable path | `TestTier1_ARTIFACT8_UnrepresentablePath` | `TestTier2_Boundary_PathComponentLimits` | `TestTier3_LongComponentMix` | `TestTier4_Workload_Deployment` |
| ARTIFACT-9 | Range 416 retains no-store | `TestTier1_ARTIFACT9_RangeNoStore` | `TestTier2_Boundary_RangeHeaders` | `TestTier3_ConcurrentRangeRequests` | `TestTier4_Workload_Deployment` |
| ARTIFACT-10 | Daemon shutdown staging cleanup | `TestTier1_ARTIFACT10_ShutdownStagingClean` | `TestTier2_Boundary_ShutdownTimeouts` | `TestTier3_ShutdownDuringUpload` | `TestTier4_Workload_Deployment` |
| ARTIFACT-11 | UI fixture SIGTERM cleanup | `TestTier1_ARTIFACT11_FixtureSigtermClean` | `TestTier2_Boundary_FixtureSignals` | `TestTier3_FixtureLifecycleStress` | `TestTier4_Workload_Deployment` |

## 4. Coverage Thresholds & Quality Gates

The E2E test suite adheres to strict quality gates:
1. **100% Feature Traceability**: Every issue from ARTIFACT-1 through ARTIFACT-11 has dedicated test coverage in Tiers 1 through 3.
2. **Deterministic Exit Codes**:
   - Client and API failures must produce expected exit codes (e.g. exit code 1 on CLI error, HTTP 400 for bad input, HTTP 413 for payload too large, HTTP 416 for invalid range).
3. **Hermetic Filesystem Sanitation**:
   - Tests assert that `staging/` is 100% clean after failed, rejected, aborted, or shut-down uploads.
   - Tests assert that no temporary files or orphaned processes linger after test execution.
4. **Header Retention Guarantee**:
   - All responses (including error and range responses) must preserve core security headers: `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `X-Robots-Tag: noindex, nofollow, noarchive`, `Cache-Control: no-store`.

## 5. Execution Guide

```bash
# Build production binaries
make build

# Run the complete E2E test suite
go test -v -tags e2e ./tests/e2e/...

# Run specific tier
go test -v -tags e2e -run TestTier1 ./tests/e2e/...
go test -v -tags e2e -run TestTier2 ./tests/e2e/...
go test -v -tags e2e -run TestTier3 ./tests/e2e/...
go test -v -tags e2e -run TestTier4 ./tests/e2e/...

# Run tests for a specific issue
go test -v -tags e2e -run ARTIFACT1 ./tests/e2e/...
```
