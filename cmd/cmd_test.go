package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// mockServer provides an in-memory Weaviate REST API for testing all CLI commands.
type mockServer struct {
	mu          sync.Mutex
	collections map[string]map[string]any
	roles       map[string][]map[string]any
	users       map[string][]string // userID -> roles
	aliases     map[string]string   // alias -> class
	tenants     map[string][]map[string]any
	shards      map[string][]map[string]any
}

func newMockServer() *mockServer {
	return &mockServer{
		collections: map[string]map[string]any{
			"Articles": {
				"class":      "Articles",
				"vectorizer": "none",
				"replicationConfig": map[string]any{
					"factor": 1,
				},
				"shardingConfig": map[string]any{
					"desiredCount": 1,
				},
				"multiTenancyConfig": map[string]any{
					"enabled": false,
				},
				"properties": []any{
					map[string]any{"name": "title", "dataType": []string{"text"}},
				},
			},
			"MTNews": {
				"class":      "MTNews",
				"vectorizer": "none",
				"replicationConfig": map[string]any{
					"factor": 1,
				},
				"shardingConfig": map[string]any{
					"desiredCount": 1,
				},
				"multiTenancyConfig": map[string]any{
					"enabled": true,
				},
			},
		},
		roles: map[string][]map[string]any{
			"admin": {
				{"action": "read_roles", "roles": map[string]any{"role": "*"}},
			},
		},
		users: map[string][]string{
			"alice": {"admin"},
		},
		aliases: map[string]string{
			"News": "Articles",
		},
		tenants: map[string][]map[string]any{
			"MTNews": {
				{"name": "tenant-1", "activityStatus": "HOT"},
			},
		},
		shards: map[string][]map[string]any{
			"Articles": {
				{"name": "shard-1", "status": "READY", "vectorQueueSize": 0},
			},
		},
	}
}

func (m *mockServer) handler() http.Handler {
	mux := http.NewServeMux()

	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}

	// Meta
	mux.HandleFunc("GET /v1/meta", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"hostname": "http://localhost:8080",
			"version":  "1.40.1",
		})
	})

	// Nodes
	mux.HandleFunc("GET /v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"nodes": []map[string]any{
				{
					"name":            "node-1",
					"status":          "HEALTHY",
					"version":         "1.40.1",
					"operationalMode": "ACTIVE",
					"shards":          []any{map[string]any{}},
				},
			},
		})
	})

	// Collections (Schema)
	mux.HandleFunc("GET /v1/schema", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		classes := make([]map[string]any, 0, len(m.collections))
		for _, c := range m.collections {
			classes = append(classes, c)
		}
		writeJSON(w, http.StatusOK, map[string]any{"classes": classes})
	})

	mux.HandleFunc("GET /v1/schema/{name}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		name := r.PathValue("name")
		if col, ok := m.collections[name]; ok {
			writeJSON(w, http.StatusOK, col)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	mux.HandleFunc("POST /v1/schema", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		var col map[string]any
		_ = json.NewDecoder(r.Body).Decode(&col)
		name, _ := col["class"].(string)
		if _, exists := m.collections[name]; exists {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": []map[string]string{{"message": fmt.Sprintf("class name %s already exists", name)}},
			})
			return
		}
		m.collections[name] = col
		writeJSON(w, http.StatusOK, col)
	})

	mux.HandleFunc("PUT /v1/schema/{name}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		var col map[string]any
		_ = json.NewDecoder(r.Body).Decode(&col)
		name := r.PathValue("name")
		m.collections[name] = col
		writeJSON(w, http.StatusOK, col)
	})

	mux.HandleFunc("DELETE /v1/schema/{name}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		name := r.PathValue("name")
		if _, ok := m.collections[name]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(m.collections, name)
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /v1/schema/{name}/properties", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		name := r.PathValue("name")
		var prop map[string]any
		_ = json.NewDecoder(r.Body).Decode(&prop)
		col, ok := m.collections[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		props, _ := col["properties"].([]any)
		col["properties"] = append(props, prop)
		writeJSON(w, http.StatusOK, prop)
	})

	// Shards
	mux.HandleFunc("GET /v1/schema/{name}/shards", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		name := r.PathValue("name")
		if _, ok := m.collections[name]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, m.shards[name])
	})

	// Tenants
	mux.HandleFunc("GET /v1/schema/{name}/tenants", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		name := r.PathValue("name")
		if _, ok := m.collections[name]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, m.tenants[name])
	})

	mux.HandleFunc("POST /v1/schema/{name}/tenants", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		name := r.PathValue("name")
		if _, ok := m.collections[name]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var ts []map[string]any
		_ = json.NewDecoder(r.Body).Decode(&ts)
		m.tenants[name] = append(m.tenants[name], ts...)
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("PUT /v1/schema/{name}/tenants", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("DELETE /v1/schema/{name}/tenants", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	// Roles
	mux.HandleFunc("GET /v1/authz/roles", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		var out []map[string]any
		for name, perms := range m.roles {
			out = append(out, map[string]any{"name": name, "permissions": perms})
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("GET /v1/authz/roles/{name}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		name := r.PathValue("name")
		perms, ok := m.roles[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": name, "permissions": perms})
	})

	mux.HandleFunc("POST /v1/authz/roles", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		var body struct {
			Name        string           `json:"name"`
			Permissions []map[string]any `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.roles[body.Name] = body.Permissions
		w.WriteHeader(http.StatusCreated)
	})

	mux.HandleFunc("DELETE /v1/authz/roles/{name}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		name := r.PathValue("name")
		if _, ok := m.roles[name]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(m.roles, name)
		w.WriteHeader(http.StatusOK)
	})

	// Users
	mux.HandleFunc("GET /v1/users/db", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		var out []map[string]any
		for id, roles := range m.users {
			out = append(out, map[string]any{"userId": id, "roles": roles, "active": true, "dbUserType": "custom"})
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("GET /v1/users/db/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		id := r.PathValue("id")
		roles, ok := m.users[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"userId": id, "roles": roles, "active": true, "dbUserType": "custom"})
	})

	mux.HandleFunc("POST /v1/users/db/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		id := r.PathValue("id")
		m.users[id] = []string{}
		writeJSON(w, http.StatusCreated, map[string]any{"apikey": "generated-key-" + id})
	})

	mux.HandleFunc("DELETE /v1/users/db/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		id := r.PathValue("id")
		if _, ok := m.users[id]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(m.users, id)
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /v1/users/db/{id}/activate", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/users/db/{id}/deactivate", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/users/db/{id}/rotate-key", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		id := r.PathValue("id")
		if _, ok := m.users[id]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"apikey": "rotated-key-" + id})
	})

	mux.HandleFunc("POST /v1/authz/users/{id}/assign", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/authz/users/{id}/revoke", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Aliases
	mux.HandleFunc("GET /v1/aliases", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		var out []map[string]any
		for a, c := range m.aliases {
			out = append(out, map[string]any{"alias": a, "class": c})
		}
		writeJSON(w, http.StatusOK, map[string]any{"aliases": out})
	})

	mux.HandleFunc("POST /v1/aliases", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		var a struct{ Alias, Class string }
		_ = json.NewDecoder(r.Body).Decode(&a)
		m.aliases[a.Alias] = a.Class
		writeJSON(w, http.StatusOK, a)
	})

	mux.HandleFunc("PUT /v1/aliases/{name}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		name := r.PathValue("name")
		var body struct{ Class string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.aliases[name] = body.Class
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("DELETE /v1/aliases/{name}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		name := r.PathValue("name")
		if _, ok := m.aliases[name]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(m.aliases, name)
		w.WriteHeader(http.StatusOK)
	})

	return mux
}

func executeCommand(args []string) (string, string, error) {
	// Reset package global flags before executing
	flagServer = ""
	flagAPIKey = ""
	flagContext = ""
	flagConfig = ""
	flagNoConfig = false

	cmd := NewRootCommand()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)

	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func TestCLI_Version(t *testing.T) {
	mock := newMockServer()
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	out, _, err := executeCommand([]string{"version", "--server", srv.URL})
	if err != nil {
		t.Fatalf("version failed: %v", err)
	}
	if !strings.Contains(out, "weavectl") || !strings.Contains(out, "Weaviate 1.40.1") {
		t.Fatalf("unexpected version output: %q", out)
	}

	// Extra argument rejected
	_, _, err = executeCommand([]string{"version", "extra", "--server", srv.URL})
	if err == nil {
		t.Fatal("expected error for extra arguments to version")
	}

	// Unreachable server
	_, _, err = executeCommand([]string{"version", "--server", "http://127.0.0.1:1"})
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestCLI_Get(t *testing.T) {
	mock := newMockServer()
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	cases := []struct {
		name       string
		args       []string
		wantIn     string
		wantErr    bool
		wantErrMsg string
	}{
		// Collections
		{"get collections table", []string{"get", "collections"}, "Articles", false, ""},
		{"get collection singular", []string{"get", "collection"}, "Articles", false, ""},
		{"get collection single", []string{"get", "collection", "Articles"}, "Articles", false, ""},
		{"get collection case-insensitive", []string{"get", "collection", "articles"}, "Articles", false, ""},
		{"get collection -o json", []string{"get", "collection", "Articles", "-o", "json"}, `"class": "Articles"`, false, ""},
		{"get collection -o yaml", []string{"get", "collection", "Articles", "-o", "yaml"}, "class: Articles", false, ""},
		{"get collection -o wide", []string{"get", "collection", "Articles", "-o", "wide"}, "Articles", false, ""},
		{"get collection not found", []string{"get", "collection", "nonexistent"}, "", true, "not found"},

		// Roles
		{"get roles", []string{"get", "roles"}, "admin", false, ""},
		{"get role single", []string{"get", "role", "admin"}, "admin", false, ""},
		{"get role not found", []string{"get", "role", "nonexistent"}, "", true, "not found"},

		// Users
		{"get users", []string{"get", "users"}, "alice", false, ""},
		{"get user single", []string{"get", "user", "alice"}, "alice", false, ""},
		{"get user not found", []string{"get", "user", "nonexistent"}, "", true, "not found"},

		// Aliases
		{"get aliases", []string{"get", "aliases"}, "News", false, ""},
		{"get alias single", []string{"get", "alias", "News"}, "News", false, ""},
		{"get alias case-insensitive", []string{"get", "alias", "news"}, "News", false, ""},
		{"get alias not found", []string{"get", "alias", "nonexistent"}, "", true, "not found"},

		// Tenants
		{"get tenants valid", []string{"get", "tenants", "MTNews"}, "tenant-1", false, ""},
		{"get tenants missing col", []string{"get", "tenants"}, "", true, "get tenants requires a collection name"},
		{"get tenants not found col", []string{"get", "tenants", "nonexistent"}, "", true, "not found"},

		// Shards
		{"get shards valid", []string{"get", "shards", "Articles"}, "shard-1", false, ""},
		{"get shards missing col", []string{"get", "shards"}, "", true, "get shards requires a collection name"},
		{"get shards not found col", []string{"get", "shards", "nonexistent"}, "", true, "not found"},

		// Nodes
		{"get nodes valid", []string{"get", "nodes"}, "node-1", false, ""},
		{"get nodes with name error", []string{"get", "nodes", "extra"}, "", true, "get nodes takes no name argument"},

		// Formats and edge cases
		{"invalid format", []string{"get", "collections", "-o", "xml"}, "", true, "unknown output format"},
		{"unknown resource type", []string{"get", "unknown_type"}, "", true, "unknown resource type"},
		{"get without args", []string{"get"}, "", true, "accepts between 1 and 2 arg(s)"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{}, tc.args...)
			args = append(args, "--server", srv.URL)
			out, _, err := executeCommand(args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got success (out=%q)", tc.wantErrMsg, out)
				}
				if tc.wantErrMsg != "" && !strings.Contains(err.Error(), tc.wantErrMsg) {
					t.Fatalf("expected error containing %q, got %q", tc.wantErrMsg, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantIn != "" && !strings.Contains(out, tc.wantIn) {
				t.Fatalf("expected %q in output, got %q", tc.wantIn, out)
			}
		})
	}
}

func TestCLI_Create(t *testing.T) {
	mock := newMockServer()
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	dir := t.TempDir()

	// 1. Create collection with flags
	out, _, err := executeCommand([]string{
		"create", "collection", "Books",
		"--vectorizer", "none",
		"--replication-factor", "2",
		"--shard-count", "2",
		"--multi-tenancy",
		"--server", srv.URL,
	})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	if !strings.Contains(out, "collection/Books created") {
		t.Fatalf("unexpected out: %q", out)
	}

	// Duplicate create collection fails
	_, _, err = executeCommand([]string{"create", "collection", "Books", "--server", srv.URL})
	if err == nil {
		t.Fatal("expected duplicate create collection to fail")
	}

	// 2. Create user
	out, _, err = executeCommand([]string{"create", "user", "bob", "--server", srv.URL})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if !strings.Contains(out, "user/bob created") || !strings.Contains(out, "API key (shown once") {
		t.Fatalf("unexpected out: %q", out)
	}

	// 3. Create alias
	out, _, err = executeCommand([]string{"create", "alias", "BookAlias", "--collection", "Books", "--server", srv.URL})
	if err != nil {
		t.Fatalf("create alias: %v", err)
	}
	if !strings.Contains(out, "alias/BookAlias created") {
		t.Fatalf("unexpected out: %q", out)
	}

	// 4. Create alias missing collection
	_, _, err = executeCommand([]string{"create", "alias", "BookAlias2", "--server", srv.URL})
	if err == nil {
		t.Fatal("expected error when --collection is missing")
	}

	// 5. Create tenant
	out, _, err = executeCommand([]string{"create", "tenant", "MTNews", "tenant-2", "tenant-3", "--activity-status", "ACTIVE", "--server", srv.URL})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if !strings.Contains(out, "tenant/MTNews created") {
		t.Fatalf("unexpected out: %q", out)
	}

	// 6. Create tenant on non-existent collection
	_, _, err = executeCommand([]string{"create", "tenant", "NonExistent", "t1", "--server", srv.URL})
	if err == nil {
		t.Fatal("expected error creating tenant on non-existent collection")
	}

	// 7. Create from file -f (collection, role, user, alias, tenants)
	manifestPath := filepath.Join(dir, "manifest.yaml")
	_ = os.WriteFile(manifestPath, []byte("class: FromFile\nvectorizer: none\n"), 0o644)
	out, _, err = executeCommand([]string{"create", "-f", manifestPath, "--server", srv.URL})
	if err != nil {
		t.Fatalf("create -f: %v", err)
	}
	if !strings.Contains(out, "collection/FromFile created") {
		t.Fatalf("unexpected out: %q", out)
	}

	// Duplicate create -f fails
	_, _, err = executeCommand([]string{"create", "-f", manifestPath, "--server", srv.URL})
	if err == nil {
		t.Fatal("expected duplicate create -f to fail")
	}

	// Role via create -f
	rolePath := filepath.Join(dir, "role.yaml")
	_ = os.WriteFile(rolePath, []byte("name: file-role\npermissions: []\n"), 0o644)
	out, _, err = executeCommand([]string{"create", "-f", rolePath, "--server", srv.URL})
	if err != nil {
		t.Fatalf("create -f role: %v", err)
	}
	if !strings.Contains(out, "role/file-role created") {
		t.Fatalf("unexpected out: %q", out)
	}
	// Duplicate role create -f fails
	_, _, err = executeCommand([]string{"create", "-f", rolePath, "--server", srv.URL})
	if err == nil {
		t.Fatal("expected duplicate role create -f to fail")
	}

	// User via create -f
	userPath := filepath.Join(dir, "user.yaml")
	_ = os.WriteFile(userPath, []byte("userId: file-user\n"), 0o644)
	out, _, err = executeCommand([]string{"create", "-f", userPath, "--server", srv.URL})
	if err != nil {
		t.Fatalf("create -f user: %v", err)
	}
	if !strings.Contains(out, "user/file-user created") {
		t.Fatalf("unexpected out: %q", out)
	}
	// Duplicate user create -f fails
	_, _, err = executeCommand([]string{"create", "-f", userPath, "--server", srv.URL})
	if err == nil {
		t.Fatal("expected duplicate user create -f to fail")
	}

	// Alias via create -f
	aliasPath := filepath.Join(dir, "alias.yaml")
	_ = os.WriteFile(aliasPath, []byte("alias: file-alias\nclass: Articles\n"), 0o644)
	out, _, err = executeCommand([]string{"create", "-f", aliasPath, "--server", srv.URL})
	if err != nil {
		t.Fatalf("create -f alias: %v", err)
	}
	if !strings.Contains(out, "alias/File-alias created") {
		t.Fatalf("unexpected out: %q", out)
	}
	// Duplicate alias create -f fails
	_, _, err = executeCommand([]string{"create", "-f", aliasPath, "--server", srv.URL})
	if err == nil {
		t.Fatal("expected duplicate alias create -f to fail")
	}

	// Tenants via create -f
	tenantsPath := filepath.Join(dir, "tenants.yaml")
	_ = os.WriteFile(tenantsPath, []byte("collection: MTNews\ntenants:\n  - name: t-extra\n"), 0o644)
	out, _, err = executeCommand([]string{"create", "-f", tenantsPath, "--server", srv.URL})
	if err != nil {
		t.Fatalf("create -f tenants: %v", err)
	}
	if !strings.Contains(out, "tenants/MTNews") {
		t.Fatalf("unexpected out: %q", out)
	}
	// Duplicate tenant create -f fails
	_, _, err = executeCommand([]string{"create", "-f", tenantsPath, "--server", srv.URL})
	if err == nil {
		t.Fatal("expected duplicate tenants create -f to fail")
	}

	// Create with no subcommand and no -f
	_, _, err = executeCommand([]string{"create", "--server", srv.URL})
	if err == nil {
		t.Fatal("expected error when create has no args and no -f")
	}
}

func TestCLI_Apply(t *testing.T) {
	mock := newMockServer()
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	dir := t.TempDir()

	// 1. Apply new collection
	colFile := filepath.Join(dir, "articles_apply.yaml")
	_ = os.WriteFile(colFile, []byte("class: Articles\nproperties:\n  - name: title\n    dataType: [text]\n  - name: content\n    dataType: [text]\n"), 0o644)
	out, _, err := executeCommand([]string{"apply", "-f", colFile, "--server", srv.URL})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(out, "Articles") {
		t.Fatalf("unexpected out: %q", out)
	}

	// 2. Apply is idempotent
	out, _, err = executeCommand([]string{"apply", "-f", colFile, "--server", srv.URL})
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if !strings.Contains(out, "unchanged") {
		t.Fatalf("expected unchanged on re-apply, got: %q", out)
	}

	// 3. Multi-doc apply
	multiFile := filepath.Join(dir, "multi.yaml")
	_ = os.WriteFile(multiFile, []byte(`class: MultiCol
---
name: reader
permissions:
  - action: read_data
    data: {collection: MultiCol}
`), 0o644)
	out, _, err = executeCommand([]string{"apply", "-f", multiFile, "--server", srv.URL})
	if err != nil {
		t.Fatalf("multi apply: %v", err)
	}
	if !strings.Contains(out, "MultiCol") || !strings.Contains(out, "reader") {
		t.Fatalf("unexpected multi out: %q", out)
	}

	// 4. Missing -f flag
	_, _, err = executeCommand([]string{"apply", "--server", srv.URL})
	if err == nil {
		t.Fatal("expected error when -f flag is missing")
	}

	// 5. Non-existent file
	_, _, err = executeCommand([]string{"apply", "-f", "nonexistent.yaml", "--server", srv.URL})
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}

func TestCLI_ApplyUserActiveState(t *testing.T) {
	mock := newMockServer()
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	dir := t.TempDir()
	userFile := filepath.Join(dir, "user-active.yaml")
	_ = os.WriteFile(userFile, []byte("userId: bob\nroles: [dev]\nactive: false\n"), 0o644)

	// New user: created, key shown, deactivated in the same apply.
	out, _, err := executeCommand([]string{"apply", "-f", userFile, "--server", srv.URL})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(out, "user/bob created") || !strings.Contains(out, "deactivated user") {
		t.Fatalf("unexpected out: %q", out)
	}

	// Existing user (mock reports active): deactivated via apply.
	out, _, err = executeCommand([]string{"apply", "-f", userFile, "--server", srv.URL})
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if !strings.Contains(out, "user/bob configured") || !strings.Contains(out, "deactivated user") {
		t.Fatalf("unexpected out: %q", out)
	}
}

func TestCLI_RotateUser(t *testing.T) {
	mock := newMockServer()
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	out, _, err := executeCommand([]string{"rotate", "user", "alice", "--server", srv.URL})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if !strings.Contains(out, "user/alice rotated") ||
		!strings.Contains(out, "API key (shown once, store it now): rotated-key-alice") {
		t.Fatalf("unexpected out: %q", out)
	}

	// Unknown user: server 404, non-zero exit.
	_, _, err = executeCommand([]string{"rotate", "user", "ghost", "--server", srv.URL})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("rotate ghost: expected 404 error, got %v", err)
	}
}

func TestCLI_Delete(t *testing.T) {
	mock := newMockServer()
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	dir := t.TempDir()

	// 1. Delete collection (case-insensitive resolution prints canonical name)
	out, _, err := executeCommand([]string{"delete", "collection", "articles", "--server", srv.URL})
	if err != nil {
		t.Fatalf("delete collection: %v", err)
	}
	if !strings.Contains(out, `collection "Articles" deleted`) {
		t.Fatalf("unexpected out: %q", out)
	}

	// Delete non-existent collection
	_, _, err = executeCommand([]string{"delete", "collection", "articles", "--server", srv.URL})
	if err == nil {
		t.Fatal("expected 404 deleting non-existent collection")
	}

	// 2. Delete role
	out, _, err = executeCommand([]string{"delete", "role", "admin", "--server", srv.URL})
	if err != nil {
		t.Fatalf("delete role: %v", err)
	}
	if !strings.Contains(out, `role "admin" deleted`) {
		t.Fatalf("unexpected out: %q", out)
	}

	// 3. Delete user
	out, _, err = executeCommand([]string{"delete", "user", "alice", "--server", srv.URL})
	if err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if !strings.Contains(out, `user "alice" deleted`) {
		t.Fatalf("unexpected out: %q", out)
	}

	// 4. Delete alias
	out, _, err = executeCommand([]string{"delete", "alias", "news", "--server", srv.URL})
	if err != nil {
		t.Fatalf("delete alias: %v", err)
	}
	if !strings.Contains(out, `alias "News" deleted`) {
		t.Fatalf("unexpected out: %q", out)
	}

	// 5. Delete tenant
	out, _, err = executeCommand([]string{"delete", "tenant", "mtnews", "tenant-1", "--server", srv.URL})
	if err != nil {
		t.Fatalf("delete tenant: %v", err)
	}
	if !strings.Contains(out, `deleted: tenant-1`) {
		t.Fatalf("unexpected out: %q", out)
	}

	// 6. Delete -f (collection, role, user, alias, tenants)
	delFile := filepath.Join(dir, "del.yaml")
	_ = os.WriteFile(delFile, []byte("class: MTNews\n"), 0o644)
	out, _, err = executeCommand([]string{"delete", "-f", delFile, "--server", srv.URL})
	if err != nil {
		t.Fatalf("delete -f collection: %v", err)
	}
	if !strings.Contains(out, `collection "MTNews" deleted`) {
		t.Fatalf("unexpected out: %q", out)
	}

	// Delete role -f
	delRole := filepath.Join(dir, "del_role.yaml")
	mock.roles["temp-role"] = []map[string]any{}
	_ = os.WriteFile(delRole, []byte("name: temp-role\npermissions: []\n"), 0o644)
	out, _, err = executeCommand([]string{"delete", "-f", delRole, "--server", srv.URL})
	if err != nil {
		t.Fatalf("delete -f role: %v", err)
	}
	if !strings.Contains(out, `role "temp-role" deleted`) {
		t.Fatalf("unexpected out: %q", out)
	}

	// Delete user -f
	delUser := filepath.Join(dir, "del_user.yaml")
	mock.users["temp-user"] = []string{}
	_ = os.WriteFile(delUser, []byte("userId: temp-user\n"), 0o644)
	out, _, err = executeCommand([]string{"delete", "-f", delUser, "--server", srv.URL})
	if err != nil {
		t.Fatalf("delete -f user: %v", err)
	}
	if !strings.Contains(out, `user "temp-user" deleted`) {
		t.Fatalf("unexpected out: %q", out)
	}

	// Delete alias -f
	delAlias := filepath.Join(dir, "del_alias.yaml")
	mock.aliases["TempAlias"] = "Articles"
	_ = os.WriteFile(delAlias, []byte("alias: TempAlias\nclass: Articles\n"), 0o644)
	out, _, err = executeCommand([]string{"delete", "-f", delAlias, "--server", srv.URL})
	if err != nil {
		t.Fatalf("delete -f alias: %v", err)
	}
	if !strings.Contains(out, `alias "TempAlias" deleted`) {
		t.Fatalf("unexpected out: %q", out)
	}

	// Delete tenants -f
	delTenants := filepath.Join(dir, "del_tenants.yaml")
	mock.collections["ColWithTenants"] = map[string]any{"class": "ColWithTenants"}
	_ = os.WriteFile(delTenants, []byte("collection: ColWithTenants\ntenants:\n  - name: t-del\n"), 0o644)
	out, _, err = executeCommand([]string{"delete", "-f", delTenants, "--server", srv.URL})
	if err != nil {
		t.Fatalf("delete -f tenants: %v", err)
	}
	if !strings.Contains(out, `tenant "ColWithTenants" deleted`) {
		t.Fatalf("unexpected out: %q", out)
	}

	// 7. Delete with no args and no -f
	_, _, err = executeCommand([]string{"delete", "--server", srv.URL})
	if err == nil {
		t.Fatal("expected error when delete has no args and no -f")
	}
}

func TestCLI_ConfigFlags(t *testing.T) {
	mock := newMockServer()
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(configPath, []byte(fmt.Sprintf(`
current-context: dev
contexts:
  dev:
    server: %s
    api-key: my-key
  prod:
    server: http://prod.example.com
`, srv.URL)), 0o644)

	// Context from config file
	out, _, err := executeCommand([]string{"version", "--config", configPath})
	if err != nil {
		t.Fatalf("execute with config: %v", err)
	}
	if !strings.Contains(out, "Weaviate 1.40.1") {
		t.Fatalf("expected version output, got: %q", out)
	}

	// Switch context via flag
	_, _, err = executeCommand([]string{"version", "--config", configPath, "--context", "prod"})
	if err == nil {
		t.Fatal("expected connection error to non-existent prod server")
	}

	// --no-config flag
	_, _, err = executeCommand([]string{"version", "--config", configPath, "--no-config", "--server", srv.URL})
	if err != nil {
		t.Fatalf("--no-config with --server: %v", err)
	}
}
