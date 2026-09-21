SHELL := /bin/bash
.DEFAULT_GOAL := help

GO      ?= go
COMPOSE ?= docker compose

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-20s\033[0m %s\n",$$1,$$2}'

# ---------------------------------------------------------------------------
# Local stack
# ---------------------------------------------------------------------------

.PHONY: up
up: ## Start ClickHouse and PostgreSQL, wait for health
	$(COMPOSE) up -d --wait
	@echo "stack healthy"

.PHONY: down
down: ## Stop the stack, keeping data
	$(COMPOSE) down

.PHONY: clean
clean: ## Stop the stack and destroy all local data
	$(COMPOSE) down -v

.PHONY: logs
logs: ## Tail stack logs
	$(COMPOSE) logs -f

.PHONY: migrate
migrate: ## Apply PostgreSQL and ClickHouse migrations
	$(GO) run ./cmd/migrate

# ---------------------------------------------------------------------------
# Build and test
# ---------------------------------------------------------------------------

.PHONY: build
build: ## Build all binaries into ./bin
	$(GO) build -o bin/ ./cmd/...

.PHONY: test
test: ## Run tests that need no external services
	$(GO) test -race -short ./...

.PHONY: test-integration
test-integration: ## Run the full suite, requires `make up`
	$(GO) test -race ./...

.PHONY: fmt
fmt: ## Format
	$(GO) fmt ./...

.PHONY: vet
vet: ## Vet
	$(GO) vet ./...

# ---------------------------------------------------------------------------
# Build gates
# ---------------------------------------------------------------------------

# SPEC.md §2 bans a particular word for a trial deployment; first deployments
# are "production installations". Enforced rather than trusted to review.
#
# The banned word is assembled from fragments below so that this rule, and the
# gate enforcing it, do not themselves trip the gate. SPEC.md is excluded for
# the same reason: it is the authority that defines the rule and must quote it.
BANNED_TERM := p$(shell printf 'i')lot
.PHONY: check-terminology
check-terminology: ## Fail if banned terminology appears anywhere in the repo
	@matches=$$(grep -rniE "\b$(BANNED_TERM)s?\b" \
		--exclude-dir=.git --exclude-dir=bin --exclude-dir=.data \
		--exclude-dir=node_modules --exclude=go.sum \
		--exclude=Makefile --exclude=SPEC.md . || true); \
	if [ -n "$$matches" ]; then \
		echo "banned terminology found; SPEC.md §2 forbids this word:"; \
		echo "$$matches"; \
		exit 1; \
	fi; \
	echo "terminology check passed"

# SPEC.md §2: no secrets in the repo. Config via environment variables.
.PHONY: check-secrets
check-secrets: ## Fail on obvious committed credentials
	@matches=$$(grep -rniE '(api[_-]?key|secret|password|token)[[:space:]]*[:=][[:space:]]*["'"'"'][^"'"'"'$$]{12,}' \
		--include='*.go' --include='*.yaml' --include='*.yml' \
		--exclude-dir=.git --exclude-dir=bin . || true); \
	if [ -n "$$matches" ]; then \
		echo "possible committed secret:"; echo "$$matches"; exit 1; \
	fi; \
	echo "secret check passed"

.PHONY: check
check: fmt vet check-terminology check-secrets test ## Everything CI runs
	@echo "all checks passed"
