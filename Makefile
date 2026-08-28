.DEFAULT_GOAL := help
DB_URL ?= postgres://composectl:composectl@localhost:5473/composectl?sslmode=disable
MIGRATION_DB_URL ?= postgres://composectl:composectl@postgres:5432/composectl?sslmode=disable
API    ?= http://localhost:8417
# Every node's agent, for log tailing and for the stop/start dance the tests
# need. Scaling the fleet means adding a node here and in compose.yaml.
AGENTS ?= agent-1 agent-2 agent-3 agent-4
API_TOKEN ?= dev-operator-token-change-me

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n",$$1,$$2}'

.PHONY: tidy
tidy: ## Resolve module dependencies
	go mod tidy

.PHONY: build
build: ## Build binaries into ./bin
	CGO_ENABLED=0 go build -o bin/controlplane ./cmd/controlplane
	CGO_ENABLED=0 go build -o bin/navarch ./cmd/navarch

.PHONY: test
test: ## Run tests (stops the dev control plane, fails on any skip)
	@./scripts/test.sh

.PHONY: up
up: ## Start the dev stack
	# --remove-orphans is not tidiness. Renaming a service leaves the old
	# container running, and a stale agent keeps registering: RegisterNode
	# upserts by (org_id, hostname), so an orphan and its replacement fight over
	# one node row, alternately publishing their own advertise address. Routes
	# then point at whichever won last. The fleet rename from `agent`/`dind-b` to
	# `agent-N`/`dind-N` produced exactly that, and it was invisible until
	# someone looked at `docker ps` rather than at the node list.
	docker compose up -d --build --remove-orphans

.PHONY: down
down: ## Stop the dev stack
	docker compose down

.PHONY: nuke
nuke: ## Stop the dev stack and delete volumes
	docker compose down -v

.PHONY: logs
logs: ## Tail control plane logs
	docker compose logs -f controlplane

.PHONY: psql
psql: ## Open a psql shell
	docker compose exec postgres psql -U composectl -d composectl

.PHONY: migrate-up
migrate-up: ## Apply migrations
	docker compose run --rm migrate -path=/migrations -database="$(MIGRATION_DB_URL)" up

.PHONY: migrate-down
migrate-down: ## Roll back one migration
	docker compose run --rm migrate -path=/migrations -database="$(MIGRATION_DB_URL)" down 1

.PHONY: validate
validate: ## Validate the example stack against the running API
	curl -sS -H "Authorization: Bearer $(API_TOKEN)" -X POST $(API)/v1/validate \
		--data-binary @examples/webapp/compose.yaml | jq .

.PHONY: health
health: ## Check control plane health
	curl -sS $(API)/healthz | jq .

.PHONY: metrics
metrics: ## Show Prometheus-compatible control-plane metrics
	curl -sS -H "Authorization: Bearer $(API_TOKEN)" $(API)/metrics

.PHONY: demo
demo: ## Walk the full loop: catalog -> version -> agent-driven rollout -> promote
	API=$(API) API_TOKEN=$(API_TOKEN) ./scripts/demo.sh

.PHONY: demo-failure
demo-failure: ## Show a failed rollout leaving any live deployment untouched
	API=$(API) API_TOKEN=$(API_TOKEN) ./scripts/demo-failure.sh

.PHONY: demo-rollback
demo-rollback: ## Deploy two versions, then roll back to the first
	API=$(API) API_TOKEN=$(API_TOKEN) ./scripts/demo-rollback.sh

.PHONY: demo-secrets
demo-secrets: ## Set + deploy a secret end to end: ciphertext at rest, plaintext through Traefik, 422 when unset
	API=$(API) API_TOKEN=$(API_TOKEN) DB_URL=$(DB_URL) ./scripts/demo-secrets.sh

.PHONY: demo-preview
demo-preview: ## Create a preview env with inherited secrets, then watch it expire and get reaped
	@API_TOKEN=$(API_TOKEN) ./scripts/demo-preview.sh

.PHONY: demo-fleet
migrate-check: ## Round-trip every migration up/down/up on a throwaway database
	./scripts/migrate-check.sh

release: ## Build versioned, reproducible navarch binaries into dist/
	./scripts/release.sh

site-image: ## Build the marketing-site image and load it onto every node
	./scripts/site-image.sh

# The CLI-driven demos depend on `build`. A stale ./bin/navarch is the worst
# kind of green: the demo passes, against code nobody is running. This was hit
# twice in one session — a guard that refuses plaintext credentials was verified
# "working" by a binary that predated it.
demo-fleet: build ## Two nodes, two daemons: ingress stack pinned to the router node, worker stack spread to the other
	API=$(API) API_TOKEN=$(API_TOKEN) ./scripts/demo-fleet.sh

.PHONY: demo-logs
demo-logs: build ## Read a container's output through the fleet, and prove none of it is stored
	API=$(API) API_TOKEN=$(API_TOKEN) ./scripts/demo-logs.sh

.PHONY: agent-logs
demo-tls: build ## Put TLS in front of the control plane and prove it, both directions
	./scripts/demo-tls.sh

demo-site: build site-image ## Deploy Navarch's own marketing site onto Navarch
	API=$(API) API_TOKEN=$(API_TOKEN) ./scripts/demo-site.sh

agent-logs: ## Tail the node agent logs
	docker compose logs -f $(AGENTS)
