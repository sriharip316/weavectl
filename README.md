# weavectl

A kubectl-inspired control-plane CLI for [Weaviate](https://weaviate.io)
databases, built for DBAs. It manages schema-level objects — **collections,
roles, users, aliases, tenants** — over the REST API only. It never touches
object data (`/objects`, `/batch`, `/graphql` are deliberately absent).

```
$ weavectl get collections
NAME              VECTORIZER  REPLICATION  SHARDS  PROPERTIES  MULTI-TENANT
----------------  ----------  -----------  ------  ----------  ------------
Articles          none        1            1       3           false
MTNews            none        1            1       0           true
```

## Install

```
make install        # builds and copies to ~/.local/bin
```

Requires Go 1.26+. Developed and verified against Weaviate **1.40.1**.

## Configuration

Precedence, highest first:

1. flags: `--server`, `--api-key` (also `--context`, `--config`)
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

Credentials are sent as `Authorization: Bearer <key>`, which covers both
Weaviate API keys and OIDC bearer tokens.

> The role and db-user commands need a server with RBAC and user management
> enabled. For a local dev container:
>
> ```
> make dev-up   # runs weaviate:1.40.1 with the flags below
> ```
>
> ```
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

```
weavectl get collections [NAME]      # table by default
weavectl get roles [NAME]
weavectl get users [NAME]
weavectl get aliases [NAME]
weavectl get tenants COLLECTION
weavectl get shards COLLECTION
weavectl get nodes                   # cluster health
```

`-o json` / `-o yaml` prints the API-native definition, so output
round-trips: `weavectl get collection Articles -o yaml > a.yaml`, edit,
`weavectl apply -f a.yaml`.

### create — insert new objects

```
weavectl create -f manifest.yaml            # any kind, from file or "-"
weavectl create collection NAME [--replication-factor N] [--vectorizer MODULE]
                                            [--shard-count N] [--multi-tenancy]
weavectl create user NAME                   # prints the API key exactly once
weavectl create alias NAME --collection CLASS
weavectl create tenant COLLECTION NAME... [--activity-status ACTIVE]
```

`create` fails if the object already exists (use `apply` to converge).

### apply — create or update to match a manifest

```
weavectl apply -f manifest.yaml
```

Manifests are **raw Weaviate definitions** (JSON or YAML; multi-document
YAML separated by `---`). See `examples/`. The kind is detected from the
shape (`class` → collection, `name`+`permissions` → role, `userId` → user,
`alias` → alias, `collection`+`tenants` → tenants), or set explicitly with
a `kind:` key.

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

### delete — remove objects

```
weavectl delete collection NAME
weavectl delete role NAME
weavectl delete user NAME
weavectl delete alias NAME
weavectl delete tenant COLLECTION NAME...
weavectl delete -f manifest.yaml     # kind+name from the file
```

No confirmation prompts (kubectl behavior); deleting a collection deletes
all its data.

### version

```
weavectl version    # client + server version and endpoint
```

## Name handling

Weaviate capitalizes the first letter of collection and alias names
(`my_collection` is stored as `My_collection`). weavectl mirrors that
client-side and additionally resolves names case-insensitively when
referring to existing objects, so `get tenants mtnews` finds `MTNews`.

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

## Development

```
make build          # build ./bin/weavectl
make test           # unit tests (fake server, no container needed)
make dev-up         # start Weaviate 1.40.1 in podman (RBAC + db users on)
make integration    # full end-to-end run against the dev container
make dev-down       # stop the container
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
```

## Scope and non-goals

No data-plane operations (objects, batches, GraphQL, search, aggregate).
Backups, classifications, replication repair, OIDC groups, namespaces, and
user activate/deactivate/rotate-key are not implemented (the REST
endpoints exist in `weaviate_api.json` if you want to extend).
