BIN_DIR := bin
CLI_BIN := $(BIN_DIR)/opensearch-cli
VERSION ?= 0.1.0
INSTALL_BIN_DIR ?= $(HOME)/.local/bin
SKILL_SRC := skills/opensearch
SKILL_NAME := opensearch
SKILL_SOURCE ?= $(CURDIR)
SKILLS_CLI ?= npx --yes skills
SKILL_AGENTS ?= claude-code codex opencode trae trae-cn
SKILL_AGENT_FLAGS = $(foreach agent,$(SKILL_AGENTS),--agent "$(agent)")
LEGACY_CODEX_SKILL_DIR ?= $(HOME)/.codex/skills/$(SKILL_NAME)

.PHONY: build test smoke smoke-exa smoke-codex-exec smoke-strict clean install install-cli install-skill install-skill-copy install-skill-all install-skill-list remove-legacy-codex-skill

build:
	mkdir -p $(BIN_DIR)
	go build -ldflags "-X main.version=$(VERSION)" -o $(CLI_BIN) ./cmd/opensearch-cli

test:
	go test ./...

smoke: test build
	$(CLI_BIN) --version
	$(CLI_BIN) --help >/dev/null
	@tmp=$$(mktemp -d .opensearch-smoke.XXXXXX); \
	tmp=$$(cd "$$tmp" && pwd); \
	trap 'rm -rf "$$tmp"' EXIT; \
	env -u EXA_API_KEY $(CLI_BIN) search -n 1 "北京今天天气" >"$$tmp/search-no-key.json"; \
	python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d["success"] is True; assert d["metadata"]["command"] == "search"; assert "results" in d["data"]' "$$tmp/search-no-key.json"; \
	$(CLI_BIN) scrape https://example.com >"$$tmp/scrape-example.json"; \
	python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); r=d["data"]["results"][0]; assert d["success"] is True; assert r["success"] is True; assert r["finalUrl"] == "https://example.com"; assert "Example Domain" in r.get("title", ""); assert "Example Domain" in r.get("content", "")' "$$tmp/scrape-example.json"; \
	$(CLI_BIN) scrape https://example.com http://127.0.0.1 >"$$tmp/scrape-partial-failure.json"; \
	python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); rs=d["data"]["results"]; assert d["success"] is True; assert len(rs) == 2; assert rs[0]["success"] is True; assert rs[1]["success"] is False; assert rs[1]["error"]["code"] == "SSRF_BLOCKED"' "$$tmp/scrape-partial-failure.json"; \
	$(CLI_BIN) scrape -o "$$tmp/scrape-full.json" https://example.com >"$$tmp/scrape-summary.json"; \
	python3 -c 'import json,sys,os; s=json.load(open(sys.argv[1])); f=json.load(open(sys.argv[2])); assert s["success"] is True; assert s["metadata"]["contentOmitted"] is True; assert s["metadata"]["outputPath"] == os.path.abspath(sys.argv[2]); assert f["success"] is True; assert not f["metadata"].get("contentOmitted", False); assert f["metadata"]["outputPath"] == os.path.abspath(sys.argv[2]); assert "Example Domain" in f["data"]["results"][0].get("content", "")' "$$tmp/scrape-summary.json" "$$tmp/scrape-full.json"; \
	if command -v codex >/dev/null 2>&1; then \
		mkdir -p "$$tmp/codex/skills"; \
		cp -R "$(SKILL_SRC)" "$$tmp/codex/skills/opensearch"; \
		CODEX_HOME="$$tmp/codex" codex debug prompt-input "请用 opensearch 读取 https://example.com" >"$$tmp/codex-prompt.json"; \
		python3 -c 'import json,sys; s=open(sys.argv[1]).read(); json.loads(s); assert "- opensearch:" in s and "/skills/opensearch/SKILL.md" in s' "$$tmp/codex-prompt.json"; \
	else \
		printf 'codex not found; skip skill runtime visibility smoke\n'; \
	fi; \
	$(CLI_BIN) search -n 1 "opensearch smoke" >"$$tmp/search-real.json"; \
	python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d["success"] is True; assert d["metadata"]["command"] == "search"; assert "results" in d["data"]' "$$tmp/search-real.json"

smoke-exa: build
	@tmp=$$(mktemp -d .opensearch-exa-smoke.XXXXXX); \
	tmp=$$(cd "$$tmp" && pwd); \
	trap 'rm -rf "$$tmp"' EXIT; \
	env -u EXA_API_KEY $(CLI_BIN) search -n 1 "opensearch smoke" >"$$tmp/search-real.json"; \
	python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d["success"] is True; assert d["metadata"]["command"] == "search"; assert "results" in d["data"]' "$$tmp/search-real.json"

smoke-codex-exec: build
	@command -v codex >/dev/null 2>&1 || { printf 'codex is required for smoke-codex-exec\n' >&2; exit 2; }
	@tmp=$$(mktemp -d .opensearch-codex-smoke.XXXXXX); \
	tmp=$$(cd "$$tmp" && pwd); \
	trap 'rm -rf "$$tmp"' EXIT; \
	mkdir -p "$$tmp/codex/skills"; \
	cp -R "$(SKILL_SRC)" "$$tmp/codex/skills/opensearch"; \
	CODEX_HOME="$$tmp/codex" PATH="$(CURDIR)/$(BIN_DIR):$$PATH" codex exec --cd "$(CURDIR)" --sandbox workspace-write --output-last-message "$$tmp/codex-last.txt" "请使用 opensearch skill 读取 https://example.com。只输出页面标题，不要输出其他内容。" >"$$tmp/codex-exec.log"; \
	python3 -c 'import sys; s=open(sys.argv[1]).read(); assert "Example Domain" in s, s' "$$tmp/codex-last.txt"

smoke-strict: smoke smoke-exa smoke-codex-exec

install: install-cli install-skill

install-cli: build
	mkdir -p $(INSTALL_BIN_DIR)
	cp $(CLI_BIN) $(INSTALL_BIN_DIR)/opensearch-cli
	@installed="$$(cd "$(INSTALL_BIN_DIR)" && pwd)/opensearch-cli"; \
	found="$$(command -v opensearch-cli 2>/dev/null || true)"; \
	if test "$$found" = "$$installed"; then \
		opensearch-cli --version; \
		opensearch-cli --help >/dev/null; \
	else \
		printf 'warning: %s is installed but not the opensearch-cli found on PATH; add %s to PATH before using the skill\n' "$$installed" "$$(cd "$(INSTALL_BIN_DIR)" && pwd)" >&2; \
	fi

install-skill: 
	$(SKILLS_CLI) add "$(SKILL_SOURCE)" -g --skill "$(SKILL_NAME)" $(SKILL_AGENT_FLAGS) -y --full-depth
	$(MAKE) remove-legacy-codex-skill
	$(SKILLS_CLI) ls -g --json

install-skill-copy: 
	$(SKILLS_CLI) add "$(SKILL_SOURCE)" -g --skill "$(SKILL_NAME)" $(SKILL_AGENT_FLAGS) -y --full-depth --copy
	$(MAKE) remove-legacy-codex-skill
	$(SKILLS_CLI) ls -g --json

install-skill-all:
	$(MAKE) install-skill SKILL_AGENTS='*'

install-skill-list:
	$(SKILLS_CLI) add "$(SKILL_SOURCE)" --list --full-depth

remove-legacy-codex-skill:
	@if test -L "$(LEGACY_CODEX_SKILL_DIR)"; then \
		rm -f "$(LEGACY_CODEX_SKILL_DIR)"; \
	elif test -d "$(LEGACY_CODEX_SKILL_DIR)" && test -f "$(LEGACY_CODEX_SKILL_DIR)/SKILL.md" && grep -q '^name: $(SKILL_NAME)$$' "$(LEGACY_CODEX_SKILL_DIR)/SKILL.md"; then \
		rm -rf "$(LEGACY_CODEX_SKILL_DIR)"; \
	elif test -e "$(LEGACY_CODEX_SKILL_DIR)"; then \
		printf 'warning: %s exists but is not a %s skill; leaving unchanged\n' "$(LEGACY_CODEX_SKILL_DIR)" "$(SKILL_NAME)" >&2; \
	fi

clean:
	rm -rf $(BIN_DIR)
