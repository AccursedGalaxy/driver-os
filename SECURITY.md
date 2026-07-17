# Security Policy

## Reporting a vulnerability

Do not open public issues for security problems. Report them privately via
[GitHub security advisories](https://github.com/AccursedGalaxy/driver-os/security/advisories/new).

You should get a response within a few days. There is no bug bounty.

## Scope notes

driver-os executes model-chosen commands. The security-relevant surfaces are
the sandbox (`sandbox/`) and the worktree isolation layer. Escapes from either
are in scope: a delegated agent writing outside its worktree, or command
execution that bypasses the configured sandbox. Prompt injection that makes a
model produce bad code is model behavior, not a vulnerability in this harness.

## Supported versions

Only the latest release and `master` receive fixes.
