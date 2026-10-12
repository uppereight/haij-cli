# haij CLI

`haij` is the command-line client and stdio MCP server for [haij](https://app.haij.eu),
the multi-tenant form and workflow platform. It talks to the app over `/api/v1/*`
with a hand-written HTTP client and never boots Rails.

The server half of the API lives in the haij app, not here. This repository is
public from v0.7.0 (2026-10-12); its history starts there.

## Install

```bash
# Homebrew: this repository doubles as the tap
brew tap uppereight/haij-cli https://github.com/uppereight/haij-cli
brew install haij

# Go
go install github.com/uppereight/haij-cli/cmd/haij@latest

# A release archive (darwin/linux × arm64/amd64), checked against SHA256SUMS
gh release download --repo uppereight/haij-cli --pattern "haij-*-darwin-arm64.tar.gz" --pattern SHA256SUMS
```

## Build

```bash
go build -o haij ./cmd/haij
./haij --help
./haij --help --agent   # prints the embedded SKILL.md for agents
```

Release builds stamp the version with `-ldflags "-X main.version=… -X main.commit=…"`.
Without them, `haij --version` reports the module version Go recorded: the tag for
`go install …@v0.1.0`, a pseudo-version for a checkout build.

## Release

Push a `v<semver>` tag on `main`:

```bash
git tag v0.1.0 && git push origin v0.1.0
```

`.github/workflows/release.yml` runs vet and the tests, then `scripts/release.sh`, and
publishes a GitHub release with `haij-<version>-<os>-<arch>.tar.gz` for darwin/linux ×
arm64/amd64, `SHA256SUMS`, and `haij.rb`, the Homebrew formula rendered for those
archives. Commit that formula afterwards:

```bash
gh release download v0.1.0 --pattern haij.rb --output Formula/haij.rb --clobber
```

`scripts/release.sh 0.1.0 "$(git rev-parse --short HEAD)" dist` builds the same set
locally (with different checksums, so never commit a locally rendered formula).

## Test

```bash
gofmt -l -s .    # must print nothing
go vet ./...
go test ./...
```

CI (`.github/workflows/ci.yml`) runs exactly those four steps on every push and PR.

## Use

```bash
haij auth login --api-key haij_xxx --base-url https://app.haij.eu
haij doctor
haij forms list --json
haij mcp serve          # stdio MCP server for Claude Desktop and friends
```

Profiles live in `~/.config/haij/config.json` (`$XDG_CONFIG_HOME` and, for tests,
`$HAIJ_CONFIG_HOME` are honoured). `--profile <name>` switches tenants.

In CI or an agent's sandbox, skip the login: `HAIJ_API_KEY` authenticates every
command without writing `config.json` (`HAIJ_BASE_URL` sets the server, default
`https://app.haij.eu`), and `HAIJ_JSON=1` makes the JSON envelope the default.
Only `--profile` outranks `HAIJ_API_KEY`, so don't leave it exported in a shell
where you want your stored profiles.

## Add-ons

A command `haij` does not know runs an add-on: a program named `haij-<name>` on your
PATH, called as `haij <name> …` with the same arguments (the way `git` and `gh` find
theirs). `--json` given before the command name is passed on, and `--profile` arrives as
`HAIJ_PROFILE`.

## Layout

- `cmd/haij/` — entrypoint and top-level dispatch
- `internal/<name>cmd/` — one package per command family
- `internal/client/` — the REST client
- `internal/mcpserve/`, `internal/mcptools/` — the MCP server and its tool catalogue
- `internal/skill/SKILL.md` — agent-facing usage, embedded into the binary
- `Formula/haij.rb` — Homebrew formula for the latest release, rendered by `scripts/render-formula.sh`
- `scripts/release.sh`, `.github/workflows/release.yml` — the release build
