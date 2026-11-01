#!/usr/bin/env bash
# End-to-end exercise of weavectl against a live Weaviate: make integration
# starts the MinIO stack below and runs every check against it — the whole
# command surface plus both backup backends (filesystem and s3). Any failure
# aborts: assertions use grep against actual output.
#
# The S3 checks are skipped when the connected server lacks the backup-s3
# module (e.g. the lighter make dev-up stack, and CI's service container),
# so the same script serves those environments too.
#
# make dev-up/dev-down delegate here too: they run the lighter stack (a
# single Weaviate with only the filesystem backup module, no MinIO).
#
# usage: scripts/integration.sh [run|dev-up|dev-down|s3-up|s3-down]
set -euo pipefail

cd "$(dirname "$0")/.."

WEAVECTL="${WEAVECTL:-./bin/weavectl}"

IMAGE="${IMAGE:-docker.io/semitechnologies/weaviate:1.40.1}"
# MinIO no longer allows anonymous image pulls from Docker Hub or quay.io
# (2025 distribution change), so these default to the images present in
# the local container store; override if your registry differs.
MINIO_IMAGE="${MINIO_IMAGE:-docker.io/minio/minio:latest}"
MC_IMAGE="${MC_IMAGE:-docker.io/minio/mc:latest}"
NETWORK="weavectl-s3"
MINIO_CONTAINER="weavectl-minio"
WEAVIATE_CONTAINER="weavectl-s3-dev"
BUCKET="weavectl-backups"
MINIO_USER="minioroot"
MINIO_PASSWORD="minioroot-password"
SERVER="${WEAVECTL_SERVER:-http://localhost:8080}"
API_KEY="${WEAVECTL_API_KEY:-root-key-1}"

step() { printf '\n=== %s ===\n' "$*"; }
expect_in() { # expect_in <needle> <description>; reads stdin fully, then matches
	local needle="$1"; shift
	local out
	out=$(cat)
	printf '%s\n' "$out" | grep -qF -- "$needle" || {
		echo "FAIL: expected '$needle' in output ($*):" >&2
		printf '%s\n' "$out" >&2
		exit 1
	}
}
expect_fail_in() { # expect_fail_in <needle> <cmd...>: cmd must fail AND print needle
	local needle="$1"; shift
	local out
	out=$("$@" 2>&1) && {
		echo "FAIL: expected command to fail: $*" >&2
		exit 1
	}
	printf '%s\n' "$out" | grep -qF -- "$needle" || {
		echo "FAIL: expected '$needle' in failure output of $*:" >&2
		printf '%s\n' "$out" >&2
		exit 1
	}
}

mc() { # mc <command...>: run mc inside the podman network against MinIO
	podman run --rm --network "$NETWORK" --entrypoint /bin/sh "$MC_IMAGE" -c \
		"mc alias set srv http://minio:9000 '$MINIO_USER' '$MINIO_PASSWORD' >/dev/null && mc $*"
}

wait_backup() { # wait_backup <backend> <id> <path>: poll until the backup completes;
	# Weaviate serializes backups/restores on one coordinator lock, so the
	# next operation must not start until this one is terminal.
	local backend="$1" id="$2" path="$3"
	for _ in $(seq 1 60); do
		if $WEAVECTL get backups --backend "$backend" "$id" --bucket "$BUCKET" --path "$path" 2>/dev/null | grep -q SUCCESS; then
			return 0
		fi
		sleep 2
	done
	echo "FAIL: backup $backend/$id did not reach SUCCESS in time" >&2
	exit 1
}

# Weaviate env shared by both stacks: API-key auth, RBAC and db-user
# management (the role/user commands need it) plus the filesystem backup
# module. Each stack adds its own ENABLE_MODULES and backend config.
WEAVIATE_ENV=(
	-e AUTHENTICATION_APIKEY_ENABLED=true
	-e AUTHENTICATION_APIKEY_ALLOWED_KEYS="$API_KEY"
	-e AUTHENTICATION_APIKEY_USERS=admin@example.com
	-e AUTHENTICATION_DB_USERS_ENABLED=true
	-e AUTHORIZATION_RBAC_ENABLED=true
	-e AUTHORIZATION_RBAC_ROOT_USERS=admin@example.com
	-e DEFAULT_VECTORIZER_MODULE=none
	-e BACKUP_FILESYSTEM_PATH=/tmp/weaviate-backups
)

dev_up() { # the default dev stack (make dev-up): a single Weaviate with
	# only the filesystem backup module — the S3 checks skip against it.
	command -v podman >/dev/null 2>&1 || { echo "podman is required" >&2; exit 1; }
	# Both stacks bind 8080, so stop the MinIO one first if it is up.
	podman rm -f weavectl-dev "$WEAVIATE_CONTAINER" >/dev/null 2>&1 || true
	echo "starting weaviate ($IMAGE) with backup-filesystem..."
	podman run -d --name weavectl-dev -p 8080:8080 \
		"${WEAVIATE_ENV[@]}" \
		-e ENABLE_MODULES=backup-filesystem \
		"$IMAGE" >/dev/null
	until curl -sf "${SERVER%/}/v1/.well-known/ready" >/dev/null 2>&1; do sleep 1; done
	echo "Weaviate ready at $SERVER (api key: $API_KEY)"
}

dev_down() {
	podman rm -f weavectl-dev >/dev/null 2>&1 || true
}

s3_up() { # the MinIO stack (make s3-up / make integration): MinIO plus
	# Weaviate with backup-s3 against it.
	command -v podman >/dev/null 2>&1 || { echo "podman is required" >&2; exit 1; }
	# The stack binds 8080 like the default dev container (make dev-up),
	# so stop that one first.
	podman rm -f weavectl-dev >/dev/null 2>&1 || true
	podman network create "$NETWORK" >/dev/null 2>&1 || true
	podman rm -f "$MINIO_CONTAINER" "$WEAVIATE_CONTAINER" >/dev/null 2>&1 || true

	echo "starting minio ($MINIO_IMAGE)..."
	podman run -d --name "$MINIO_CONTAINER" --network "$NETWORK" --network-alias minio \
		-p 9000:9000 -p 9001:9001 \
		-e MINIO_ROOT_USER="$MINIO_USER" \
		-e MINIO_ROOT_PASSWORD="$MINIO_PASSWORD" \
		"$MINIO_IMAGE" server /data --console-address ":9001" >/dev/null
	until curl -sf http://localhost:9000/minio/health/ready >/dev/null 2>&1; do sleep 1; done
	mc "mb --ignore-existing srv/$BUCKET" >/dev/null

	echo "starting weaviate ($IMAGE) with backup-s3 against minio..."
	podman run -d --name "$WEAVIATE_CONTAINER" --network "$NETWORK" -p 8080:8080 \
		"${WEAVIATE_ENV[@]}" \
		-e ENABLE_MODULES=backup-filesystem,backup-s3 \
		-e BACKUP_S3_BUCKET="$BUCKET" \
		-e BACKUP_S3_ENDPOINT=minio:9000 \
		-e BACKUP_S3_USE_SSL=false \
		-e AWS_REGION=us-east-1 \
		-e AWS_ACCESS_KEY_ID="$MINIO_USER" \
		-e AWS_SECRET_ACCESS_KEY="$MINIO_PASSWORD" \
		"$IMAGE" >/dev/null
	until curl -sf "${SERVER%/}/v1/.well-known/ready" >/dev/null 2>&1; do sleep 1; done
	echo "Weaviate ready at $SERVER (api key: $API_KEY; s3 backend: bucket $BUCKET @ minio:9000)"
}

s3_down() {
	podman rm -f "$WEAVIATE_CONTAINER" "$MINIO_CONTAINER" >/dev/null 2>&1 || true
	podman network rm "$NETWORK" >/dev/null 2>&1 || true
}

s3_checks() { # the S3 half of the suite, called from run(); needs backup-s3.
	# Targets finding 3: backups stored at a non-default bucket/path must
	# be findable by every status lookup, so "create backup s3 ... --bucket
	# B --path P --wait" and the matching restore only succeed when the CLI
	# carries the location into the polls.
	local RUN_ID
	RUN_ID="$(date +%s)" # unique backup ids keep reruns independent

	step "seed Articles collection"
	$WEAVECTL apply -f examples/collection-articles.yaml | expect_in "collection/Articles" "seed collection"

	step "create backup s3 with --bucket/--path --wait"
	$WEAVECTL create backup s3 "s3-wait-$RUN_ID" --include Articles \
		--bucket "$BUCKET" --path sub1 --wait | expect_in "status: SUCCESS" "create backup s3 --wait"

	step "backup objects really live under the custom path in minio"
	mc "ls --recursive srv/$BUCKET/sub1" | expect_in "s3-wait-$RUN_ID" "minio objects at sub1"

	step "get backups status with --bucket/--path"
	$WEAVECTL get backups --backend s3 "s3-wait-$RUN_ID" \
		--bucket "$BUCKET" --path sub1 | expect_in "SUCCESS" "get backups single status"

	step "restore with --bucket/--path --wait"
	$WEAVECTL delete collection articles | expect_in "deleted" "delete for restore"
	$WEAVECTL restore s3 "s3-wait-$RUN_ID" --include Articles \
		--bucket "$BUCKET" --path sub1 --wait | expect_in "status: SUCCESS" "restore s3 --wait"
	$WEAVECTL get collection articles | expect_in "Articles" "collection restored"

	step "manifest-driven backup at a custom location is found by its pre-checks"
	cat > "/tmp/weavectl-s3-backup-$RUN_ID.yaml" <<-EOF
		kind: Backup
		backend: s3
		id: s3-manifest-$RUN_ID
		include: [Articles]
		config:
		  bucket: $BUCKET
		  path: sub2
	EOF
	$WEAVECTL apply -f "/tmp/weavectl-s3-backup-$RUN_ID.yaml" | expect_in "backup/s3/s3-manifest-$RUN_ID created" "backup apply"
	# Wait for completion, then re-apply must recognize the completed
	# backup at its location ("unchanged"/"already exists"), not restart
	# it after a NotFound pre-check at the wrong location.
	wait_backup s3 "s3-manifest-$RUN_ID" sub2
	$WEAVECTL apply -f "/tmp/weavectl-s3-backup-$RUN_ID.yaml" | expect_in "backup/s3/s3-manifest-$RUN_ID unchanged" "backup re-apply unchanged"
	$WEAVECTL apply -f "/tmp/weavectl-s3-backup-$RUN_ID.yaml" | expect_in "backup already exists" "backup re-apply recognized"
	expect_fail_in "already exists" $WEAVECTL create -f "/tmp/weavectl-s3-backup-$RUN_ID.yaml"

	step "manifest-driven restore pre-check finds the finished restore"
	cat > "/tmp/weavectl-s3-restore-$RUN_ID.yaml" <<-EOF
		kind: Restore
		backend: s3
		id: s3-wait-$RUN_ID
		include: [Articles]
		config:
		  bucket: $BUCKET
		  path: sub1
	EOF
	$WEAVECTL apply -f "/tmp/weavectl-s3-restore-$RUN_ID.yaml" | expect_in "restore/s3/s3-wait-$RUN_ID unchanged" "restore apply unchanged"

	step "filesystem control: --path --wait (original finding-3 repro shape)"
	$WEAVECTL create backup filesystem "fs-wait-$RUN_ID" --include Articles \
		--path subfs --wait | expect_in "status: SUCCESS" "create backup filesystem --wait"
}

run() { # the whole suite: the command surface, then the S3 backup checks.
	step "version"
	$WEAVECTL version | expect_in "Weaviate 1.40" "version"

	step "create collection from flags"
	$WEAVECTL create collection integration_books --replication-factor 1 | expect_in "collection/Integration_books created" "create collection"
	$WEAVECTL get collection integration_books | expect_in "Integration_books" "get collection"

	step "create -f and duplicate rejection"
	sed 's/class: Articles/class: CreateFTest/' examples/collection-articles.yaml > /tmp/weavectl-createf.yaml
	$WEAVECTL create -f /tmp/weavectl-createf.yaml | expect_in "collection/CreateFTest created" "create -f"
	expect_fail_in "already exists" $WEAVECTL create -f /tmp/weavectl-createf.yaml
	$WEAVECTL delete collection createftest | expect_in "deleted" "create -f cleanup"

	$WEAVECTL apply -f examples/collection-articles.yaml | expect_in "collection/Articles created" "apply create"
	expect_fail_in "already exists" $WEAVECTL create -f examples/collection-articles.yaml

	step "apply is idempotent"
	$WEAVECTL apply -f examples/collection-articles.yaml | expect_in "collection/Articles unchanged" "re-apply"
	$WEAVECTL apply -f examples/collection-articles.yaml | expect_in "collection/Articles unchanged" "re-apply again"

	step "collection evolve: new property, config change, immutable warning"
	cat > /tmp/weavectl-evolved.yaml <<-'EOF'
		class: Articles
		description: news articles
		vectorizer: none
		invertedIndexConfig:
		  cleanupIntervalSeconds: 120
		properties:
		  - name: title
		    dataType: [text]
		    description: article headline
		    tokenization: lowercase
		  - name: url
		    dataType: [text]
		    indexFilterable: true
		  - name: publishedAt
		    dataType: [date]
		  - name: author
		    dataType: [text]
	EOF
	$WEAVECTL apply -f /tmp/weavectl-evolved.yaml | expect_in 'property "author" added' "property add"
	$WEAVECTL apply -f /tmp/weavectl-evolved.yaml | expect_in "tokenization is immutable" "immutable warning"
	$WEAVECTL get collection Articles -o json | expect_in '"cleanupIntervalSeconds": 120' "config applied"

	step "yaml roundtrip"
	$WEAVECTL get collection Articles -o yaml > /tmp/weavectl-roundtrip.yaml
	$WEAVECTL apply -f /tmp/weavectl-roundtrip.yaml | expect_in "collection/Articles unchanged" "roundtrip apply"

	step "roles: create, converge, diff"
	$WEAVECTL apply -f examples/role-editor.yaml | expect_in "role/editor created" "role create"
	$WEAVECTL apply -f examples/role-editor.yaml | expect_in "role/editor unchanged" "role idempotent"
	$WEAVECTL get roles | expect_in "editor" "roles table"

	step "users: create via apply converges roles"
	$WEAVECTL apply -f examples/user-editor.yaml | expect_in "user/editor" "user apply"
	$WEAVECTL apply -f examples/user-editor.yaml | expect_in "user/editor unchanged" "user idempotent"
	$WEAVECTL get user editor | expect_in "editor" "user table"

	step "users: activate/deactivate via apply"
	cat > /tmp/weavectl-user-off.yaml <<-'EOF'
		userId: editor
		roles: [editor]
		active: false
	EOF
	$WEAVECTL apply -f /tmp/weavectl-user-off.yaml | expect_in "user/editor configured" "user deactivate"
	$WEAVECTL apply -f /tmp/weavectl-user-off.yaml | expect_in "user/editor unchanged" "user deactivate idempotent"
	$WEAVECTL get user editor | expect_in "false" "user shows inactive"
	$WEAVECTL apply -f examples/user-editor.yaml | expect_in "user/editor configured" "user reactivate"
	$WEAVECTL apply -f examples/user-editor.yaml | expect_in "user/editor unchanged" "user reactivate idempotent"

	step "aliases"
	$WEAVECTL apply -f examples/alias-articles.yaml | expect_in "alias/News created" "alias create"
	sed 's/class: Articles/class: Integration_books/' examples/alias-articles.yaml > /tmp/weavectl-alias2.yaml
	$WEAVECTL apply -f /tmp/weavectl-alias2.yaml | expect_in "alias/News configured" "alias retarget"
	$WEAVECTL get aliases | expect_in "Integration_books" "aliases table"

	step "tenants: multi-doc apply and status convergence"
	$WEAVECTL create collection MTNews --multi-tenancy | expect_in "collection/MTNews created" "mt collection"
	$WEAVECTL apply -f examples/tenants-mtnews.yaml | expect_in "created tenant(s): tenant-a, tenant-b" "tenant create"
	$WEAVECTL apply -f examples/tenants-mtnews.yaml | expect_in "tenants/MTNews unchanged" "tenant idempotent"
	$WEAVECTL get tenants mtnews | expect_in "tenant-a" "tenants table"
	$WEAVECTL get shards articles | expect_in "READY" "shards table"

	step "multi-document apply"
	$WEAVECTL apply -f examples/full-stack.yaml | expect_in "collection/Archive created" "multi-doc"
	$WEAVECTL apply -f examples/full-stack.yaml | expect_in "role/archivist unchanged" "multi-doc idempotent"

	step "create user prints one-time api key"
	$WEAVECTL create user keymaster | expect_in "API key (shown once" "user create key"

	step "rotate user revokes the old key"
	OLD_KEY=$($WEAVECTL create user rotator | sed -n 's/^API key (shown once, store it now): //p')
	$WEAVECTL rotate user rotator | expect_in "user/rotator rotated" "rotate user"
	$WEAVECTL rotate user rotator | expect_in "API key (shown once" "rotate shows new key"
	expect_fail_in "Error from server" $WEAVECTL get user rotator --api-key "$OLD_KEY"

	step "delete flows"
	$WEAVECTL delete tenant mtnews tenant-a | expect_in "deleted" "tenant delete"
	$WEAVECTL delete alias news | expect_in "deleted" "alias delete"
	$WEAVECTL delete role editor | expect_in "deleted" "role delete"
	$WEAVECTL delete user editor | expect_in "deleted" "user delete"
	cat > /tmp/weavectl-delete.yaml <<-'EOF'
		class: Archive
	EOF
	$WEAVECTL delete -f /tmp/weavectl-delete.yaml | expect_in "collection \"Archive\" deleted" "delete -f"

	step "case-insensitive collection apply and canonical delete"
	cat > /tmp/weavectl-case.yaml <<-'EOF'
		class: integration_books
		description: books with different case class name
	EOF
	$WEAVECTL apply -f /tmp/weavectl-case.yaml | expect_in "collection/Integration_books configured" "case apply"
	$WEAVECTL delete collection integration_books | expect_in 'collection "Integration_books" deleted' "canonical delete"

	step "backups: manifest-driven backup, export round-trip"
	$WEAVECTL apply -f examples/backup-articles.yaml | expect_in "backup/filesystem/articles-snapshot created" "backup apply"
	for _ in $(seq 1 60); do
		$WEAVECTL get backups --backend filesystem articles-snapshot 2>/dev/null | grep -q SUCCESS && break
		sleep 2
	done
	$WEAVECTL get backups --backend filesystem articles-snapshot | expect_in "SUCCESS" "backup completed"
	$WEAVECTL apply -f examples/backup-articles.yaml | expect_in "backup/filesystem/articles-snapshot unchanged" "backup idempotent"
	$WEAVECTL get backups --backend filesystem | expect_in "articles-snapshot" "backups table"
	$WEAVECTL get backups --backend filesystem -o yaml > /tmp/weavectl-backups.yaml
	$WEAVECTL apply -f /tmp/weavectl-backups.yaml | expect_in "backup/filesystem/articles-snapshot unchanged" "backup roundtrip"

	step "backups: imperative create with --wait, then delete-collection + restore"
	$WEAVECTL create backup filesystem articles-snapshot-2 --include Articles --wait \
		| expect_in "status: SUCCESS" "create backup --wait"
	$WEAVECTL delete collection articles | expect_in 'collection "Articles" deleted' "delete for restore"
	$WEAVECTL apply -f examples/restore-articles.yaml | expect_in "restore/filesystem/articles-snapshot created" "restore apply"
	for _ in $(seq 1 60); do
		$WEAVECTL restore status filesystem articles-snapshot 2>/dev/null | grep -q SUCCESS && break
		sleep 2
	done
	$WEAVECTL restore status filesystem articles-snapshot | expect_in "SUCCESS" "restore completed"
	$WEAVECTL get collection articles | expect_in "Articles" "collection restored"

	step "error paths"
	expect_fail_in "(NotFound)" $WEAVECTL get collection does_not_exist
	expect_fail_in "(NotFound)" $WEAVECTL delete collection does_not_exist
	expect_fail_in "unknown output format" $WEAVECTL get collections -o invalid_format

	step "s3 backup checks (only if the server has backup-s3)"
	if curl -sf -H "Authorization: Bearer $API_KEY" "${SERVER%/}/v1/meta" | grep -q '"backup-s3"'; then
		s3_checks
	else
		echo "skipped: backup-s3 not enabled on $SERVER (make integration's MinIO stack has it)"
	fi

	printf '\nALL INTEGRATION CHECKS PASSED\n'
}

export WEAVECTL_SERVER="$SERVER" WEAVECTL_API_KEY="$API_KEY"

case "${1:-run}" in
dev-up) dev_up; exit 0 ;;
dev-down) dev_down; exit 0 ;;
s3-up) s3_up; exit 0 ;;
s3-down) s3_down; exit 0 ;;
run|"") run ;;
*)
	echo "usage: $0 {run|dev-up|dev-down|s3-up|s3-down}" >&2
	exit 2
	;;
esac
