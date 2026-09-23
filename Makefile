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
# Daily refresh (scripts/daily.sh), scheduled with launchd
# ---------------------------------------------------------------------------

DAILY_HOUR   ?= 3
DAILY_MINUTE ?= 0
DAILY_LABEL  := com.tether-risk.daily
DAILY_PLIST  := $(HOME)/Library/LaunchAgents/$(DAILY_LABEL).plist

.PHONY: daily
daily: ## Run the daily refresh now
	scripts/daily.sh

.PHONY: daily-install
daily-install: ## Schedule the daily refresh at DAILY_HOUR:DAILY_MINUTE (default 03:00)
	@mkdir -p $(HOME)/Library/LaunchAgents .data/logs
	@printf '%s\n' \
		'<?xml version="1.0" encoding="UTF-8"?>' \
		'<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' \
		'<plist version="1.0"><dict>' \
		'  <key>Label</key><string>$(DAILY_LABEL)</string>' \
		'  <key>ProgramArguments</key><array><string>/bin/bash</string><string>$(CURDIR)/scripts/daily.sh</string></array>' \
		'  <key>WorkingDirectory</key><string>$(CURDIR)</string>' \
		'  <key>StartCalendarInterval</key><dict>' \
		'    <key>Hour</key><integer>$(DAILY_HOUR)</integer>' \
		'    <key>Minute</key><integer>$(DAILY_MINUTE)</integer>' \
		'  </dict>' \
		'  <key>StandardOutPath</key><string>$(CURDIR)/.data/logs/launchd.log</string>' \
		'  <key>StandardErrorPath</key><string>$(CURDIR)/.data/logs/launchd.log</string>' \
		'</dict></plist>' > $(DAILY_PLIST)
	@launchctl bootout gui/$$(id -u)/$(DAILY_LABEL) 2>/dev/null || true
	@launchctl bootstrap gui/$$(id -u) $(DAILY_PLIST)
	@printf 'daily refresh scheduled at %02d:%02d; logs in .data/logs/\n' $(DAILY_HOUR) $(DAILY_MINUTE)

.PHONY: daily-uninstall
daily-uninstall: ## Remove the daily schedule
	@launchctl bootout gui/$$(id -u)/$(DAILY_LABEL) 2>/dev/null || true
	@rm -f $(DAILY_PLIST)
	@echo "daily refresh unscheduled"

.PHONY: daily-status
daily-status: ## Show the schedule and the latest run's summary
	@launchctl print gui/$$(id -u)/$(DAILY_LABEL) 2>/dev/null | grep -E 'state|last exit|Hour|Minute' || echo "not scheduled"
	@latest=$$(ls -t .data/logs/daily-*.log 2>/dev/null | head -1); \
		if [ -n "$$latest" ]; then echo "--- $$latest"; grep -E '^[0-9-]+ [0-9:]+ (ok|FAIL|===)' "$$latest"; \
		else echo "no runs yet"; fi

# ---------------------------------------------------------------------------
# Tether blacklist refresh (scripts/tether.sh), every TETHER_INTERVAL seconds
# ---------------------------------------------------------------------------

TETHER_INTERVAL ?= 600
TETHER_LABEL    := com.tether-risk.tether
TETHER_PLIST    := $(HOME)/Library/LaunchAgents/$(TETHER_LABEL).plist

.PHONY: tether-install
tether-install: ## Refresh Tether's blacklist every TETHER_INTERVAL seconds (default 600)
	@mkdir -p $(HOME)/Library/LaunchAgents .data/logs
	@printf '%s\n' \
		'<?xml version="1.0" encoding="UTF-8"?>' \
		'<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' \
		'<plist version="1.0"><dict>' \
		'  <key>Label</key><string>$(TETHER_LABEL)</string>' \
		'  <key>ProgramArguments</key><array><string>/bin/bash</string><string>$(CURDIR)/scripts/tether.sh</string></array>' \
		'  <key>WorkingDirectory</key><string>$(CURDIR)</string>' \
		'  <key>RunAtLoad</key><true/>' \
		'  <key>StartInterval</key><integer>$(TETHER_INTERVAL)</integer>' \
		'  <key>StandardOutPath</key><string>$(CURDIR)/.data/logs/tether.log</string>' \
		'  <key>StandardErrorPath</key><string>$(CURDIR)/.data/logs/tether.log</string>' \
		'</dict></plist>' > $(TETHER_PLIST)
	@launchctl bootout gui/$$(id -u)/$(TETHER_LABEL) 2>/dev/null || true
	@while launchctl print gui/$$(id -u)/$(TETHER_LABEL) >/dev/null 2>&1; do sleep 1; done
	@launchctl bootstrap gui/$$(id -u) $(TETHER_PLIST)
	@echo "tether blacklist refresh every $(TETHER_INTERVAL)s; log in .data/logs/tether.log"

.PHONY: tether-uninstall
tether-uninstall: ## Remove the blacklist refresh schedule
	@launchctl bootout gui/$$(id -u)/$(TETHER_LABEL) 2>/dev/null || true
	@rm -f $(TETHER_PLIST)
	@echo "tether blacklist refresh removed"

# ---------------------------------------------------------------------------
# Ingest worker (scripts/worker.sh), kept alive by launchd
# ---------------------------------------------------------------------------

WORKER_LABEL := com.tether-risk.worker
WORKER_PLIST := $(HOME)/Library/LaunchAgents/$(WORKER_LABEL).plist

.PHONY: worker-install
worker-install: ## Run the ingest worker permanently, restarting it if it stops
	@mkdir -p $(HOME)/Library/LaunchAgents .data/logs
	@printf '%s\n' \
		'<?xml version="1.0" encoding="UTF-8"?>' \
		'<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' \
		'<plist version="1.0"><dict>' \
		'  <key>Label</key><string>$(WORKER_LABEL)</string>' \
		'  <key>ProgramArguments</key><array><string>/bin/bash</string><string>$(CURDIR)/scripts/worker.sh</string></array>' \
		'  <key>WorkingDirectory</key><string>$(CURDIR)</string>' \
		'  <key>RunAtLoad</key><true/>' \
		'  <key>KeepAlive</key><true/>' \
		'  <key>ThrottleInterval</key><integer>60</integer>' \
		'  <key>StandardOutPath</key><string>$(CURDIR)/.data/logs/worker.log</string>' \
		'  <key>StandardErrorPath</key><string>$(CURDIR)/.data/logs/worker.log</string>' \
		'</dict></plist>' > $(WORKER_PLIST)
	@launchctl bootout gui/$$(id -u)/$(WORKER_LABEL) 2>/dev/null || true
	@# bootout returns before the service is gone; bootstrapping too soon fails with error 5.
	@while launchctl print gui/$$(id -u)/$(WORKER_LABEL) >/dev/null 2>&1; do sleep 1; done
	@launchctl bootstrap gui/$$(id -u) $(WORKER_PLIST)
	@echo "ingest worker running under launchd; log in .data/logs/worker.log"

.PHONY: worker-uninstall
worker-uninstall: ## Stop and remove the permanent ingest worker
	@launchctl bootout gui/$$(id -u)/$(WORKER_LABEL) 2>/dev/null || true
	@rm -f $(WORKER_PLIST)
	@echo "ingest worker removed"

# ---------------------------------------------------------------------------
# Screening API (scripts/api.sh), kept alive by launchd
# ---------------------------------------------------------------------------

API_LABEL := com.tether-risk.api
API_PLIST := $(HOME)/Library/LaunchAgents/$(API_LABEL).plist

.PHONY: api-install
api-install: ## Run the API permanently on API_ADDR (default 127.0.0.1:8099), restarting it if it stops
	@mkdir -p $(HOME)/Library/LaunchAgents .data/logs
	@printf '%s\n' \
		'<?xml version="1.0" encoding="UTF-8"?>' \
		'<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' \
		'<plist version="1.0"><dict>' \
		'  <key>Label</key><string>$(API_LABEL)</string>' \
		'  <key>ProgramArguments</key><array><string>/bin/bash</string><string>$(CURDIR)/scripts/api.sh</string></array>' \
		'  <key>WorkingDirectory</key><string>$(CURDIR)</string>' \
		'  <key>RunAtLoad</key><true/>' \
		'  <key>KeepAlive</key><true/>' \
		'  <key>ThrottleInterval</key><integer>60</integer>' \
		'  <key>StandardOutPath</key><string>$(CURDIR)/.data/logs/api.log</string>' \
		'  <key>StandardErrorPath</key><string>$(CURDIR)/.data/logs/api.log</string>' \
		'</dict></plist>' > $(API_PLIST)
	@launchctl bootout gui/$$(id -u)/$(API_LABEL) 2>/dev/null || true
	@# bootout returns before the service is gone; bootstrapping too soon fails with error 5.
	@while launchctl print gui/$$(id -u)/$(API_LABEL) >/dev/null 2>&1; do sleep 1; done
	@launchctl bootstrap gui/$$(id -u) $(API_PLIST)
	@echo "api running under launchd; log in .data/logs/api.log"

.PHONY: api-uninstall
api-uninstall: ## Stop and remove the permanent API
	@launchctl bootout gui/$$(id -u)/$(API_LABEL) 2>/dev/null || true
	@rm -f $(API_PLIST)
	@echo "api removed"

# ---------------------------------------------------------------------------
# TRON index (scripts/indexer.sh): the tail and the query API, kept alive by launchd
# ---------------------------------------------------------------------------

INDEXER_MODES := tail serve

.PHONY: indexer-install
indexer-install: ## Run the index tail and its API (127.0.0.1:8098) permanently
	@mkdir -p $(HOME)/Library/LaunchAgents .data/logs
	@for m in $(INDEXER_MODES); do \
		label=com.tether-risk.indexer-$$m; plist=$(HOME)/Library/LaunchAgents/$$label.plist; \
		printf '%s\n' \
			'<?xml version="1.0" encoding="UTF-8"?>' \
			'<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' \
			'<plist version="1.0"><dict>' \
			"  <key>Label</key><string>$$label</string>" \
			"  <key>ProgramArguments</key><array><string>/bin/bash</string><string>$(CURDIR)/scripts/indexer.sh</string><string>$$m</string></array>" \
			'  <key>WorkingDirectory</key><string>$(CURDIR)</string>' \
			'  <key>RunAtLoad</key><true/>' \
			'  <key>KeepAlive</key><true/>' \
			'  <key>ThrottleInterval</key><integer>60</integer>' \
			"  <key>StandardOutPath</key><string>$(CURDIR)/.data/logs/indexer-$$m.log</string>" \
			"  <key>StandardErrorPath</key><string>$(CURDIR)/.data/logs/indexer-$$m.log</string>" \
			'</dict></plist>' > $$plist; \
		launchctl bootout gui/$$(id -u)/$$label 2>/dev/null || true; \
		while launchctl print gui/$$(id -u)/$$label >/dev/null 2>&1; do sleep 1; done; \
		launchctl bootstrap gui/$$(id -u) $$plist; \
	done
	@echo "indexer tail and API running under launchd; logs in .data/logs/indexer-*.log"

.PHONY: indexer-uninstall
indexer-uninstall: ## Stop and remove the index tail and its API
	@for m in $(INDEXER_MODES); do \
		launchctl bootout gui/$$(id -u)/com.tether-risk.indexer-$$m 2>/dev/null || true; \
		rm -f $(HOME)/Library/LaunchAgents/com.tether-risk.indexer-$$m.plist; \
	done
	@echo "indexer removed"

# ---------------------------------------------------------------------------
# Telegram bot (scripts/bot.sh), kept alive by launchd
# ---------------------------------------------------------------------------

BOT_LABEL := com.tether-risk.bot
BOT_PLIST := $(HOME)/Library/LaunchAgents/$(BOT_LABEL).plist

.PHONY: bot-install
bot-install: ## Run the Telegram bot permanently against the API, restarting it if it stops
	@grep -q '^TELEGRAM_BOT_TOKEN=.' .env 2>/dev/null || { echo "TELEGRAM_BOT_TOKEN missing from .env"; exit 1; }
	@grep -q '^BILLING_SUPPORT_CONTACT=.' .env 2>/dev/null || { echo "BILLING_SUPPORT_CONTACT missing from .env"; exit 1; }
	@mkdir -p $(HOME)/Library/LaunchAgents .data/logs
	@printf '%s\n' \
		'<?xml version="1.0" encoding="UTF-8"?>' \
		'<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' \
		'<plist version="1.0"><dict>' \
		'  <key>Label</key><string>$(BOT_LABEL)</string>' \
		'  <key>ProgramArguments</key><array><string>/bin/bash</string><string>$(CURDIR)/scripts/bot.sh</string></array>' \
		'  <key>WorkingDirectory</key><string>$(CURDIR)</string>' \
		'  <key>RunAtLoad</key><true/>' \
		'  <key>KeepAlive</key><true/>' \
		'  <key>ThrottleInterval</key><integer>60</integer>' \
		'  <key>StandardOutPath</key><string>$(CURDIR)/.data/logs/bot.log</string>' \
		'  <key>StandardErrorPath</key><string>$(CURDIR)/.data/logs/bot.log</string>' \
		'</dict></plist>' > $(BOT_PLIST)
	@launchctl bootout gui/$$(id -u)/$(BOT_LABEL) 2>/dev/null || true
	@# bootout returns before the service is gone; bootstrapping too soon fails with error 5.
	@while launchctl print gui/$$(id -u)/$(BOT_LABEL) >/dev/null 2>&1; do sleep 1; done
	@launchctl bootstrap gui/$$(id -u) $(BOT_PLIST)
	@echo "bot running under launchd; log in .data/logs/bot.log"

.PHONY: bot-uninstall
bot-uninstall: ## Stop and remove the permanent Telegram bot
	@launchctl bootout gui/$$(id -u)/$(BOT_LABEL) 2>/dev/null || true
	@rm -f $(BOT_PLIST)
	@echo "bot removed"

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
