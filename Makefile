.DEFAULT_GOAL := help
COVER_PROFILE := coverage.out

.PHONY: help fmt lint test integration-test cover-html

help: ## Show available targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

fmt: ## Format code (gofmt -s via golangci-lint)
	golangci-lint fmt ./...

lint: ## Run golangci-lint
	golangci-lint run ./...

test: ## Run unit tests with race detector and coverage
	go test -race -count=1 -coverprofile=$(COVER_PROFILE) ./...
	@go tool cover -func=$(COVER_PROFILE) | tail -1

integration-test: ## Run end-to-end tests against https://swapi.dev (needs internet)
	go test -tags integration -race -count=1 -run '^TestIntegration' -v ./...

cover-html: test ## Open the HTML coverage report
	go tool cover -html=$(COVER_PROFILE)
