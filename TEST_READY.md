# Artifact E2E Test Suite Specification & Readiness Report

## Executive Summary

The end-to-end test suite for Artifact is fully designed, implemented, and verified in `tests/e2e/`. It exercises the compiled `bin/artifact` CLI and `bin/artifactd` server binaries over loopback HTTP and local filesystems, providing opaque-box verification with zero external Go dependencies beyond the standard library and existing python UI test fixture.

## Test Runner Commands

The test suite uses the standard Go test toolchain with build tag `e2e` to isolate E2E binary tests from fast unit checks:

```bash
# Ensure binaries are built
make build

# Run the complete E2E test suite (all tiers)
go test -v -tags e2e ./tests/e2e/...

# Run specific tiers
go test -v -tags e2e -run TestTier1 ./tests/e2e/...
go test -v -tags e2e -run TestTier2 ./tests/e2e/...
go test -v -tags e2e -run TestTier3 ./tests/e2e/...
go test -v -tags e2e -run TestTier4 ./tests/e2e/...

# Run specific feature tests (e.g. ARTIFACT-1)
go test -v -tags e2e -run ARTIFACT1 ./tests/e2e/...
```

Standard unit tests and build validation continue to pass cleanly via:
```bash
make test build
```

## Coverage & Feature Traceability

All 11 inventoried Plane issues (ARTIFACT-1 through ARTIFACT-11) are mapped to test cases across 4 tiers:

| Issue | Target Feature | Tier 1 (Contract) | Tier 2 (Boundary) | Tier 3 (Cross) | Baseline Status (Pre-Fix) |
|---|---|---|---|---|---|
| ARTIFACT-1 | Empty path rejection | `TestTier1_ARTIFACT1_EmptyPath` | `TestTier2_Boundary_EmptyPath` | `TestTier3_Cross_EmptyMixedWithUnicodeAndSpaces` | FAILS (Publishes cwd) |
| ARTIFACT-2 | Large inventory receipt | `TestTier1_ARTIFACT2_LargeInventory` | `TestTier2_Boundary_ReceiptLimit` | `TestTier3_Cross_LargeInventoryWithExplicitEntry` | PASS (Baseline) |
| ARTIFACT-3 | Invalid UTF-8 rejection | `TestTier1_ARTIFACT3_InvalidUTF8` | `TestTier2_Boundary_UTF8Sequences` | `TestTier3_Cross_UnicodeAndSpacedWithEntry` | FAILS (Returns 201 with \ufffd) |
| ARTIFACT-4 | Filename space preservation | `TestTier1_ARTIFACT4_SpacePreservation` | `TestTier2_Boundary_WhitespaceFilenames` | `TestTier3_Cross_RangeOnSpacedFile` | FAILS (Strips outer spaces) |
| ARTIFACT-5 | Bare origin delimiter rejection | `TestTier1_ARTIFACT5_BareOriginDelimiter` | `TestTier2_Boundary_OriginDelimiters` | `TestTier3_Cross_BareDelimiterWithFullOptions` | PASS (Basic check) |
| ARTIFACT-6 | Exact-once receipt inventory | `TestTier1_ARTIFACT6_ExactInventoryMatch` | `TestTier2_Boundary_ReceiptDuplicates` | N/A | PASS (Baseline unique) |
| ARTIFACT-7 | HTTP 413 on options overflow | `TestTier1_ARTIFACT7_OptionsLimit413` | `TestTier2_Boundary_OptionsPayloadLimit` | N/A | FAILS (Returns 400 instead of 413) |
| ARTIFACT-8 | HTTP 400 on unrepresentable path | `TestTier1_ARTIFACT8_UnrepresentablePath` | `TestTier2_Boundary_PathComponentLimits` | N/A | FAILS (Returns 500 instead of 400) |
| ARTIFACT-9 | Range 416 retains no-store | `TestTier1_ARTIFACT9_RangeNoStore` | `TestTier2_Boundary_RangeHeaders` | `TestTier3_Cross_RangeOnSpacedFile` | FAILS (Missing Cache-Control) |
| ARTIFACT-10 | Daemon shutdown staging clean | `TestTier1_ARTIFACT10_ShutdownStagingClean` | `TestTier2_Boundary_ClientDisconnectCleanup` | N/A | FAILS (Leaves staging directory) |
| ARTIFACT-11 | UI fixture SIGTERM clean | `TestTier1_ARTIFACT11_FixtureSigtermClean` | N/A | N/A | PASS (Process cleanly reaped) |
| Real-World | Static site, ZIP, Restart, Concurrency | N/A | N/A | N/A | 100% PASS (Tier 4) |

## Test Suite Architecture & File Layout

- **`tests/e2e/helpers_test.go`**:
  - Ephemeral daemon process lifecycle manager (`startTestDaemon`) with dynamic port allocation and `/healthz` readiness polling.
  - Subprocess CLI execution runner (`runCLI`) with environment isolation and exit code extraction.
  - Multipart payload generator (`buildCustomMultipart`) supporting adversarial, truncated, and raw byte structures.
- **`tests/e2e/tier1_test.go`**:
  - Direct, isolated contract validation for each feature ARTIFACT-1 through ARTIFACT-11.
- **`tests/e2e/tier2_test.go`**:
  - Boundary and stress testing for path lengths, UTF-8 byte sequences, whitespace encodings, HTTP Range edge values, and request-body limit crossings.
- **`tests/e2e/tier3_test.go`**:
  - Cross-feature combinations verifying that multiple edge features do not interact negatively.
- **`tests/e2e/tier4_test.go`**:
  - End-to-end user workflows: complete multi-file HTML5 application deployment, relative link traversal, ZIP bundle round-trip download and extraction, daemon restart snapshot immutability, and concurrent publication isolation.

## Discovered Defect Escalation Matrix (Pre-Fix Verification)

The baseline run against commit `e8940c6` definitively reproduced the reported defects:
1. **ARTIFACT-1**: CLI exited 0 on `artifact publish ""` and published current directory contents to the server.
2. **ARTIFACT-3**: Direct API upload of `caf\xe9.txt` returned HTTP 201 Created and serialized replacement character `\ufffd` into manifest, breaking file retrieval with 404.
3. **ARTIFACT-4**: Outer whitespace in filenames (`" leading_and_trailing.txt "`) was stripped by MIME header parsing into `"leading_and_trailing.txt"`, causing checksum verification failure and broken downloads.
4. **ARTIFACT-7**: Options payload exceeding `MaxUploadBytes` returned HTTP 400 Bad Request instead of HTTP 413 Request Entity Too Large.
5. **ARTIFACT-8**: 256+ byte filename component returned HTTP 500 Internal Server Error instead of HTTP 400 Bad Request.
6. **ARTIFACT-9**: Unsatisfiable range request (`Range: bytes=99999-99999`) returned HTTP 416 without `Cache-Control: no-store`.
7. **ARTIFACT-10**: Daemon shutdown during stalled multipart upload left orphan directory in `staging/`.
