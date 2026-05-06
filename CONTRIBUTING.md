# Contributing

Thanks for your interest in `incidentary/opentelemetry-collector-incidentary`
— the open-source Bridge that wraps the Incidentary processor in a standalone
OTel-collector binary.

This repo is Apache-2.0 and accepts external contributions. The companion
backend that consumes Bridge output is closed-source and lives elsewhere; if
you want to discuss a change that crosses that boundary, open an issue first.

## Ground rules

- **Discuss before you build.** For anything more than a typo, open an issue
  describing the problem and the rough approach. Maintainers will tell you
  fast if it's in scope.
- **One PR, one concern.** Don't bundle a refactor with a feature with a docs
  pass. We'll ask you to split it.
- **Tests are not optional.** New behavior comes with tests in the same PR.
  Bug fixes come with a regression test that fails before the fix and passes
  after.
- **Match the existing style.** Code formatting follows `go fmt` and
  `golangci-lint run`; chart templates follow the conventions already in
  `charts/incidentary-bridge/templates/`. Don't drift.
- **No new dependencies without justification.** Anything beyond
  `go.opentelemetry.io/collector/*` and the Go standard library needs a
  one-paragraph rationale in the PR description.

## Development setup

```bash
git clone https://github.com/incidentary/opentelemetry-collector-incidentary.git
cd opentelemetry-collector-incidentary

# Build + test
make build
make test

# Lint
make vet
make lint

# Helm chart
make helm-lint
make helm-template

# Container image (single-arch local)
make docker-build
```

Go version: see `go.mod`. CI tests against the version pinned there.

## Pull request checklist

Before requesting review:

- [ ] `make vet` passes
- [ ] `make lint` passes
- [ ] `make test` passes
- [ ] If touching `charts/**`, `make helm-lint` and `make helm-template` pass
- [ ] New behavior has tests
- [ ] Commit message follows conventional commits (`feat:`, `fix:`, `docs:`,
  `test:`, `chore:`, `refactor:`, `perf:`, `ci:`)
- [ ] PR description references the issue it closes (or the plan section)

## What does NOT belong in this repo

- Backend changes — this repo is the OSS processor only.
- Customer-specific configuration — the chart ships the same defaults for
  everyone; per-customer config is `--set` flags.
- Vendor-specific exporters — the Bridge forwards OTLP to whatever
  `forward.endpoint` you point it at. We won't add vendor-specific exporter
  code paths.

## Reporting security issues

Do not file a public GitHub issue for a security report. See
[SECURITY.md](./SECURITY.md) for the disclosure process.

## Code of conduct

This project follows the [Contributor Covenant Code of
Conduct](./CODE_OF_CONDUCT.md). Be excellent to each other.
