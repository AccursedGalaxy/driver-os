.PHONY: deps-clone sandbox-image sandbox-integration

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
