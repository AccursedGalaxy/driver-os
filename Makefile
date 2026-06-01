.PHONY: deps-clone

# Clone/update dependency repos listed in deps/repos.txt into deps/ for browsing.
# Missing repos are shallow-cloned; existing ones are fast-forwarded.
deps-clone:
	@while read -r url ref _; do \
		case "$$url" in ''|\#*) continue;; esac; \
		name=$$(basename "$$url" .git); \
		dir="deps/$$name"; \
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
	done < deps/repos.txt
	@echo "done."
