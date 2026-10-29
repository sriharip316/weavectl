#!/usr/bin/env bash
# End-to-end exercise of weavectl against a live Weaviate (make integration).
# Any failure aborts: assertions use grep against actual output.
set -euo pipefail

WEAVECTL="${WEAVECTL:-./bin/weavectl}"
cd "$(dirname "$0")/.."

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
cat > /tmp/weavectl-evolved.yaml <<'EOF'
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
cat > /tmp/weavectl-user-off.yaml <<'EOF'
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
cat > /tmp/weavectl-delete.yaml <<'EOF'
class: Archive
EOF
$WEAVECTL delete -f /tmp/weavectl-delete.yaml | expect_in "collection \"Archive\" deleted" "delete -f"

step "case-insensitive collection apply and canonical delete"
cat > /tmp/weavectl-case.yaml <<'EOF'
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

printf '\nALL INTEGRATION CHECKS PASSED\n'
