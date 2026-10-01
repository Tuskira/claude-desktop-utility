# Security Policy

## Supported versions

Claude Desktop Utility is pre-1.0. Security fixes land on `main` and in the next
release; there is no long-term-support branch.

## Reporting a vulnerability

Email **security@tuskira.ai** with a description of the issue, the
affected version or commit, and reproduction steps if you have them. Do
not open a public GitHub issue for a security report.

Once this repository is public, you can instead use GitHub's private
vulnerability reporting (Security -> Report a vulnerability). It is not
available while the repository is private, so email remains the channel
until then.

## Response targets

- **Acknowledgement:** within 3 business days of your report.
- **Triage and severity assessment:** within 10 business days, with a plan
  and an expected timeline.
- **Fix target:** 30 days for a high-severity issue, from confirmation to
  a released fix or documented mitigation. Lower-severity issues are
  scheduled case by case; we'll tell you the plan once we've assessed
  impact.

We will keep you updated at least every 2 weeks until the report is closed.

## Handling sensitive material

These files hold sensitive data, and reports about how they are created,
stored, or exposed are in scope:

- `~/.interceptor/ca-key.pem`, the local certificate authority's private
  key, written with mode 0600.
- `~/.interceptor/gateway.key`, the gateway key. The interceptor refuses to
  start unless this file is mode 0600.
- The capture file (by default `~/claude-capture.jsonl`) and the forwarding
  spool (`~/.interceptor/spool`), which can hold captured request and
  response content.

## Coordinated disclosure

We ask that you give us the chance to investigate and release a fix (or a
mitigation) before any public disclosure. We'll credit reporters who want
credit, in the release notes of the fix, once it ships.

## Scope

In scope: the `interceptor` binary (`main.go`, `internal/`) and the
background-service install files (`deploy/`, `Makefile`).

Out of scope: vulnerabilities in the Tuskira gateway itself (report those
to the [AI Agent Gateway](https://github.com/Tuskira/ai-agent-gateway)
project), in third-party applications whose traffic you route through the
interceptor, and social-engineering or physical-access attacks.
