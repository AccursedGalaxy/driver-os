.PHONY: build routing test race vet check deps-clone sandbox-image sandbox-integration install install-council install-driver corpus-baseline corpus-regress swebench swebench-gold fasthttp-ws r1 r2 r3

# Core module hygiene. These cover the Go module only — the embedded
# frontends have their own *-build targets below. `check` is the
# pre-commit bar: vet + full suite under the race detector.
build:
	go build ./...

routing:
	go run ./cmd/routing

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

check: vet race

# Install the council CLI onto PATH (~/.local/bin) so any Claude Code session's
# /council skill can call `council` directly. Re-run after changing cmd/council
# or the council/ packages. See docs/specs/COUNCIL.md and ~/.claude/skills/council/SKILL.md.
install-council:
	GOBIN=$(HOME)/.local/bin go install ./cmd/council
	@echo ">> installed council to $(HOME)/.local/bin/council"

install: install-driver

# Install the unified driver binary and its driver-agent compatibility name.
# If $(HOME)/.local/bin/driver is a SCRIPT (a rebuild-on-invoke dev wrapper,
# not a binary this target made), leave it alone and only refresh the
# driver-agent symlink — the wrapper must use `exec -a "$(basename "$0")"` so
# argv[0] dispatch survives the exec.
install-driver:
	@mkdir -p $(HOME)/.local/bin
	@if [ -f $(HOME)/.local/bin/driver ] && head -c2 $(HOME)/.local/bin/driver | grep -q '#!'; then \
		echo ">> $(HOME)/.local/bin/driver is a dev wrapper script — keeping it (it rebuilds from the tree)"; \
	else \
		go build -o $(HOME)/.local/bin/driver ./cmd/driver; \
	fi
	ln -sfn driver $(HOME)/.local/bin/driver-agent
	@test "$$(readlink -f $(HOME)/.local/bin/driver)" = "$$(readlink -f $(HOME)/.local/bin/driver-agent)"
	@path="$$(command -v driver-agent 2>/dev/null || true)"; \
	if [ -n "$$path" ] && [ "$$path" != "$(HOME)/.local/bin/driver-agent" ]; then \
		echo "warning: driver-agent on PATH shadows $(HOME)/.local/bin/driver-agent"; \
	fi
	@echo ">> installed driver and driver-agent to $(HOME)/.local/bin"

# Print the dogfood corpus baseline (commit-msg verdicts + council convergence/
# signal/still-open residue) from records already on disk — NO model calls, no API
# key. The standing snapshot a harness change is judged against. Redirect to a file
# to commit it: `make corpus-baseline > eval/corpus-baseline.md`.
corpus-baseline:
	@go run ./cmd/corpus-report

# Replay the human-labeled commit-msg corpus through the CURRENT harness and grade
# each new message against the gold the human actually committed — the regression
# sweep. Needs OPENROUTER_API_KEY. MODELS overrides the slug (default: the cheap
# production model). Example: make corpus-regress MODELS=google/gemini-3-flash-preview
corpus-regress:
	go run ./cmd/eval -case=commit-corpus -n=$(or $(N),2) -models=$(or $(MODELS),google/gemini-3-flash-preview)

# Build the trusted base image the docker sandbox backend runs untrusted commands
# in (sh, rg, git, go). Tag matches docker.DefaultImage. Run once (and after any
# Dockerfile change); the daemon caches it thereafter.
sandbox-image:
	docker build -t driver-os-sandbox:latest sandbox/docker

# Run the docker backend's integration tests against a REAL daemon (network
# blocks, path escapes, resource limits, conformance). Requires `make
# sandbox-image` first and a running docker daemon.
sandbox-integration:
	go test -tags docker_integration -count=1 ./sandbox/docker

# SWE-bench Lite sweep (the external benchmark). Defaults to a 1-instance
# slice with one cheap model — every instance pulls its own ~1 GiB official
# image, so widen COUNT/IDS deliberately. Needs OPENROUTER_API_KEY + docker.
# NUDGE = HP-4 near-cap finisher window; default 5 keeps the validated 3-of-30
# proportion at the 50-iter cap (the first roster showed hit_cap-but-PASS runs
# it exists to clean). NUDGE=0 disables.
# Examples:
#   make swebench COUNT=5 N=1 MODELS=deepseek/deepseek-v4-flash
#   make swebench IDS=django__django-11099 MODELS=openai/gpt-5.5
swebench:
	go run ./cmd/eval -case=swebench -n=$(or $(N),1) \
		-swebench-count=$(or $(COUNT),1) -swebench-ids=$(or $(IDS),) \
		-max-iters=$(or $(ITERS),50) -max-wall=$(or $(WALL),30m) -run-timeout=$(or $(RUNTO),10m) \
		-finish-nudge=$(or $(NUDGE),5) \
		-models=$(or $(MODELS),deepseek/deepseek-v4-flash)

# The gold-patch pipeline check: applies the dataset's GOLD patch and asserts
# the oracle grades it resolved (official invariant — validates extraction,
# sandbox, test-spec, parsers, resolution with NO model in the loop). Needs
# docker + network; pulls ~1 GiB per instance on first run.
swebench-gold:
	go test -tags swebench_integration -count=1 -timeout 60m -run TestGoldPatchResolves -v ./eval/suite/swebench/



# ROADMAP §C validation runs (docs/ROADMAP.md). Both prompt with a plan + cost
# estimate before spending; pass Y=1 to skip the prompt. Results land under
# eval/runs/ (gitignored). Needs OPENROUTER_API_KEY (env or .env), jq, git, go.
#
# R1 (~$2, 1.5–3h): gate-only cheap-reviewer block over the 4 banked fasthttp
# patches — answers "were the 2026-07-02 cheap-reviewer deaths plumbing or
# capability?" now that the SSE filter + -review-wall knob shipped.
r1:
	bash eval/scripts/r1.sh $(if $(Y),-y,)

# R2 (~$5–6, hours): fasthttp #2272 head-to-head at N=3 across the four arms —
# error bars for the §5.0 baselines that are all N=1 today.
r2:
	bash eval/scripts/r2.sh $(if $(Y),-y,)

# R3 (~$1.5–2, ~1h): designed tasks 1–3 × {deepseek+review, deepseek-solo} at
# N=5 — error bars on the "near-free QA" claim (§5.3), and re-exercises round
# continuation. No network clones; runs from the eval/fixtures/review-gate/tasks.
r3:
	bash eval/scripts/r3.sh $(if $(Y),-y,)

# Build (or --force rebuild) the pristine fasthttp challenge-workspace template
# the R runs copy per trial. ARGS="--check" also asserts it starts RED.
fasthttp-ws:
	bash eval/scripts/fasthttp-ws.sh $(ARGS)

# Clone/update dependency repos listed in _deps/repos.txt into _deps/ for browsing.
# Missing repos are shallow-cloned; existing ones are fast-forwarded.
deps-clone:
	@while read -r url ref _; do \
		case "$$url" in ''|\#*) continue;; esac; \
		name=$$(basename "$$url" .git); \
		dir="_deps/$$name"; \
		if [ -d "$$dir/.git" ]; then \
			echo ">> updating $$name"; \
			git -C "$$dir" fetch --quiet --depth 1 origin "$${ref:-HEAD}" && \
			git -C "$$dir" checkout --quiet FETCH_HEAD; \
		else \
			echo ">> cloning $$name"; \
			if [ -n "$$ref" ]; then \
				git clone --quiet --depth 1 --branch "$$ref" "$$url" "$$dir" || \
				git clone --quiet --depth 1 "$$url" "$$dir"; \
			else \
				git clone --quiet --depth 1 "$$url" "$$dir"; \
			fi; \
		fi; \
	done < _deps/repos.txt
	@echo "done."
