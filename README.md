# weavectl

A kubectl-inspired control-plane CLI for [Weaviate](https://weaviate.io)
databases, built for DBAs. It manages schema-level objects — **collections,
roles, users, aliases, tenants** — and inspects cluster status (**shards,
nodes**) over the REST API only. It never touches object data (`/objects`,
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
> enabled. For a local dev container:
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
  unlisted).
- **alias**: created, or retargeted (`PUT /v1/aliases/{name}`).
- **tenants**: missing tenants are created, changed activity statuses are
  updated. Tenants are **never deleted implicitly** — use
  `weavectl delete tenant`.

Results report `<kind>/<name> created`, `<kind>/<name> configured`, or `<kind>/<name> unchanged`.

#### Example Manifests (`examples/`)

- [`examples/collection-articles.yaml`](examples/collection-articles.yaml): collection schema with indexing, bm25, replication, and properties.
- [`examples/alias-articles.yaml`](examples/alias-articles.yaml): alias targeting a collection.
- [`examples/role-editor.yaml`](examples/role-editor.yaml): RBAC role with fine-grained permissions.
- [`examples/user-editor.yaml`](examples/user-editor.yaml): database user with role assignments.
- [`examples/tenants-mtnews.yaml`](examples/tenants-mtnews.yaml): tenants in a multi-tenant collection.
- [`examples/full-stack.yaml`](examples/full-stack.yaml): multi-document YAML applying a collection, role, and user in a single pass (`weavectl apply -f examples/full-stack.yaml`).

### delete — remove objects

```bash
weavectl delete collection NAME
weavectl delete role NAME
weavectl delete user NAME
weavectl delete alias NAME
weavectl delete tenant COLLECTION NAME...
weavectl delete -f manifest.yaml            # from file or "-" (stdin); kind+name from the file
```

No confirmation prompts (kubectl behavior); deleting a collection deletes
all its data. Name lookups are case-insensitive and confirm the canonical
deleted object. Multiple tenants can be deleted in a single command.

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
- Structured YAML output normalizes JSON numbers back to real integers and
  floats so that exported manifests round-trip without type-mismatch errors.

## Development

```bash
make build          # build ./bin/weavectl
make test           # unit tests (fake server, no container needed)
make coverage       # run tests and print function coverage summary
make dev-up         # start Weaviate 1.40.1 in podman (RBAC + db users on)
make dev-logs       # tail logs from the dev container
make integration    # full end-to-end run against the dev container
make dev-down       # stop and remove the container
make vet            # run go vet
make fmt            # run go fmt
make lint           # run staticcheck, golangci-lint, govulncheck
make clean          # clean bin, dist, and coverage.out
```

Project layout:

```
cmd/               cobra commands (get/create/apply/delete/version)
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
Backups, classifications, replication repair, OIDC groups, namespaces, and
user activate/deactivate/rotate-key are not implemented (the REST
endpoints exist in `weaviate_api.json` if you want to extend).
