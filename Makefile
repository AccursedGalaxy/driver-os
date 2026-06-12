.PHONY: deps-clone sandbox-image sandbox-integration install-council corpus-baseline corpus-regress lab lab-build lab-dev vault vault-build vault-dev vault-gen vault-deploy swebench swebench-gold

# Build the Duet Lab frontend (React/Vite) into lab/web/dist, which the Go server
# embeds. Run once, and after any frontend change. Needs node+npm (mise provides
# them). The first run also writes package-lock.json.
lab-build:
	cd lab/web && npm install && npm run build && touch dist/.gitkeep

# Build the duet-lab server binary with the UI embedded.
lab: lab-build
	go build -o bin/duet-lab ./cmd/duet-lab
	@echo ">> built bin/duet-lab — run it with: OPENROUTER_API_KEY=... ./bin/duet-lab"

# Live frontend development: runs BOTH the Go API (:8099) and the Vite dev server
# (which proxies /api + the events WebSocket to it) together, so the API is never
# a stale build. Ctrl-C stops both. Open the URL Vite prints (usually :5173).
# Needs OPENROUTER_API_KEY in the environment or .env for launches to work.
lab-dev:
	cd lab/web && npm install
	@echo ">> Go API on :8099  +  Vite dev server (proxying to it).  Ctrl-C stops both."
	@trap 'kill 0' EXIT INT TERM; \
		go run ./cmd/duet-lab -addr :8099 & \
		cd lab/web && npm run dev

# Build the Vault PWA frontend (React/Vite + service worker) into vault/web/dist,
# which the Go server embeds. Run once, and after any frontend change. Needs
# node+npm (mise provides them). The first run also writes package-lock.json.
vault-build:
	cd vault/web && npm install && npm run build && touch dist/.gitkeep

# Build the vault server binary with the PWA embedded.
vault: vault-build
	go build -o bin/vault ./cmd/vault
	@echo ">> built bin/vault — run it with: ./bin/vault  (then open it on your phone over your LAN and Add to Home Screen)"

# Live frontend development: runs BOTH the Go server (:8097) and the Vite dev
# server (which proxies /api + /healthz to it) together. Ctrl-C stops both. Open
# the URL Vite prints (usually :5173); use --host to reach it from your phone.
vault-dev:
	cd vault/web && npm install
	@echo ">> Go server on :8097  +  Vite dev server (proxying to it).  Ctrl-C stops both."
	@trap 'kill 0' EXIT INT TERM; \
		go run ./cmd/vault -addr :8097 & \
		cd vault/web && npm run dev

# Tier 1: (re)generate the feed from the vault with the cheap model. Pass extra
# flags via ARGS, e.g. `make vault-gen ARGS="-n 30"`. Needs OPENROUTER_API_KEY.
vault-gen:
	go run ./cmd/vault-gen $(ARGS)

# Rebuild the PWA + binaries, reinstall to ~/.local/bin, restart the running
# user service. One-shot redeploy after any change.
vault-deploy: vault-build
	go build -o $$HOME/.local/bin/vault ./cmd/vault
	go build -o $$HOME/.local/bin/vault-gen ./cmd/vault-gen
	systemctl --user restart vault.service
	@echo ">> redeployed — live at http://pegasus:8097"

# Install the council CLI onto PATH (~/.local/bin) so any Claude Code session's
# /council skill can call `council` directly. Re-run after changing cmd/council
# or the council/ packages. See COUNCIL.md and ~/.claude/skills/council/SKILL.md.
install-council:
	GOBIN=$(HOME)/.local/bin go install ./cmd/council
	@echo ">> installed council to $(HOME)/.local/bin/council"

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
