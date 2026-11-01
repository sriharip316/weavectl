BINARY  := weavectl
DEST    := bin/$(BINARY)
IMAGE   := docker.io/semitechnologies/weaviate:1.40.1
ROOTKEY := root-key-1

.PHONY: build test coverage fmt vet lint clean install dev-up dev-down dev-logs integration s3-up s3-down

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
# The container config itself lives in scripts/integration.sh (dev-up),
# next to the MinIO stack it shares the auth/RBAC env with.
dev-up:
	bash scripts/integration.sh dev-up

dev-down:
	bash scripts/integration.sh dev-down

dev-logs:
	podman logs -f weavectl-dev

# End-to-end run of the whole command surface against the MinIO stack,
# which has both backup backends: the suite covers everything, including
# the S3 backup flows and --bucket/--path propagation (finding 3).
integration: build s3-up
	WEAVECTL_SERVER=http://localhost:8080 WEAVECTL_API_KEY=$(ROOTKEY) bash scripts/integration.sh

# The MinIO + Weaviate(backup-s3) stack that integration runs against;
# start/stop it standalone for manual poking.
s3-up:
	bash scripts/integration.sh s3-up

s3-down:
	bash scripts/integration.sh s3-down
