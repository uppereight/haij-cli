---
name: scoped-tests
description: Run only the Go tests for what you changed in haij-cli instead of the whole suite. Use after editing any file under cmd/ or internal/, before running the full `go test ./...`.
---

# Scoped tests for haij-cli

The full suite takes about 30 seconds, most of it in `internal/listencmd` (polling
timers). Run the package you touched first.

## Pick the package

Tests live next to the code, one package per command family. Map the changed file
to its package directory and run that:

```bash
go test -count=1 ./internal/formscmd
```

Several packages at once:

```bash
go test -count=1 ./internal/formscmd ./internal/entriescmd
```

One test by name (regex on the test function):

```bash
go test -count=1 -run 'TestListen' ./internal/listencmd
```

Repeat a timing-sensitive test to prove it is not flaky:

```bash
go test -count=20 -run 'TestListen' ./internal/listencmd
```

## Packages with no tests

`cmd/haij`, `internal/client`, `internal/envelope`, `internal/catalog`,
`internal/cmdutil`, `internal/mcptools`, `internal/skill`, `internal/linkcmd` have no
test files. A change there is covered only through the command packages that use them,
so run the family that exercises the code path:

- `internal/client` → the `*cmd` package whose endpoint you touched
- `internal/envelope`, `internal/catalog`, `cmd/haij` → `./internal/formscmd`
  and `./internal/mcpcmd` (they assert on envelope shape and dispatch)
- `internal/mcptools` → `./internal/mcpserve`

## Always before a commit

```bash
gofmt -l -s .          # must print nothing
go vet ./...
go test -count=1 ./...
```

Use `-count=1` everywhere: Go caches test results and a cached pass hides a fix
that did not take.

## Isolating config

Tests that touch profiles set `HAIJ_CONFIG_HOME` to a temp directory. Do the same for
any manual run; never point a test at `~/.config/haij/config.json`.
