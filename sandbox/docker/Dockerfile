# The trusted base image for the docker sandbox backend. It is the floor the
# UNTRUSTED command runs on, so it carries only what the agent's toolset shells
# out to — nothing more (a smaller base is a smaller attack surface).
#
# Contents, each justified by a tool that needs it:
#   - sh        the `run` tool execs `sh -c <command>` (agent/tools.go).
#   - rg        the `search` tool execs `rg` DIRECTLY (agent/tools.go) — not via sh.
#   - git       common in `run` commands and the gated DefaultPolicy allowlist.
#   - go        so the agent can `go build`/`go test` in-sandbox.
#
# Pinned by digest (not just the :1.26-alpine tag) so the trusted base can't
# silently change under us. Refresh deliberately: re-pull, copy the new digest.
FROM golang:1.26-alpine@sha256:91eda9776261207ea25fd06b5b7fed8d397dd2c0a283e77f2ab6e91bfa71079d

# ripgrep for the search tool; git for the run tool; coreutils gives a `timeout`
# that supports `-s KILL` (busybox timeout's flag set is thinner) — the Exec
# timeout wrapper uses it. procps gives a real `ps` so process-lifecycle tests can
# observe what's running (busybox `ps` lacks the options).
RUN apk add --no-cache ripgrep git coreutils procps

# Go cache locations MUST be writable, but the root filesystem is mounted
# --read-only and the default GOPATH cache (/go/pkg/mod) lives ON it — so leaving
# the defaults makes any in-container `go build` fail with "could not create
# module cache: read-only file system". Point BOTH caches at the writable tmpfs
# the backend mounts at /tmp, so the toolchain works out of the box:
#   - GOMODCACHE=/tmp/...  downloaded modules (network-on) or a fail-fast miss
#                          (network-off) — never the read-only /go/pkg/mod.
#   - GOCACHE=/tmp/...     the build cache; per-container and ephemeral.
# For the network-off module story (§13), mount a host cache READ-ONLY and point
# GOMODCACHE at it via docker.Options.ExtraEnv — don't mount the host cache
# writable (poisoning risk).
#   - GOFLAGS=-mod=mod     use the cache without demanding a vendor dir.
# GOPROXY is left at its default so a network-ON build can fetch; with
# --network none a fetch simply fails fast (no route), which is the intended
# containment.
ENV GOMODCACHE=/tmp/gomodcache \
    GOCACHE=/tmp/gocache \
    GOFLAGS=-mod=mod \
    CGO_ENABLED=0

# A non-root user exists in the image as defense-in-depth, but the backend ALSO
# passes --user <host-uid:gid> at run time (so bind-mount writes are host-owned).
# The workspace is the working directory; the backend sets -w to it per Exec too.
WORKDIR /workspace
