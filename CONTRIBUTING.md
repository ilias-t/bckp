# Contributing

Issues and pull requests are welcome.

## Build and test

You need macOS, Go (see `go.mod`) and the Xcode Command Line Tools
(`xcode-select --install`). The thermal reading is a small cgo shim.

```
make build   # bin/backpack, bin/backpackd, bin/bckp -> backpack
make test    # go test -race ./...
make lint    # gofmt check, go vet (and golangci-lint when installed)
make fmt     # gofmt -w -s .
make dist    # universal release tarball and checksums.txt in dist/
```

CI also runs `shellcheck -s sh install.sh`.

To try a build end to end, run `sudo bin/backpack install`. It installs the
`bin/backpackd` next to it, replacing any helper you installed before.
Then run `bin/bckp`. To go back to a release, run
`sudo ~/.local/bin/backpack install`.

When you report a bug, attach the file from `bckp logs --bundle`: the logs
of the last hours, with the config and ntfy topic redacted.

## Tests never touch the real system

backpack changes system-wide power and network settings, so tests must
never do that for real:

- No test may run `pmset`, `networksetup`, `sudo` or `osascript`, or post a
  notification.
- Shell out only through `contracts.Exec`, using the absolute tool paths in
  `internal/paths`.
- Test against recorded output with `fakes.Exec`.
- New fixtures go in `testdata/`. Anonymize them: MAC and IP addresses
  (use `192.0.2.x`/`198.51.100.x`), serials, UUIDs, host and user names.
- The fakes in `internal/fakes` cover every interface in
  `internal/contracts`, plus the clock.
- Tests that bind Unix sockets need a short directory (under `/tmp`):
  macOS socket paths are limited to 103 bytes.

The only exception is the opt-in end-to-end test of the dead-man's switch.
It needs the installed helper and really disables sleep for a moment:

```
BACKPACK_E2E=1 go test ./internal/daemon/client -run E2E -v
```

## Design

See [docs/DESIGN.md](docs/DESIGN.md) for the architecture, the safety model
and the state machines. See
[internal/eventlog/events.md](internal/eventlog/events.md) for the event log.
Keep the root helper (`internal/daemon`) small, and never let client input
reach a command line.
