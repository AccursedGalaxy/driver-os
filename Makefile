.PHONY: deps-clone sandbox-image sandbox-integration install-council corpus-baseline corpus-regress

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
