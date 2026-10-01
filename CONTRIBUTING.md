# Contributing to Claude Desktop Utility

Thanks for your interest in contributing. This guide covers how to set up a
development environment, how pull requests work, and the Contributor License
Agreement every contributor signs once.

Please read and follow the [Code of Conduct](CODE_OF_CONDUCT.md). Report
security issues privately as described in [SECURITY.md](SECURITY.md), never
in a public issue.

## Dev setup

You need Go (the version in `go.mod`) and `make`. The project uses the Go
standard library only, so there are no dependencies to install.

```sh
git clone https://github.com/Tuskira/calude-desktop-utility.git claude-desktop-utility
cd claude-desktop-utility
make check   # go vet + go test
make build   # builds bin/interceptor
```

To run it against a real application, follow
[deploy/macos/README.md](deploy/macos/README.md).

## `make` targets

| Target | What it does |
|---|---|
| `make build` | Builds `bin/interceptor` |
| `make test` | Runs the tests |
| `make vet` | Runs `go vet` |
| `make fmt` | Formats the code with `gofmt` |
| `make check` | `vet` and `test` |
| `make clean` | Removes build output |
| `make install-agent` | Installs and starts the macOS background service |
| `make uninstall-agent` | Stops and removes the macOS background service |

## Branch and pull request conventions

- Branch from `main` and open a pull request against `main`.
- Name branches by intent: `feat/…`, `fix/…`, `docs/…`, `chore/…`.
- Write commit subjects in the imperative, using the same prefixes,
  for example `fix(forward): retry on 503`.
- Keep pull requests focused. Add or update tests for any behavior change,
  and update `README.md` or `deploy/macos/README.md` when behavior or
  flags change.
- Add a line to the `[Unreleased]` section of [CHANGELOG.md](CHANGELOG.md).
- Never commit keys, certificates, captured traffic, or real identifiers
  from a capture. Use made-up values in tests and fixtures.
- CI (see below) must pass before a pull request is merged.

## Contributor License Agreement

Every contributor signs a Contributor License Agreement (CLA) once, before
their first pull request is merged. We use a **license-grant** CLA, not a
copyright assignment: you keep ownership of your contribution. What you
grant Tuskira is a broad, permanent license to use it — including the
right to sublicense it and to offer it later under different license
terms (for example, as part of a commercial or hosted edition), alongside
the project's continuing open-source availability to everyone else under
its current license. See [.github/cla/INDIVIDUAL_CLA.md](.github/cla/INDIVIDUAL_CLA.md)
for the full terms, including what this means for your patents.

**Why a CLA at all, when the project is Apache-2.0?** Apache-2.0 already
gives every user a copyright and patent license to Contributions. What the
CLA adds is Tuskira's own explicit right to relicense (so the project can
later ship a commercial or otherwise differently-licensed edition without
having to track down every past contributor individually), your written
representation that the contribution is yours to give, and — if your
employer owns your work — your employer's sign-off via the Corporate CLA.

**Signing as an individual.** Open your pull request as usual. On your
first pull request, the CLA Assistant bot comments with a link to
[.github/cla/INDIVIDUAL_CLA.md](.github/cla/INDIVIDUAL_CLA.md) and asks
you to reply with a fixed line of text to sign. You only need to do this
once; it covers this and future pull requests.

**Signing as a company.** If your employer owns the intellectual property
in your contributions, a person authorized to bind the company signs the
[Corporate CLA](.github/cla/CORPORATE_CLA.md) instead (or in addition),
listing which employees are authorized to contribute on the company's
behalf (its "Schedule A"), and emails it to the contact address in
[NOTICE](NOTICE). Each authorized employee then still signs pull requests
individually through the bot, as their employer's representative under
the Corporate CLA — update the company's Schedule A at that same address
when the authorized list changes.

Either way, your contribution remains available to everyone else under
this project's current open-source license — see [LICENSE](LICENSE). The
CLA only adds rights that Tuskira, specifically, also holds.

## Release process

Releases are cut from `main` by pushing a version tag:

```sh
git tag v0.1.0
git push origin v0.1.0
```

The [release workflow](.github/workflows/release.yml) runs GoReleaser
([.goreleaser.yaml](.goreleaser.yaml)), which builds macOS binaries for
Intel and Apple silicon and publishes them with checksums on the GitHub
Releases page. Move the `[Unreleased]` entries in `CHANGELOG.md` under the
new version before tagging.

## CI

Every pull request and every push to `main` runs
[.github/workflows/ci.yml](.github/workflows/ci.yml):

- `gofmt` and `go vet`
- `go test -race` on Linux and macOS, plus `make build`
- `govulncheck`

The [CLA workflow](.github/workflows/cla.yml) also runs on every pull
request and checks that each author has signed the CLA.
