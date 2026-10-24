BINARY  := weavectl
DEST    := bin/$(BINARY)
IMAGE   := docker.io/semitechnologies/weaviate:1.40.1
ROOTKEY := root-key-1

.PHONY: build test coverage fmt vet lint clean install dev-up dev-down dev-logs integration

build:
	go build -ldflags "-X github.com/sriharip316/weavectl/cmd.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)" -o $(DEST) .

test:
	go test ./...

coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

fmt:
	go fmt ./...

vet:
	go vet ./...

lint:
	staticcheck ./...
	golangci-lint run ./...
	govulncheck ./...

clean:
	rm -rf bin dist coverage.out

install: build
	install -D $(DEST) $(HOME)/.local/bin/$(BINARY)

# Run a local Weaviate with API-key auth, RBAC and db-user management
# enabled — the configuration weavectl's role and user commands need —
# plus the filesystem backup module the backup/restore commands need.
dev-up:
	podman rm -f weavectl-dev >/dev/null 2>&1 || true
	podman run -d --name weavectl-dev -p 8080:8080 \
	  -e AUTHENTICATION_APIKEY_ENABLED=true \
	  -e AUTHENTICATION_APIKEY_ALLOWED_KEYS=$(ROOTKEY) \
	  -e AUTHENTICATION_APIKEY_USERS=admin@example.com \
	  -e AUTHENTICATION_DB_USERS_ENABLED=true \
	  -e AUTHORIZATION_RBAC_ENABLED=true \
	  -e AUTHORIZATION_RBAC_ROOT_USERS=admin@example.com \
	  -e DEFAULT_VECTORIZER_MODULE=none \
	  -e ENABLE_MODULES=backup-filesystem \
	  -e BACKUP_FILESYSTEM_PATH=/tmp/weaviate-backups \
	  $(IMAGE)
	@until curl -sf http://localhost:8080/v1/.well-known/ready >/dev/null 2>&1; do sleep 1; done
	@echo "Weaviate ready at http://localhost:8080 (api key: $(ROOTKEY))"

dev-down:
	podman rm -f weavectl-dev

dev-logs:
	podman logs -f weavectl-dev

# End-to-end run of the whole command surface against the dev container.
integration: build dev-up
	WEAVECTL_SERVER=http://localhost:8080 WEAVECTL_API_KEY=$(ROOTKEY) bash scripts/integration.sh
