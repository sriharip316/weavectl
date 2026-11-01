# weavectl

A kubectl-inspired control-plane CLI for [Weaviate](https://weaviate.io)
databases, built for DBAs. It manages schema-level objects — **collections,
roles, users, aliases, tenants** — orchestrates **backups and restores**,
and inspects cluster status (**shards, nodes**) over the REST API only.
Apart from backup and restore it never touches object data (`/objects`,
`/batch`, `/graphql` are deliberately absent).

```
$ weavectl get collections
NAME              VECTORIZER  REPLICATION  SHARDS  PROPERTIES  MULTI-TENANT
----------------  ----------  -----------  ------  ----------  ------------
Articles          none        1            1       3           false
MTNews            none        1            1       0           true
```

## Install

```bash
# Install via Go:
go install github.com/sriharip316/weavectl@latest

# Or build from source:
make build          # builds ./bin/weavectl
make install        # builds and copies to ~/.local/bin/weavectl
```

Requires Go 1.26+. Developed and verified against Weaviate **1.40.1**.

## Configuration

Precedence, highest first:

1. flags: `--server`, `--api-key` (also `--context`, `--config`, `--no-config`)
2. environment: `WEAVECTL_SERVER`, `WEAVECTL_API_KEY`
3. the context named by `current-context` in `~/.weavectl/config.yaml`
4. default: `http://localhost:8080`

The config file holds named contexts (a mini-kubeconfig):

```yaml
current-context: dev
contexts:
  dev:
    server: http://localhost:8080
    api-key: root-key-1
  prod:
    server: https://weaviate.example.com
    api-key: ${PROD_KEY}   # literal value; expand secrets out of band
```

Use `--no-config` to bypass `~/.weavectl/config.yaml` entirely.

Credentials are sent as `Authorization: Bearer <key>`, which covers both
Weaviate API keys and OIDC bearer tokens.

> The role and db-user commands need a server with RBAC and user management
> enabled; the backup/restore commands need a backup module. For a local
> dev container:
>
> ```bash
> make dev-up   # runs docker.io/semitechnologies/weaviate:1.40.1 with podman
> ```
>
> ```env
> AUTHENTICATION_APIKEY_ENABLED=true
> AUTHENTICATION_APIKEY_ALLOWED_KEYS=root-key-1
> AUTHENTICATION_APIKEY_USERS=admin@example.com
> AUTHENTICATION_DB_USERS_ENABLED=true
> AUTHORIZATION_RBAC_ENABLED=true
> AUTHORIZATION_RBAC_ROOT_USERS=admin@example.com
> ENABLE_MODULES=backup-filesystem
> BACKUP_FILESYSTEM_PATH=/tmp/weaviate-backups
> ```
>
> Note: with the older admin-list mode (`AUTHORIZATION_ADMINLIST_ENABLED`)
> the `/v1/authz/*` endpoints accept writes but silently discard them —
> RBAC mode is required for role management to actually persist.

## Commands

### get — list or show objects

```bash
weavectl get collections [NAME]      # table by default (also accepts singular: collection)
weavectl get roles [NAME]            # also accepts singular: role
weavectl get users [NAME]            # also accepts singular: user
weavectl get aliases [NAME]          # also accepts singular: alias
weavectl get tenants COLLECTION      # tenants within a multi-tenant collection
weavectl get shards COLLECTION       # shard states and vector queue sizes
weavectl get nodes                   # cluster node health, versions, and modes
weavectl get backups --backend B [ID] # backups on a backend (ID shows one status; --bucket/--path for non-default locations)
```

Both singular and plural resource names are supported (`collection`/`collections`, `role`/`roles`, `user`/`users`, `alias`/`aliases`, `tenant`/`tenants`, `shard`/`shards`, `node`/`nodes`).

Output formats with `-o`:
- `-o table` (default, or `-o wide`): aligned columnar output.
  - `collections`: `NAME`, `VECTORIZER`, `REPLICATION`, `SHARDS`, `PROPERTIES`, `MULTI-TENANT`
  - `roles`: `NAME`, `PERMISSIONS`
  - `users`: `USER ID`, `TYPE`, `ACTIVE`, `ROLES`
  - `aliases`: `ALIAS`, `COLLECTION`
  - `tenants`: `NAME`, `STATUS`
  - `shards`: `NAME`, `STATUS`, `VECTOR QUEUE`
  - `nodes`: `NAME`, `STATUS`, `VERSION`, `MODE`, `SHARDS`
  - `backups`: `ID`, `STATUS`, `CLASSES`, `SIZE (GIB)`, `STARTED`, `COMPLETED`, `BASE`
- `-o json`: raw API JSON (single object if named, JSON array if listing).
- `-o yaml` (or `-o yml`): API-native YAML (single document or `---` separated).

Because `-o yaml` outputs API-native definitions, output round-trips cleanly:
```bash
weavectl get collection Articles -o yaml > articles.yaml
# edit articles.yaml ...
weavectl apply -f articles.yaml
```

### create — insert new objects

```bash
weavectl create -f manifest.yaml            # any kind, from file or "-" (stdin)
weavectl create collection NAME [--replication-factor N] [--vectorizer MODULE]
                                            [--shard-count N] [--multi-tenancy]
weavectl create user NAME                   # prints the API key exactly once
weavectl create alias NAME --collection CLASS
weavectl create tenant COLLECTION NAME... [--activity-status ACTIVE]
weavectl create backup BACKEND ID [--include A,B | --exclude C] [--wait]
```

- `create` fails if the object already exists (use `apply` to converge).
- Roles have structured permissions and are created from manifests via `weavectl create -f role.yaml`.
- `create tenant` accepts multiple tenant names in a single invocation.
- Creating a user outputs the generated API key once; Weaviate does not allow retrieving it again later.

### apply — create or update to match a manifest

```bash
weavectl apply -f manifest.yaml             # from file or "-" (stdin)
```

Manifests are **raw Weaviate definitions** (JSON or YAML; multi-document
YAML separated by `---`). The kind is detected from the shape:
- `class` → collection
- `name` + `permissions` → role
- `userId` → user
- `alias` + `class` → alias
- `collection` + `tenants` → tenants
- `backend` + `id` → backup
- or set explicitly with a `kind:` key (`kind: collection`, `kind: role`, etc.)

A single apply may issue several API requests, handled automatically:

- **collection**: if missing, `POST /v1/schema`. Otherwise one
  `PUT /v1/schema/{class}` carrying mutable settings (the server requires
  the full definition — weavectl assembles it from the live state plus
  your manifest), then one `POST .../properties` per **new** property.
  Existing properties are immutable except `description`; attempts to
  change anything else produce a warning, not an error.
- **role**: if missing, created. Otherwise a permission **diff** drives
  `add-permissions` / `remove-permissions`. Omitted resource fields equal
  to server defaults (`"object": "*"`, `"scope": "match"`, …) compare as
  equal to explicit ones, so repeated applies are true no-ops.
- **user**: if missing, created and the **one-time API key is printed**.
  Role assignments converge to the manifest (assign missing, revoke
  unlisted). An optional `active: true|false` key converges the login
  state: `false` deactivates the user (their API key is kept, so
  `active: true` fully restores them), `true` re-activates; omitting the
  key leaves the current state untouched. A new user created with
  `active: false` is deactivated in the same apply.
- **alias**: created, or retargeted (`PUT /v1/aliases/{name}`).
- **tenants**: missing tenants are created, changed activity statuses are
  updated. Tenants are **never deleted implicitly** — use
  `weavectl delete tenant`.
- **backup / restore**: started if unknown; a running or completed attempt
  reports `unchanged`; a `FAILED`/`CANCELED` attempt is restarted
  (see [backup and restore](#backup-and-restore)).

Results report `<kind>/<name> created`, `<kind>/<name> configured`, or `<kind>/<name> unchanged`.

#### Example Manifests (`examples/`)

- [`examples/collection-articles.yaml`](examples/collection-articles.yaml): collection schema with indexing, bm25, replication, and properties.
- [`examples/alias-articles.yaml`](examples/alias-articles.yaml): alias targeting a collection.
- [`examples/role-editor.yaml`](examples/role-editor.yaml): RBAC role with fine-grained permissions.
- [`examples/user-editor.yaml`](examples/user-editor.yaml): database user with role assignments.
- [`examples/tenants-mtnews.yaml`](examples/tenants-mtnews.yaml): tenants in a multi-tenant collection.
- [`examples/backup-articles.yaml`](examples/backup-articles.yaml): a backup of one collection on the filesystem backend.
- [`examples/restore-articles.yaml`](examples/restore-articles.yaml): a restore from that backup.
- [`examples/full-stack.yaml`](examples/full-stack.yaml): multi-document YAML applying a collection, role, and user in a single pass (`weavectl apply -f examples/full-stack.yaml`).

### delete — remove objects

```bash
weavectl delete collection NAME
weavectl delete role NAME
weavectl delete user NAME
weavectl delete alias NAME
weavectl delete tenant COLLECTION NAME...
weavectl delete backup BACKEND ID    # cancels an in-progress backup
weavectl delete -f manifest.yaml            # from file or "-" (stdin); kind+name from the file
```

No confirmation prompts (kubectl behavior); deleting a collection deletes
all its data. Name lookups are case-insensitive and confirm the canonical
deleted object. Multiple tenants can be deleted in a single command.

### rotate — replace a secret in place

```bash
weavectl rotate user NAME     # revokes the current API key, prints the new one once
```

Rotating a db user's API key **revokes the old key immediately** — every
consumer of it (services, scripts, config files) stops authenticating and
must be updated. The new key is printed exactly once, like `create user`;
store it now.

Rotation is deliberately imperative-only: there is no `rotateKey` manifest
field and apply never rotates keys. The new key is a one-time secret that
must be captured out of band (secret manager, workload redeploy), and the
server keeps no "already rotated" state, so a manifest field could only
mean "rotate on every apply" — re-applying a shared manifest would
silently revoke live credentials. Treat rotation as an operator action:
run it explicitly, then record the new key where your workloads get it.

### backup and restore

Backups and restores are manifest-driven like everything else. A backup
manifest is the raw backup API body plus `backend`/`id` scoping fields:

```yaml
# examples/backup-articles.yaml
kind: Backup          # optional: backend+id is detected as a backup
backend: filesystem
id: articles-snapshot
include: [Articles]   # or exclude: [...]; wildcards allowed; omit for all
config:
  CPUPercentage: 60
```

```bash
weavectl apply -f examples/backup-articles.yaml   # start it
weavectl get backups --backend filesystem         # list: ID, STATUS, SIZE, …
weavectl get backups --backend filesystem articles-snapshot   # one status (--bucket/--path if stored outside the default)
weavectl delete backup filesystem articles-snapshot           # cancel
```

A restore manifest selects the backup the same way — but always needs the
explicit `kind: Restore` key, since `backend`+`id` alone means a backup:

```yaml
# examples/restore-articles.yaml
kind: Restore
backend: filesystem
id: articles-snapshot
include: [Articles]
overwriteAlias: true
```

```bash
weavectl apply -f examples/restore-articles.yaml  # start restoring
weavectl restore status filesystem articles-snapshot
weavectl restore cancel filesystem articles-snapshot
```

Behavior:

- Applying a **backup** starts it if the id is unknown; a backup that is
  running or already completed reports `unchanged`; a `FAILED`/`CANCELED`
  attempt is restarted. Backups are one-shot — there is no `configured`.
- Applying a **restore** works the same way: start, or report
  `unchanged` when a restore is running or already succeeded.
- Restore requirements (enforced by the server): the target cluster must
  have the same node count as the source, node names must match (a
  `node_mapping` body field can remap them), and the restored collections
  must not already exist.
- Backend credentials live in the server's configuration, not in weavectl.
  For local testing `make dev-up` enables the `filesystem` backend module;
  `make integration` runs the whole suite against a local MinIO stack with
  the `s3` backend enabled, validating the whole backup/restore flow
  against it.
- Backups stored at a non-default location must pass that location to
  every status lookup and cancel: `--bucket`/`--path` (or `config.bucket`/
  `config.path` in a manifest) are accepted by `create backup`, `restore`,
  `get backups ID`, `restore status`, `restore cancel`, `delete backup`,
  and `--wait` — the server only finds the backup there when asked.
- Backup ids must be URL-safe filesystem paths (lowercase, numbers,
  `_`, `-`); validated client-side.

Imperative quick forms (fire-and-forget; `--wait` polls to completion and
exits non-zero on `FAILED`/`CANCELED`):

```bash
weavectl create backup filesystem snap-1 [--include A,B | --exclude C] \
    [--include-users '*'] [--include-roles editor] [--cpu-percentage N] \
    [--compression-level BestSpeed] [--bucket B] [--path P] \
    [--incremental-of base-id] [--wait] [--wait-timeout 10m]
weavectl restore filesystem snap-1 [--include A,B | --exclude C] \
    [--node-mapping old=new] [--overwrite-alias] [--bucket B] [--path P] \
    [--restore-roles noRestore|all] [--restore-users noRestore|all] \
    [--wait] [--wait-timeout 10m]
```

`weavectl get backups --backend B -o yaml` exports backup definitions as
manifests (with the backed-up classes as `include`), so exported YAML
re-applies cleanly — see the backup round-trip in `scripts/integration.sh`.

### completion — shell autocompletion

```bash
weavectl completion bash > /etc/bash_completion.d/weavectl
weavectl completion zsh > "${fpath[1]}/_weavectl"
weavectl completion fish > ~/.config/fish/completions/weavectl.fish
weavectl completion powershell
```

### version

```bash
weavectl version    # client version + server version and endpoint from /v1/meta
```

## Name handling

Weaviate capitalizes the first letter of collection and alias names
(`my_collection` is stored as `My_collection`). weavectl mirrors that
client-side and additionally resolves names case-insensitively when
referring to existing objects, so `get tenants mtnews` finds `MTNews`, and
`delete collection articles` deletes `Articles`.

## Server behaviors worth knowing (verified on 1.40.1)

- Success codes vary: collection create returns `200`, role create `201`
  with empty body, deletes `200`/`204`. weavectl treats any 2xx as success.
- Error bodies come in (at least) three shapes (`{"error":[...]}`,
  `{"allowed":null,"error":[...]}`, `{"code":…,"message":…}`); 404s often
  have empty bodies. All are normalized to
  `Error from server (NotFound): …`-style messages.
- Collection PUT accepts only the **full** definition including every
  existing property, and only description changes within properties.
- `dataType: string` is stored and read back as `text`.
- Tenant `activityStatus` is written as `ACTIVE`/`INACTIVE`/`OFFLOADED`
  but read back as `HOT`/`COLD`/`FROZEN`; weavectl treats them as
  equivalent when diffing.
- Role permissions read back with all default resource fields filled in
  (`"*"` etc.); the apply diff accounts for this.
- Backups and restores are asynchronous: the create/restore POST returns
  immediately with `status: STARTED`, and progress is tracked on the
  status endpoints (`STARTED` → `TRANSFERRING` → `TRANSFERRED` →
  `FINALIZING` → `SUCCESS`, or `FAILED`/`CANCELED`). A restore status is
  `404` until a restore has been initiated for that backup id.
- Structured YAML output normalizes JSON numbers back to real integers and
  floats so that exported manifests round-trip without type-mismatch errors.

## Development

```bash
make build          # build ./bin/weavectl
make test           # unit tests (fake server, no container needed)
make coverage       # run tests and print function coverage summary
make dev-up         # start Weaviate 1.40.1 in podman (RBAC + db users on)
make dev-logs       # tail logs from the dev container
make integration    # full end-to-end run incl. S3 backups against local MinIO
make dev-down       # stop and remove the container
make vet            # run go vet
make fmt            # run go fmt
make lint           # run staticcheck, golangci-lint, govulncheck
make clean          # clean bin, dist, and coverage.out
```

Project layout:

```
cmd/               cobra commands (get/create/apply/delete/rotate/restore/version)
internal/client/   HTTP client: bearer auth, error normalization, 404 detection
internal/weaviate/ typed wrappers per resource group
internal/manifest/ manifest loading, multi-doc YAML, kind detection
internal/apply/    per-kind create-or-update orchestration
internal/printer/  table / json / yaml output
internal/config/   flags > env > config-file contexts
examples/          ready-to-apply manifest definitions
scripts/           integration test suite
```

## Scope and non-goals

No data-plane operations (objects, batches, GraphQL, search, aggregate).
Classifications, replication repair, OIDC groups, namespaces, and
deactivate's `revoke_key` option are not implemented
(the REST endpoints exist in `weaviate_api.json` if you want to extend;
user rotate-key is implemented as `weavectl rotate user`).
