# Contributing

## Ground rules

Go 1.26 or newer. The whole gate is `go build ./...`, `go vet ./...`,
`go test ./...`, and `gofmt -l .` reporting nothing. Run it before opening a PR.

`make hooks` installs the repo's git hooks: a fast secret scan at pre-commit,
and the full gate plus master force-push protection at pre-push. Recommended
for anyone pushing regularly.

Keep PRs small and focused: one behavioral change per PR, with a test that
fails before the change and passes after. Changes without tests will usually
be asked to add one.

For anything nontrivial, open an issue first. The CLI surface is deliberately
frozen (see `docs/specs/PROFILES.md`), so PRs that add new top-level flags are
generally rejected.

## What lives where

This repository is the public core: the agent loop, providers, sandbox,
headless runner, proof bundle, and the gobench validator. Research, evals,
the TUI, and measurement data live in private companion repositories. That is
why some links inside `docs/specs/` point to files that are not published here.

## Reporting bugs

Include the command you ran, the model and provider in use, and the run report
or proof bundle if you have one. A failing reproduction beats a description.
