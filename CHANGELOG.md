# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Contributor documents: `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`,
  `SECURITY.md`, `NOTICE`, `CODEOWNERS`, and issue and pull request
  templates.
- Contributor License Agreement (individual and corporate) enforced by the
  CLA Assistant workflow.
- CI workflow: `gofmt`, `go vet`, `go test -race` on Linux and macOS, and
  `govulncheck`.
- Release workflow and GoReleaser configuration that publish macOS
  binaries for tagged versions.
- Dependabot updates for GitHub Actions and Go modules.
- Apache License 2.0 (#3).
- Single install method: a templated LaunchAgent installed with
  `make install-agent`, with gateway forwarding required (#2).
- Forwarding of Claude Desktop Code tab and Chat tab activity to the
  gateway ingest endpoint, with redaction, batching, and a disk spool (#1).
- TLS-intercepting proxy with per-host certificates from a local CA,
  JSONL capture of requests, responses, stream units, and WebSocket
  messages, and schema-less protobuf decoding.

### Changed

- The Go module path is now `github.com/Tuskira/claude-desktop-utility`, matching
  the repository name.
