package apply

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sriharip316/weavectl/internal/client"
	"github.com/sriharip316/weavectl/internal/manifest"
)

// fakeWeaviate mimics the server behaviors apply depends on: 404s with
// empty bodies, permission default-filling on read, full-body collection
// PUTs, and add/remove-permissions endpoints.
type fakeWeaviate struct {
	mu          sync.Mutex
	roles       map[string][]map[string]any
	collections map[string]map[string]any
	users       map[string][]string // userID -> roles
	aliases     map[string]string   // alias -> class
	tenants     map[string][]map[string]any
	backups     map[string]*backupState // "backend/id" -> scripted statuses
	restores    map[string]*backupState
	// schemaGetStatus, when non-zero, is returned by GET /v1/schema/{name}
	// to simulate server errors during collection lookups.
	schemaGetStatus int
	calls           []string
}

// backupState scripts the statuses a backup or restore moves through;
// each status GET returns the next entry, sticking on the last.
type backupState struct {
	backend  string
	id       string
	classes  []string
	statuses []string
	gets     int
	errMsg   string
}

func (s *backupState) advance() string {
	i := s.gets
	if i >= len(s.statuses) {
		i = len(s.statuses) - 1
	}
	s.gets++
	return s.statuses[i]
}

func (s *backupState) statusResponse() map[string]any {
	status := s.advance()
	resp := map[string]any{"id": s.id, "backend": s.backend, "status": status}
	if status == "SUCCESS" {
		resp["completedAt"] = "2026-01-01T10:01:00Z"
	}
	if s.errMsg != "" && (status == "FAILED" || status == "CANCELED") {
		resp["error"] = s.errMsg
	}
	return resp
}

func newFake() *fakeWeaviate {
	return &fakeWeaviate{
		roles:       map[string][]map[string]any{},
		collections: map[string]map[string]any{},
		users:       map[string][]string{},
		aliases:     map[string]string{},
		tenants:     map[string][]map[string]any{},
		backups:     map[string]*backupState{},
		restores:    map[string]*backupState{},
	}
}

func (f *fakeWeaviate) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeWeaviate) handler() http.Handler {
	mux := http.NewServeMux()

	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	writeErr := func(w http.ResponseWriter, status int, msg string) {
		writeJSON(w, status, map[string]any{"error": []map[string]string{{"message": msg}}})
	}

	mux.HandleFunc("GET /v1/authz/roles/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		name := r.PathValue("name")
		perms, ok := f.roles[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": name, "permissions": perms})
	})

	mux.HandleFunc("POST /v1/authz/roles", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var role struct {
			Name        string           `json:"name"`
			Permissions []map[string]any `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&role)
		if _, exists := f.roles[role.Name]; exists {
			writeErr(w, http.StatusConflict, "role exists")
			return
		}
		f.roles[role.Name] = fillDefaults(role.Permissions)
		f.record("POST /authz/roles %s", role.Name)
		w.WriteHeader(http.StatusCreated)
	})

	mux.HandleFunc("POST /v1/authz/roles/{name}/add-permissions", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		name := r.PathValue("name")
		var body struct {
			Permissions []map[string]any `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.roles[name] = append(f.roles[name], fillDefaults(body.Permissions)...)
		f.record("POST add-permissions %s x%d", name, len(body.Permissions))
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /v1/authz/roles/{name}/remove-permissions", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		name := r.PathValue("name")
		var body struct {
			Permissions []map[string]any `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.record("POST remove-permissions %s x%d", name, len(body.Permissions))
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /v1/schema/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.schemaGetStatus != 0 {
			writeErr(w, f.schemaGetStatus, "schema lookup failed")
			return
		}
		col, ok := f.collections[r.PathValue("name")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, col)
	})

	mux.HandleFunc("GET /v1/schema", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		classes := []map[string]any{}
		for _, c := range f.collections {
			classes = append(classes, c)
		}
		writeJSON(w, http.StatusOK, map[string]any{"classes": classes})
	})

	mux.HandleFunc("POST /v1/schema", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var col map[string]any
		_ = json.NewDecoder(r.Body).Decode(&col)
		name, _ := col["class"].(string)
		f.collections[name] = col
		f.record("POST /schema %s", name)
		writeJSON(w, http.StatusOK, col)
	})

	mux.HandleFunc("PUT /v1/schema/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var col map[string]any
		_ = json.NewDecoder(r.Body).Decode(&col)
		name := r.PathValue("name")
		f.collections[name] = col
		f.record("PUT /schema/%s", name)
		writeJSON(w, http.StatusOK, col)
	})

	mux.HandleFunc("POST /v1/schema/{name}/properties", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		name := r.PathValue("name")
		var prop map[string]any
		_ = json.NewDecoder(r.Body).Decode(&prop)
		col := f.collections[name]
		props, _ := col["properties"].([]any)
		col["properties"] = append(props, prop)
		f.record("POST /schema/%s/properties %v", name, prop["name"])
		writeJSON(w, http.StatusOK, prop)
	})

	mux.HandleFunc("GET /v1/users/db/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := r.PathValue("id")
		roles, ok := f.users[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"userId": id, "roles": roles, "active": true})
	})

	mux.HandleFunc("POST /v1/users/db/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := r.PathValue("id")
		f.users[id] = []string{}
		f.record("POST /users/db/%s", id)
		writeJSON(w, http.StatusCreated, map[string]any{"apikey": "key-" + id})
	})

	mux.HandleFunc("POST /v1/authz/users/{id}/assign", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body struct {
			Roles []string `json:"roles"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.users[r.PathValue("id")] = append(f.users[r.PathValue("id")], body.Roles...)
		f.record("POST assign %v", body.Roles)
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /v1/authz/users/{id}/revoke", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body struct {
			Roles []string `json:"roles"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.record("POST revoke %v", body.Roles)
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /v1/aliases", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []map[string]any{}
		for a, c := range f.aliases {
			out = append(out, map[string]any{"alias": a, "class": c})
		}
		writeJSON(w, http.StatusOK, map[string]any{"aliases": out})
	})

	mux.HandleFunc("POST /v1/aliases", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var a struct{ Alias, Class string }
		_ = json.NewDecoder(r.Body).Decode(&a)
		f.aliases[a.Alias] = a.Class
		f.record("POST /aliases %s->%s", a.Alias, a.Class)
		writeJSON(w, http.StatusOK, a)
	})

	mux.HandleFunc("PUT /v1/aliases/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body struct{ Class string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.aliases[r.PathValue("name")] = body.Class
		f.record("PUT /aliases/%s->%s", r.PathValue("name"), body.Class)
		writeJSON(w, http.StatusOK, body)
	})

	mux.HandleFunc("GET /v1/schema/{name}/tenants", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, http.StatusOK, f.tenants[r.PathValue("name")])
	})

	mux.HandleFunc("POST /v1/schema/{name}/tenants", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var ts []map[string]any
		_ = json.NewDecoder(r.Body).Decode(&ts)
		f.tenants[r.PathValue("name")] = append(f.tenants[r.PathValue("name")], ts...)
		f.record("POST tenants %s x%d", r.PathValue("name"), len(ts))
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("PUT /v1/schema/{name}/tenants", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.record("PUT tenants %s", r.PathValue("name"))
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /v1/backups/{backend}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		backend := r.PathValue("backend")
		id, _ := body["id"].(string)
		classes := []string{}
		if inc, ok := body["include"].([]any); ok {
			for _, class := range inc {
				classes = append(classes, fmt.Sprint(class))
			}
		}
		f.backups[backend+"/"+id] = &backupState{
			backend: backend, id: id, classes: classes,
			statuses: []string{"TRANSFERRING", "SUCCESS"},
		}
		f.record("POST /backups/%s %s include=%v", backend, id, body["include"])
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "classes": classes, "backend": backend, "status": "STARTED",
		})
	})

	mux.HandleFunc("GET /v1/backups/{backend}/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		st, ok := f.backups[r.PathValue("backend")+"/"+r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, st.statusResponse())
	})

	mux.HandleFunc("POST /v1/backups/{backend}/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		backend, id := r.PathValue("backend"), r.PathValue("id")
		f.restores[backend+"/"+id] = &backupState{
			backend: backend, id: id,
			statuses: []string{"TRANSFERRING", "SUCCESS"},
		}
		f.record("POST restore %s/%s body=%v", backend, id, body)
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "backend": backend, "classes": []string{"Articles"}, "status": "STARTED",
		})
	})

	mux.HandleFunc("GET /v1/backups/{backend}/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		st, ok := f.restores[r.PathValue("backend")+"/"+r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, st.statusResponse())
	})

	return mux
}

// fillDefaults mirrors Weaviate: omitted resource fields come back as "*"
// (or the domain's default) when the permission is read back.
func fillDefaults(perms []map[string]any) []map[string]any {
	domainFills := map[string]map[string]any{
		"data":      {"object": "*", "tenant": "*"},
		"tenants":   {"tenant": "*"},
		"replicate": {"shard": "*"},
		"roles":     {"scope": "match"},
		"aliases":   {"collection": "*"},
		"groups":    {"groupType": "oidc"},
		"nodes":     {"verbosity": "minimal"},
	}
	out := make([]map[string]any, 0, len(perms))
	for _, p := range perms {
		q := map[string]any{"action": p["action"]}
		for k, v := range p {
			if k == "action" {
				continue
			}
			res, _ := v.(map[string]any)
			filled := map[string]any{}
			maps.Copy(filled, res)
			for def, val := range domainFills[k] {
				if _, ok := filled[def]; !ok {
					filled[def] = val
				}
			}
			q[k] = filled
		}
		out = append(out, q)
	}
	return out
}

func doc(t *testing.T, yaml string) []manifest.Document {
	t.Helper()
	docs, err := manifest.Parse([]byte(yaml), false)
	if err != nil {
		t.Fatal(err)
	}
	return docs
}

func applyDocs(t *testing.T, f *fakeWeaviate, yaml string) ([]Result, error) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return Apply(client.New(srv.URL, ""), doc(t, yaml))
}

func TestRoleApplyCreate(t *testing.T) {
	f := newFake()
	res, err := applyDocs(t, f, "name: dev\npermissions:\n  - action: read_data\n    data: {collection: A}\n")
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != "created" || len(f.calls) != 1 {
		t.Fatalf("res=%+v calls=%v", res, f.calls)
	}
}

// The heart of role apply: a role stored in server-normalized form must
// compare equal to the user's minimal intent — no oscillating adds/removes.
func TestRoleApplyIdempotentAgainstServerDefaults(t *testing.T) {
	f := newFake()
	fakeRole := "name: dev\npermissions:\n  - action: read_data\n    data: {collection: A}\n  - action: read_roles\n    roles: {role: '*'}\n  - action: read_aliases\n    aliases: {alias: a}\n"
	if _, err := applyDocs(t, f, fakeRole); err != nil {
		t.Fatal(err)
	}

	res, err := applyDocs(t, f, fakeRole)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != "unchanged" {
		t.Fatalf("re-apply action = %q, want unchanged (calls=%v)", res[0].Action, f.calls)
	}
	if len(f.calls) != 1 { // only the initial GET
		t.Fatalf("expected no mutations, got %v", f.calls)
	}
}

func TestRoleApplyDiff(t *testing.T) {
	f := newFake()
	if _, err := applyDocs(t, f, "name: dev\npermissions:\n  - action: read_data\n    data: {collection: A}\n"); err != nil {
		t.Fatal(err)
	}

	// Drop read_data, add update_data.
	res, err := applyDocs(t, f, "name: dev\npermissions:\n  - action: update_data\n    data: {collection: B}\n")
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != "configured" {
		t.Fatalf("action = %q", res[0].Action)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "add-permissions dev x1") {
		t.Errorf("missing add call: %v", f.calls)
	}
	if !strings.Contains(joined, "remove-permissions dev x1") {
		t.Errorf("missing remove call: %v", f.calls)
	}
	if strings.Index(joined, "add-permissions") > strings.Index(joined, "remove-permissions") {
		t.Errorf("adds must happen before removes: %v", f.calls)
	}
}

func TestCollectionApplyCreateAndUpdate(t *testing.T) {
	f := newFake()
	res, err := applyDocs(t, f, "class: Articles\nproperties:\n  - name: title\n    dataType: [text]\n")
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != "created" {
		t.Fatalf("action = %q", res[0].Action)
	}

	// Add a property and change a mutable setting.
	res, err = applyDocs(t, f, `class: Articles
invertedIndexConfig:
  cleanupIntervalSeconds: 120
properties:
  - name: title
    dataType: [text]
  - name: body
    dataType: [text]
`)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != "configured" {
		t.Fatalf("action = %q (info=%v)", res[0].Action, res[0].Info)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "PUT /schema/Articles") {
		t.Errorf("missing PUT: %v", f.calls)
	}
	if !strings.Contains(joined, "POST /schema/Articles/properties body") {
		t.Errorf("missing property add: %v", f.calls)
	}
}

func TestCollectionApplyUnchanged(t *testing.T) {
	f := newFake()
	yaml := "class: Articles\nproperties:\n  - name: title\n    dataType: [text]\n"
	if _, err := applyDocs(t, f, yaml); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	res, err := applyDocs(t, f, yaml)
	if err != nil {
		t.Fatal(err)
	}
	// PUT still happens (that is how convergence is checked) but the
	// reported action must be unchanged and no property adds may occur.
	if res[0].Action != "unchanged" {
		t.Fatalf("action = %q", res[0].Action)
	}
	if strings.Contains(strings.Join(f.calls, "\n"), "properties") {
		t.Errorf("unexpected property add: %v", f.calls)
	}
}

func TestCollectionApplyImmutablePropertyWarns(t *testing.T) {
	f := newFake()
	if _, err := applyDocs(t, f, "class: A\nproperties:\n  - name: t\n    dataType: [text]\n    tokenization: word\n"); err != nil {
		t.Fatal(err)
	}
	res, err := applyDocs(t, f, "class: A\nproperties:\n  - name: t\n    dataType: [text]\n    tokenization: lowercase\n")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, info := range res[0].Info {
		if strings.Contains(info, "immutable") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected immutability warning, got %+v", res[0])
	}
}

func TestUserApplyCreateShowsKeyAndAssignsRoles(t *testing.T) {
	f := newFake()
	res, err := applyDocs(t, f, "userId: bob\nroles: [dev]\n")
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != "created" {
		t.Fatalf("action = %q", res[0].Action)
	}
	if !strings.Contains(strings.Join(res[0].Info, "; "), "key-bob") {
		t.Fatalf("api key not surfaced: %+v", res[0])
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), "assign [dev]") {
		t.Errorf("roles not assigned: %v", f.calls)
	}
}

func TestUserApplyConvergesRoles(t *testing.T) {
	f := newFake()
	f.users["bob"] = []string{"old1", "old2", "keep"}
	res, err := applyDocs(t, f, "userId: bob\nroles: [keep, new]\n")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "assign [new]") {
		t.Errorf("missing assign: %v", f.calls)
	}
	if !strings.Contains(joined, "revoke [old1 old2]") && !strings.Contains(joined, "revoke") {
		t.Errorf("missing revoke: %v", f.calls)
	}
	if res[0].Action != "configured" {
		t.Fatalf("action = %q", res[0].Action)
	}
}

func TestAliasApplyCreateAndRetarget(t *testing.T) {
	f := newFake()
	if _, err := applyDocs(t, f, "alias: news\nclass: Articles\n"); err != nil {
		t.Fatal(err)
	}
	res, err := applyDocs(t, f, "alias: news\nclass: Archive\n")
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != "configured" {
		t.Fatalf("action = %q", res[0].Action)
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), "PUT /aliases/News->Archive") {
		t.Errorf("missing retarget: %v", f.calls)
	}
}

func TestAliasApplySurfacesResolveError(t *testing.T) {
	f := newFake()
	f.schemaGetStatus = http.StatusInternalServerError
	if _, err := applyDocs(t, f, "alias: news\nclass: Articles\n"); err == nil {
		t.Fatal("expected alias apply to surface the resolve error, got nil")
	}
}

func TestTenantsApplyStatusEquivalence(t *testing.T) {
	f := newFake()
	f.collections["MT"] = map[string]any{"class": "MT"}
	// Server reports ACTIVE as HOT.
	f.tenants["MT"] = []map[string]any{{"name": "t1", "activityStatus": "HOT"}}

	res, err := applyDocs(t, f, "collection: MT\ntenants:\n  - name: t1\n    activityStatus: ACTIVE\n  - name: t2\n    activityStatus: ACTIVE\n")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "\n")
	if strings.Contains(joined, "PUT tenants") {
		t.Errorf("t1 is already HOT; must not be updated: %v", f.calls)
	}
	if !strings.Contains(joined, "POST tenants MT x1") {
		t.Errorf("t2 must be created: %v", f.calls)
	}
	if res[0].Action != "configured" {
		t.Fatalf("action = %q", res[0].Action)
	}
}

// ---- backups and restore ----

const backupManifest = `kind: Backup
backend: filesystem
id: snap-1
include: [Articles]
config:
  CPUPercentage: 60
`

func TestBackupApplyLifecycle(t *testing.T) {
	f := newFake()

	// First apply starts the backup.
	res, err := applyDocs(t, f, backupManifest)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != "created" || res[0].Name != "filesystem/snap-1" {
		t.Fatalf("res = %+v", res[0])
	}
	if len(res[0].Info) == 0 || !strings.Contains(res[0].Info[0], "started (collections: Articles)") {
		t.Fatalf("info = %v", res[0].Info)
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), "POST /backups/filesystem snap-1 include=[Articles]") {
		t.Errorf("create call missing: %v", f.calls)
	}

	// Second apply sees it still running.
	res, err = applyDocs(t, f, backupManifest)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != "unchanged" || !strings.Contains(strings.Join(res[0].Info, " "), "in progress: TRANSFERRING") {
		t.Fatalf("res = %+v", res[0])
	}

	// Third apply sees it completed.
	res, err = applyDocs(t, f, backupManifest)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != "unchanged" || !strings.Contains(strings.Join(res[0].Info, " "), "already exists") {
		t.Fatalf("res = %+v", res[0])
	}
}

func TestBackupApplyRestartsFailedAttempt(t *testing.T) {
	f := newFake()
	f.backups["filesystem/snap-1"] = &backupState{
		backend: "filesystem", id: "snap-1",
		statuses: []string{"FAILED"}, errMsg: "disk full",
	}

	res, err := applyDocs(t, f, backupManifest)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != "created" {
		t.Fatalf("action = %q", res[0].Action)
	}
	if !strings.Contains(strings.Join(res[0].Info, " "), "previous attempt FAILED: disk full") {
		t.Fatalf("info = %v", res[0].Info)
	}
}

func TestBackupApplyValidation(t *testing.T) {
	f := newFake()
	if _, err := applyDocs(t, f, "kind: Backup\nbackend: filesystem\nid: snap-1\ninclude: [A]\nexclude: [B]\n"); err == nil {
		t.Error("expected include+exclude rejection")
	}
	if _, err := applyDocs(t, f, "kind: Backup\nbackend: filesystem\nid: Bad Id\n"); err == nil {
		t.Error("expected invalid id rejection")
	}
	if strings.Contains(strings.Join(f.calls, "\n"), "POST /backups") {
		t.Errorf("no request may be sent for invalid manifests: %v", f.calls)
	}
}

func TestRestoreApplyLifecycle(t *testing.T) {
	f := newFake()
	manifest := "kind: Restore\nbackend: filesystem\nid: snap-1\ninclude: [Articles]\noverwriteAlias: true\n"

	res, err := applyDocs(t, f, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != "created" || res[0].Name != "filesystem/snap-1" {
		t.Fatalf("res = %+v", res[0])
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "POST restore filesystem/snap-1") {
		t.Fatalf("restore call missing: %v", f.calls)
	}
	if !strings.Contains(joined, "overwriteAlias:true") {
		t.Errorf("restore body must pass through raw fields: %v", f.calls)
	}

	res, err = applyDocs(t, f, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != "unchanged" || !strings.Contains(strings.Join(res[0].Info, " "), "in progress: TRANSFERRING") {
		t.Fatalf("res = %+v", res[0])
	}

	res, err = applyDocs(t, f, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != "unchanged" || !strings.Contains(strings.Join(res[0].Info, " "), "already restored") {
		t.Fatalf("res = %+v", res[0])
	}
}

func TestRestoreApplyRestartsFailedAttempt(t *testing.T) {
	f := newFake()
	f.restores["filesystem/snap-1"] = &backupState{
		backend: "filesystem", id: "snap-1",
		statuses: []string{"FAILED"}, errMsg: "class exists",
	}

	res, err := applyDocs(t, f, "kind: Restore\nbackend: filesystem\nid: snap-1\n")
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != "created" {
		t.Fatalf("action = %q", res[0].Action)
	}
	if !strings.Contains(strings.Join(res[0].Info, " "), "previous attempt FAILED: class exists") {
		t.Fatalf("info = %v", res[0].Info)
	}
}
