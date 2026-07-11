# # Production-grade Makefile for resource-forecaster microservice
# # Supports local development, CI/CD, and production deployment

# # Variables
APP_NAME := resource-forecaster
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%H:%M:%S')
GIT_COMMIT := $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
DOCKER_REGISTRY ?= ghcr.io
DOCKER_IMAGE := $(DOCKER_REGISTRY)/$(APP_NAME)

# # Go build settings
GO := go
GOOS ?= linux
GOARCH ?= amd64
CGO_ENABLED ?= 0
LDFLAGS := -s -w \
	-X main.Version=$(VERSION) \
	-X main.BuildTime=$(BUILD_TIME) \
	-X main.GitCommit=$(GIT_COMMIT)

# # Colors for output
GREEN  := $(shell tput -Txterm setaf 2)
YELLOW := $(shell tput -Txterm setaf 3)
WHITE  := $(shell tput -Txterm setaf 7)
RESET  := $(shell tput -Txterm sgr0)

.PHONY: all
all: help

.PHONY: help
help: ## Show this help message
	@echo ''
	@echo 'Usage:'
	@echo '  ${YELLOW}make${RESET} ${GREEN}<target>${RESET}'
	@echo ''
	@echo 'Targets:'
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  ${YELLOW}%-25s${GREEN}%s${RESET}\n", $$1, $$2}' $(MAKEFILE_LIST)

# # Development targets
.PHONY: deps
deps: ## Download and verify dependencies
	$(GO) mod download
	$(GO) mod verify
	$(GO) mod tidy
	@echo "${GREEN}Dependencies installed successfully${RESET}"

.PHONY: fmt
fmt: ## Format code with gofmt and goimports
	$(GO) fmt ./...
	goimports -w -l .
	@echo "${GREEN}Code formatted${RESET}"

.PHONY: lint
lint: ## Run comprehensive linters
	golangci-lint run --timeout=10m --config=.golangci.yml ./...
	@echo "${GREEN}Linting passed${RESET}"

.PHONY: vet
vet: ## Run go vet with shadow analysis
	$(GO) vet -vettool=$(shell which shadow) ./...
	@echo "${GREEN}Vet passed${RESET}"

.PHONY: security-scan
security-scan: ## Run security vulnerability scanning
	govulncheck ./...
	gosec -quiet -fmt=json -out=gosec-report.json ./...
	trivy fs --exit-code=1 --severity=HIGH,CRITICAL .
	@echo "${GREEN}Security scan passed${RESET}"

# # Build targets
.PHONY: build
build: ## Build binary for current platform
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) \
		$(GO) build -ldflags="$(LDFLAGS)" \
		-o bin/$(APP_NAME) cmd/forecaster/main.go
	@echo "${GREEN}Binary built: bin/$(APP_NAME)${RESET}"

.PHONY: build-all
build-all: ## Build for all supported platforms
	@echo "Building for multiple platforms..."
	GOOS=linux GOARCH=amd64 $(MAKE) build
	mv bin/$(APP_NAME) bin/$(APP_NAME)-linux-amd64
	GOOS=linux GOARCH=arm64 $(MAKE) build
	mv bin/$(APP_NAME) bin/$(APP_NAME)-linux-arm64
	GOOS=darwin GOARCH=amd64 $(MAKE) build
	mv bin/$(APP_NAME) bin/$(APP_NAME)-darwin-amd64
	@echo "${GREEN}All binaries built${RESET}"

.PHONY: docker-build
docker-build: ## Build Docker image
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		-t $(DOCKER_IMAGE):$(VERSION) \
		-t $(DOCKER_IMAGE):latest \
		-f deploy/docker/Dockerfile .
	@echo "${GREEN}Docker image built: $(DOCKER_IMAGE):$(VERSION)${RESET}"

.PHONY: docker-push
docker-push: ## Push Docker image to registry
	docker push $(DOCKER_IMAGE):$(VERSION)
	docker push $(DOCKER_IMAGE):latest
	@echo "${GREEN}Docker image pushed${RESET}"

# # Testing targets
.PHONY: test
test: ## Run unit tests with race detection
	$(GO) test -race -v -coverprofile=coverage.out -covermode=atomic ./...
	@echo "${GREEN}Unit tests passed${RESET}"

.PHONY: test-coverage
test-coverage: test ## Generate test coverage report
	$(GO) tool cover -html=coverage.out -o coverage.html
	$(GO) tool cover -func=coverage.out | grep total:
	@echo "${GREEN}Coverage report generated: coverage.html${RESET}"

.PHONY: test-integration
test-integration: ## Run integration tests (requires infrastructure)
	@echo "Running integration tests..."
	$(GO) test -v -tags=integration ./tests/integration/...
	@echo "${GREEN}Integration tests passed${RESET}"

.PHONY: test-load
test-load: ## Run k6 load tests
	k6 run tests/load/k6-script.js --out json=load-test-results.json
	@echo "${GREEN}Load test completed${RESET}"

# # Database targets
.PHONY: db-migrate
db-migrate: ## Run database migrations
	$(GO) run cmd/migrate/main.go
	@echo "${GREEN}Database migrations completed${RESET}"

.PHONY: db-seed
db-seed: ## Seed database with test data
	@bash scripts/seed_data.sh
	@echo "${GREEN}Database seeded${RESET}"

# # Deployment targets
.PHONY: deploy-k8s
deploy-k8s: ## Deploy to Kubernetes
	kubectl apply -f deploy/kubernetes/namespace.yaml
	kubectl apply -f deploy/kubernetes/configmap.yaml
	kubectl apply -f deploy/kubernetes/secrets.yaml
	kubectl apply -f deploy/kubernetes/deployment.yaml
	kubectl apply -f deploy/kubernetes/service.yaml
	kubectl apply -f deploy/kubernetes/hpa.yaml
	kubectl apply -f deploy/kubernetes/pdb.yaml
	kubectl apply -f deploy/kubernetes/servicemonitor.yaml
	kubectl rollout status deployment/$(APP_NAME) -n monitoring
	@echo "${GREEN}Deployed to Kubernetes${RESET}"

.PHONY: deploy-terraform
deploy-terraform: ## Deploy infrastructure with Terraform
	cd deploy/terraform && terraform init && terraform plan && terraform apply -auto-approve
	@echo "${GREEN}Infrastructure deployed${RESET}"

.PHONY: rollback
rollback: ## Rollback Kubernetes deployment
	kubectl rollout undo deployment/$(APP_NAME) -n monitoring
	@echo "${GREEN}Rollback initiated${RESET}"

# # Development helpers
.PHONY: dev
dev: ## Run in development mode with hot reload
	air -c .air.toml

.PHONY: logs
logs: ## Tail Kubernetes logs
	kubectl logs -f deployment/$(APP_NAME) -n monitoring --tail=100

.PHONY: shell
shell: ## Open shell in running container
	kubectl exec -it deployment/$(APP_NAME) -n monitoring -- /bin/sh

# # Cleanup targets
.PHONY: clean
clean: ## Clean build artifacts
	rm -rf bin/
	rm -f coverage.out coverage.html
	rm -f gosec-report.json
	rm -f load-test-results.json
	@echo "${GREEN}Cleaned${RESET}"

.PHONY: clean-all
clean-all: clean ## Clean everything including Docker images
	docker rmi $(DOCKER_IMAGE):$(VERSION) $(DOCKER_IMAGE):latest 2>/dev/null || true
	@echo "${GREEN}All cleaned${RESET}"

# # CI/CD targets
.PHONY: ci-build
ci-build: deps fmt lint vet security-scan test build ## Full CI build pipeline

.PHONY: cd-deploy
cd-deploy: ci-build docker-build docker-push deploy-k8s ## Full CD deployment pipeline
