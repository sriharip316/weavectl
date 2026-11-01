#!/usr/bin/env bash
# End-to-end exercise of weavectl against a live Weaviate: make integration
# starts the MinIO stack below and runs every check against it — the whole
# command surface plus both backup backends (filesystem and s3). Any failure
# aborts: assertions use grep against actual output.
#
# Every command and its output are shown as the suite runs, with one ✓ line
# per check. Colors are on for terminals only; NO_COLOR disables them and
# FORCE_COLOR=1 forces them on (e.g. for `| tee`).
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
# expect_in sits at the end of many pipelines; without lastpipe it would run
# in a subshell and its CHECKS counter increments would never reach run().
shopt -s lastpipe

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

# ── output styling ──────────────────────────────────────────────────────────
# Colors when stdout is a terminal; NO_COLOR disables, FORCE_COLOR=1 forces
# them on. The ✓/✗/⏳/⏭ glyphs are plain text and always printed.
use_color=0
if [[ -t 1 ]]; then use_color=1; fi
if [[ -n "${FORCE_COLOR:-}" ]]; then use_color=1; fi
if [[ -n "${NO_COLOR:-}" ]]; then use_color=0; fi
C_RESET='' C_BOLD='' C_DIM='' C_CYAN='' C_GREEN='' C_RED='' C_YELLOW=''
if [[ "$use_color" == 1 ]]; then
	C_RESET=$'\033[0m' C_BOLD=$'\033[1m' C_DIM=$'\033[2m'
	C_CYAN=$'\033[1;36m' C_GREEN=$'\033[32m' C_RED=$'\033[1;31m' C_YELLOW=$'\033[33m'
fi
STEPS=0 CHECKS=0 T0=$SECONDS STEP_TITLE=''

# fmt_cmd <args...>: shell-quoted command line for display; the weavectl
# binary path is shown as plain "weavectl" and --api-key values are redacted
# so one-time keys never reach the log.
fmt_cmd() {
	local out='' a first=1
	for a in "$@"; do
		if [[ "$first" == 1 && "$a" == "$WEAVECTL" ]]; then a='weavectl'; fi
		first=0
		out+="$(printf '%q' "$a") "
	done
	printf '%s' "${out% }" | sed -E 's/(--api-key[= ])[^ ]+/\1<redacted>/g'
}

wx() { # wx <args...>: run weavectl, echoing the command line on stderr first
	# (stderr so it never enters the assertion pipe); exit status preserved
	printf '%s$ %s%s\n' "$C_DIM" "$(fmt_cmd weavectl "$@")" "$C_RESET" >&2
	"$WEAVECTL" "$@"
}

show_out() { # show_out <output>: print command output indented on stdout
	if [[ -n "$1" ]]; then
		printf '%s\n' "$1" | sed 's/^/  /'
	fi
}

step() { # step <title>: numbered banner, elapsed seconds since suite start
	STEPS=$((STEPS + 1))
	STEP_TITLE="$*"
	printf '\n%s━━ [%02d]%s %s%s%s %s· %ds%s\n' \
		"$C_CYAN" "$STEPS" "$C_RESET" "$C_BOLD" "$*" "$C_RESET" "$C_DIM" \
		"$((SECONDS - T0))" "$C_RESET"
}

fail_block() { # fail_block <desc> <expected> <context>; output on stdin is
	# appended; prints the report on stderr and exits 1
	local desc="$1" expected="$2" ctx="$3" out
	out=$(cat)
	{
		printf '%s✗ FAIL: %s%s\n' "$C_RED" "$desc" "$C_RESET"
		printf '    expected: %s\n' "$expected"
		printf '    context:  %s\n' "$ctx"
		if [[ -n "$out" ]]; then
			printf '    output:\n'
			printf '%s\n' "$out" | sed 's/^/      /'
		fi
	} >&2
	exit 1
}

expect_in() { # expect_in <needle> <description>; reads the command's output
	# on stdin, shows it, then matches — the suite aborts on a miss
	local needle="$1" desc="$2"
	local out
	out=$(cat)
	if printf '%s\n' "$out" | grep -qF -- "$needle"; then
		CHECKS=$((CHECKS + 1))
		show_out "$out"
		printf '%s✓ %s%s\n' "$C_GREEN" "$desc" "$C_RESET"
	else
		fail_block "$desc" "output containing '$needle'" "step [$STEPS] $STEP_TITLE" <<<"$out"
	fi
}

expect_fail_in() { # expect_fail_in <needle> <description> <cmd...>: cmd must
	# fail AND print needle in its combined output
	local needle="$1" desc="$2"
	shift 2
	printf '%s$ %s%s\n' "$C_DIM" "$(fmt_cmd "$@")" "$C_RESET" >&2
	local out
	if out=$("$@" 2>&1); then
		fail_block "$desc" "command to fail: $(fmt_cmd "$@")" "step [$STEPS] $STEP_TITLE" <<<"$out"
	fi
	if printf '%s\n' "$out" | grep -qF -- "$needle"; then
		CHECKS=$((CHECKS + 1))
		show_out "$out"
		printf '%s✓ %s%s\n' "$C_GREEN" "$desc" "$C_RESET"
	else
		fail_block "$desc" "failure output containing '$needle'" \
			"step [$STEPS] $STEP_TITLE · cmd: $(fmt_cmd "$@")" <<<"$out"
	fi
}

mc() { # mc <command...>: run mc inside the podman network against MinIO
	printf '%s$ mc %s%s\n' "$C_DIM" "$*" "$C_RESET" >&2
	podman run --rm --network "$NETWORK" --entrypoint /bin/sh "$MC_IMAGE" -c \
		"mc alias set srv http://minio:9000 '$MINIO_USER' '$MINIO_PASSWORD' >/dev/null && mc $*"
}

wait_backup() { # wait_backup <backend> <id> <path>: poll until the backup completes;
	# Weaviate serializes backups/restores on one coordinator lock, so the
	# next operation must not start until this one is terminal.
	local backend="$1" id="$2" path="$3"
	printf '%s⏳ waiting for backup %s/%s (path %s) to reach SUCCESS...%s\n' \
		"$C_DIM" "$backend" "$id" "$path" "$C_RESET"
	for _ in $(seq 1 60); do
		if $WEAVECTL get backups --backend "$backend" "$id" --bucket "$BUCKET" --path "$path" 2>/dev/null | grep -q SUCCESS; then
			return 0
		fi
		sleep 2
	done
	fail_block "backup $backend/$id did not reach SUCCESS in time" \
		"status SUCCESS within 120s" "step [$STEPS] $STEP_TITLE" </dev/null
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
	if ! command -v podman >/dev/null 2>&1; then
		printf '%s✗ podman is required%s\n' "$C_RED" "$C_RESET" >&2
		exit 1
	fi
	# Both stacks bind 8080, so stop the MinIO one first if it is up.
	podman rm -f weavectl-dev "$WEAVIATE_CONTAINER" >/dev/null 2>&1 || true
	printf '%s⏳ starting weaviate (%s) with backup-filesystem...%s\n' "$C_DIM" "$IMAGE" "$C_RESET"
	podman run -d --name weavectl-dev -p 8080:8080 \
		"${WEAVIATE_ENV[@]}" \
		-e ENABLE_MODULES=backup-filesystem \
		"$IMAGE" >/dev/null
	printf '%s⏳ waiting for weaviate to become ready at %s...%s\n' "$C_DIM" "$SERVER" "$C_RESET"
	until curl -sf "${SERVER%/}/v1/.well-known/ready" >/dev/null 2>&1; do sleep 1; done
	printf '%s✓ weaviate ready at %s (api key: %s)%s\n' "$C_GREEN" "$SERVER" "$API_KEY" "$C_RESET"
}

dev_down() {
	remove_container weavectl-dev
}

s3_up() { # the MinIO stack (make s3-up / make integration): MinIO plus
	# Weaviate with backup-s3 against it.
	if ! command -v podman >/dev/null 2>&1; then
		printf '%s✗ podman is required%s\n' "$C_RED" "$C_RESET" >&2
		exit 1
	fi
	# The stack binds 8080 like the default dev container (make dev-up),
	# so stop that one first.
	podman rm -f weavectl-dev >/dev/null 2>&1 || true
	podman network create "$NETWORK" >/dev/null 2>&1 || true
	podman rm -f "$MINIO_CONTAINER" "$WEAVIATE_CONTAINER" >/dev/null 2>&1 || true

	printf '%s⏳ starting minio (%s)...%s\n' "$C_DIM" "$MINIO_IMAGE" "$C_RESET"
	podman run -d --name "$MINIO_CONTAINER" --network "$NETWORK" --network-alias minio \
		-p 9000:9000 -p 9001:9001 \
		-e MINIO_ROOT_USER="$MINIO_USER" \
		-e MINIO_ROOT_PASSWORD="$MINIO_PASSWORD" \
		"$MINIO_IMAGE" server /data --console-address ":9001" >/dev/null
	printf '%s⏳ waiting for minio to become ready...%s\n' "$C_DIM" "$C_RESET"
	until curl -sf http://localhost:9000/minio/health/ready >/dev/null 2>&1; do sleep 1; done
	printf '%s✓ minio ready on :9000 (console on :9001)%s\n' "$C_GREEN" "$C_RESET"
	mc "mb --ignore-existing srv/$BUCKET" >/dev/null
	printf '%s✓ bucket %s ready%s\n' "$C_GREEN" "$BUCKET" "$C_RESET"

	printf '%s⏳ starting weaviate (%s) with backup-s3 against minio...%s\n' "$C_DIM" "$IMAGE" "$C_RESET"
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
	printf '%s⏳ waiting for weaviate to become ready at %s...%s\n' "$C_DIM" "$SERVER" "$C_RESET"
	until curl -sf "${SERVER%/}/v1/.well-known/ready" >/dev/null 2>&1; do sleep 1; done
	printf '%s✓ weaviate ready at %s (api key: %s; s3 backend: bucket %s @ minio:9000)%s\n' \
		"$C_GREEN" "$SERVER" "$API_KEY" "$BUCKET" "$C_RESET"
}

# remove_container <name>: force-remove if present and report. Success is
# judged by post-state (podman container exists), not rm's exit code: for a
# container on a custom rootless network, podman rm can exit nonzero with a
# "rootless netns: kill network process" cleanup error even though the
# container is gone — its name then never reaches stdout.
remove_container() {
	local name="$1"
	if podman container exists "$name" >/dev/null 2>&1; then
		podman rm -f "$name" >/dev/null 2>&1 || true
		if podman container exists "$name" >/dev/null 2>&1; then
			printf '%s· %s could not be removed%s\n' "$C_YELLOW" "$name" "$C_RESET"
		else
			printf '%s✓ removed %s%s\n' "$C_GREEN" "$name" "$C_RESET"
		fi
	fi
	return 0
}

s3_down() {
	remove_container "$WEAVIATE_CONTAINER"
	remove_container "$MINIO_CONTAINER"
	if podman network rm "$NETWORK" >/dev/null 2>&1; then
		printf '%s✓ removed network %s%s\n' "$C_GREEN" "$NETWORK" "$C_RESET"
	fi
}

s3_checks() { # the S3 half of the suite, called from run(); needs backup-s3.
	# Targets finding 3: backups stored at a non-default bucket/path must
	# be findable by every status lookup, so "create backup s3 ... --bucket
	# B --path P --wait" and the matching restore only succeed when the CLI
	# carries the location into the polls.
	local RUN_ID
	RUN_ID="$(date +%s)" # unique backup ids keep reruns independent

	step "seed Articles collection"
	wx apply -f examples/collection-articles.yaml | expect_in "collection/Articles" "seed collection"

	step "create backup s3 with --bucket/--path --wait"
	wx create backup s3 "s3-wait-$RUN_ID" --include Articles \
		--bucket "$BUCKET" --path sub1 --wait | expect_in "status: SUCCESS" "create backup s3 --wait"

	step "backup objects really live under the custom path in minio"
	mc "ls --recursive srv/$BUCKET/sub1" | expect_in "s3-wait-$RUN_ID" "minio objects at sub1"

	step "get backups status with --bucket/--path"
	wx get backups --backend s3 "s3-wait-$RUN_ID" \
		--bucket "$BUCKET" --path sub1 | expect_in "SUCCESS" "get backups single status"

	step "restore with --bucket/--path --wait"
	wx delete collection articles | expect_in "deleted" "delete for restore"
	wx restore s3 "s3-wait-$RUN_ID" --include Articles \
		--bucket "$BUCKET" --path sub1 --wait | expect_in "status: SUCCESS" "restore s3 --wait"
	wx get collection articles | expect_in "Articles" "collection restored"

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
	wx apply -f "/tmp/weavectl-s3-backup-$RUN_ID.yaml" | expect_in "backup/s3/s3-manifest-$RUN_ID created" "backup apply"
	# Wait for completion, then re-apply must recognize the completed
	# backup at its location ("unchanged"/"already exists"), not restart
	# it after a NotFound pre-check at the wrong location.
	wait_backup s3 "s3-manifest-$RUN_ID" sub2
	wx apply -f "/tmp/weavectl-s3-backup-$RUN_ID.yaml" | expect_in "backup/s3/s3-manifest-$RUN_ID unchanged" "backup re-apply unchanged"
	wx apply -f "/tmp/weavectl-s3-backup-$RUN_ID.yaml" | expect_in "backup already exists" "backup re-apply recognized"
	expect_fail_in "already exists" "duplicate create rejected" "$WEAVECTL" create -f "/tmp/weavectl-s3-backup-$RUN_ID.yaml"

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
	wx apply -f "/tmp/weavectl-s3-restore-$RUN_ID.yaml" | expect_in "restore/s3/s3-wait-$RUN_ID unchanged" "restore apply unchanged"

	step "filesystem control: --path --wait (original finding-3 repro shape)"
	wx create backup filesystem "fs-wait-$RUN_ID" --include Articles \
		--path subfs --wait | expect_in "status: SUCCESS" "create backup filesystem --wait"
}

run() { # the whole suite: the command surface, then the S3 backup checks.
	printf '%s━━ weavectl integration suite%s\n' "$C_BOLD" "$C_RESET"
	printf '%s   binary %s · server %s%s\n\n' "$C_DIM" "$WEAVECTL" "$SERVER" "$C_RESET"

	step "version"
	wx version | expect_in "Weaviate 1.40" "version"

	step "create collection from flags"
	wx create collection integration_books --replication-factor 1 | expect_in "collection/Integration_books created" "create collection"
	wx get collection integration_books | expect_in "Integration_books" "get collection"

	step "create -f and duplicate rejection"
	sed 's/class: Articles/class: CreateFTest/' examples/collection-articles.yaml > /tmp/weavectl-createf.yaml
	wx create -f /tmp/weavectl-createf.yaml | expect_in "collection/CreateFTest created" "create -f"
	expect_fail_in "already exists" "duplicate create rejected" "$WEAVECTL" create -f /tmp/weavectl-createf.yaml
	wx delete collection createftest | expect_in "deleted" "create -f cleanup"

	wx apply -f examples/collection-articles.yaml | expect_in "collection/Articles created" "apply create"
	expect_fail_in "already exists" "duplicate create rejected" "$WEAVECTL" create -f examples/collection-articles.yaml

	step "apply is idempotent"
	wx apply -f examples/collection-articles.yaml | expect_in "collection/Articles unchanged" "re-apply"
	wx apply -f examples/collection-articles.yaml | expect_in "collection/Articles unchanged" "re-apply again"

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
	wx apply -f /tmp/weavectl-evolved.yaml | expect_in 'property "author" added' "property add"
	wx apply -f /tmp/weavectl-evolved.yaml | expect_in "tokenization is immutable" "immutable warning"
	wx get collection Articles -o json | expect_in '"cleanupIntervalSeconds": 120' "config applied"

	step "yaml roundtrip"
	wx get collection Articles -o yaml > /tmp/weavectl-roundtrip.yaml
	wx apply -f /tmp/weavectl-roundtrip.yaml | expect_in "collection/Articles unchanged" "roundtrip apply"

	step "roles: create, converge, diff"
	wx apply -f examples/role-editor.yaml | expect_in "role/editor created" "role create"
	wx apply -f examples/role-editor.yaml | expect_in "role/editor unchanged" "role idempotent"
	wx get roles | expect_in "editor" "roles table"

	step "users: create via apply converges roles"
	wx apply -f examples/user-editor.yaml | expect_in "user/editor" "user apply"
	wx apply -f examples/user-editor.yaml | expect_in "user/editor unchanged" "user idempotent"
	wx get user editor | expect_in "editor" "user table"

	step "users: activate/deactivate via apply"
	cat > /tmp/weavectl-user-off.yaml <<-'EOF'
		userId: editor
		roles: [editor]
		active: false
	EOF
	wx apply -f /tmp/weavectl-user-off.yaml | expect_in "user/editor configured" "user deactivate"
	wx apply -f /tmp/weavectl-user-off.yaml | expect_in "user/editor unchanged" "user deactivate idempotent"
	wx get user editor | expect_in "false" "user shows inactive"
	wx apply -f examples/user-editor.yaml | expect_in "user/editor configured" "user reactivate"
	wx apply -f examples/user-editor.yaml | expect_in "user/editor unchanged" "user reactivate idempotent"

	step "aliases"
	wx apply -f examples/alias-articles.yaml | expect_in "alias/News created" "alias create"
	sed 's/class: Articles/class: Integration_books/' examples/alias-articles.yaml > /tmp/weavectl-alias2.yaml
	wx apply -f /tmp/weavectl-alias2.yaml | expect_in "alias/News configured" "alias retarget"
	wx get aliases | expect_in "Integration_books" "aliases table"

	step "tenants: multi-doc apply and status convergence"
	wx create collection MTNews --multi-tenancy | expect_in "collection/MTNews created" "mt collection"
	wx apply -f examples/tenants-mtnews.yaml | expect_in "created tenant(s): tenant-a, tenant-b" "tenant create"
	wx apply -f examples/tenants-mtnews.yaml | expect_in "tenants/MTNews unchanged" "tenant idempotent"
	wx get tenants mtnews | expect_in "tenant-a" "tenants table"
	wx get shards articles | expect_in "READY" "shards table"

	step "multi-document apply"
	wx apply -f examples/full-stack.yaml | expect_in "collection/Archive created" "multi-doc"
	wx apply -f examples/full-stack.yaml | expect_in "role/archivist unchanged" "multi-doc idempotent"

	step "create user prints one-time api key"
	wx create user keymaster | expect_in "API key (shown once" "user create key"

	step "rotate user revokes the old key"
	OLD_KEY=$(wx create user rotator | sed -n 's/^API key (shown once, store it now): //p')
	wx rotate user rotator | expect_in "user/rotator rotated" "rotate user"
	wx rotate user rotator | expect_in "API key (shown once" "rotate shows new key"
	expect_fail_in "Error from server" "old key rejected after rotate" "$WEAVECTL" get user rotator --api-key "$OLD_KEY"

	step "delete flows"
	wx delete tenant mtnews tenant-a | expect_in "deleted" "tenant delete"
	wx delete alias news | expect_in "deleted" "alias delete"
	wx delete role editor | expect_in "deleted" "role delete"
	wx delete user editor | expect_in "deleted" "user delete"
	cat > /tmp/weavectl-delete.yaml <<-'EOF'
		class: Archive
	EOF
	wx delete -f /tmp/weavectl-delete.yaml | expect_in 'collection "Archive" deleted' "delete -f"

	step "case-insensitive collection apply and canonical delete"
	cat > /tmp/weavectl-case.yaml <<-'EOF'
		class: integration_books
		description: books with different case class name
	EOF
	wx apply -f /tmp/weavectl-case.yaml | expect_in "collection/Integration_books configured" "case apply"
	wx delete collection integration_books | expect_in 'collection "Integration_books" deleted' "canonical delete"

	step "backups: manifest-driven backup, export round-trip"
	wx apply -f examples/backup-articles.yaml | expect_in "backup/filesystem/articles-snapshot created" "backup apply"
	printf '%s⏳ waiting for backup filesystem/articles-snapshot to complete...%s\n' "$C_DIM" "$C_RESET"
	for _ in $(seq 1 60); do
		$WEAVECTL get backups --backend filesystem articles-snapshot 2>/dev/null | grep -q SUCCESS && break
		sleep 2
	done
	wx get backups --backend filesystem articles-snapshot | expect_in "SUCCESS" "backup completed"
	wx apply -f examples/backup-articles.yaml | expect_in "backup/filesystem/articles-snapshot unchanged" "backup idempotent"
	wx get backups --backend filesystem | expect_in "articles-snapshot" "backups table"
	wx get backups --backend filesystem -o yaml > /tmp/weavectl-backups.yaml
	wx apply -f /tmp/weavectl-backups.yaml | expect_in "backup/filesystem/articles-snapshot unchanged" "backup roundtrip"

	step "backups: imperative create with --wait, then delete-collection + restore"
	wx create backup filesystem articles-snapshot-2 --include Articles --wait \
		| expect_in "status: SUCCESS" "create backup --wait"
	wx delete collection articles | expect_in 'collection "Articles" deleted' "delete for restore"
	wx apply -f examples/restore-articles.yaml | expect_in "restore/filesystem/articles-snapshot created" "restore apply"
	printf '%s⏳ waiting for restore filesystem/articles-snapshot to complete...%s\n' "$C_DIM" "$C_RESET"
	for _ in $(seq 1 60); do
		$WEAVECTL restore status filesystem articles-snapshot 2>/dev/null | grep -q SUCCESS && break
		sleep 2
	done
	wx restore status filesystem articles-snapshot | expect_in "SUCCESS" "restore completed"
	wx get collection articles | expect_in "Articles" "collection restored"

	step "error paths"
	expect_fail_in "(NotFound)" "get missing collection" "$WEAVECTL" get collection does_not_exist
	expect_fail_in "(NotFound)" "delete missing collection" "$WEAVECTL" delete collection does_not_exist
	expect_fail_in "unknown output format" "invalid -o rejected" "$WEAVECTL" get collections -o invalid_format

	step "s3 backup checks (only if the server has backup-s3)"
	if curl -sf -H "Authorization: Bearer $API_KEY" "${SERVER%/}/v1/meta" | grep -q '"backup-s3"'; then
		s3_checks
	else
		printf "%s⏭ skipped: backup-s3 not enabled on %s (make integration's MinIO stack has it)%s\n" \
			"$C_YELLOW" "$SERVER" "$C_RESET"
	fi

	local elapsed=$((SECONDS - T0))
	printf '\n%s━━ ✓ ALL INTEGRATION CHECKS PASSED — %d checks in %dm %02ds%s\n\n' \
		"$C_GREEN" "$CHECKS" $((elapsed / 60)) $((elapsed % 60)) "$C_RESET"
}

export WEAVECTL_SERVER="$SERVER" WEAVECTL_API_KEY="$API_KEY"

case "${1:-run}" in
dev-up) dev_up; exit 0 ;;
dev-down) dev_down; exit 0 ;;
s3-up) s3_up; exit 0 ;;
s3-down) s3_down; exit 0 ;;
run|"") run ;;
*)
	printf '%susage: %s {run|dev-up|dev-down|s3-up|s3-down}%s\n\n' "$C_BOLD" "$0" "$C_RESET" >&2
	printf '  run        full check suite against a running server (default)\n' >&2
	printf '  dev-up     start the light dev stack (filesystem backups only)\n' >&2
	printf '  dev-down   stop the dev stack\n' >&2
	printf '  s3-up      start minio + weaviate with backup-s3\n' >&2
	printf '  s3-down    stop the minio stack\n' >&2
	exit 2
	;;
esac
