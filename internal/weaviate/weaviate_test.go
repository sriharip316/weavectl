package weaviate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sriharip316/weavectl/internal/client"
)

func newTestClient(handler http.Handler) (*client.Client, func()) {
	srv := httptest.NewServer(handler)
	return client.New(srv.URL, "test-key"), srv.Close
}

func TestCapitalizeName(t *testing.T) {
	for input, want := range map[string]string{
		"":        "",
		"a":       "A",
		"A":       "A",
		"article": "Article",
		"Article": "Article",
		"m_news":  "M_news",
		"123":     "123",
	} {
		if got := CapitalizeName(input); got != want {
			t.Errorf("CapitalizeName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCollectionsEndpoints(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/schema", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"classes": []map[string]any{
				{"class": "Articles"},
				{"class": "MTNews"},
			},
		})
	})
	mux.HandleFunc("GET /v1/schema/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "Articles" || name == "articles" {
			_ = json.NewEncoder(w).Encode(map[string]any{"class": "Articles"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("POST /v1/schema", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("PUT /v1/schema/{name}", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("DELETE /v1/schema/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/schema/{name}/properties", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	c, cleanup := newTestClient(mux)
	defer cleanup()

	// ListCollections
	list, err := ListCollections(c)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListCollections: %v (len=%d)", err, len(list))
	}

	// GetCollection
	col, err := GetCollection(c, "Articles")
	if err != nil || col["class"] != "Articles" {
		t.Fatalf("GetCollection: %v (%v)", err, col)
	}

	// GetCollection empty string must return NotFound
	if _, err := GetCollection(c, ""); !client.IsNotFound(err) {
		t.Fatalf("GetCollection('') expected NotFound, got %v", err)
	}

	// CreateCollection
	created, err := CreateCollection(c, map[string]any{"class": "TestCol"})
	if err != nil || created["class"] != "TestCol" {
		t.Fatalf("CreateCollection: %v (%v)", err, created)
	}

	// UpdateCollection
	updated, err := UpdateCollection(c, "Articles", map[string]any{"class": "Articles"})
	if err != nil || updated["class"] != "Articles" {
		t.Fatalf("UpdateCollection: %v (%v)", err, updated)
	}

	// UpdateCollection empty
	if _, err := UpdateCollection(c, "", map[string]any{}); !client.IsNotFound(err) {
		t.Fatalf("UpdateCollection('') expected NotFound, got %v", err)
	}

	// DeleteCollection
	if err := DeleteCollection(c, "Articles"); err != nil {
		t.Fatalf("DeleteCollection: %v", err)
	}

	// DeleteCollection empty
	if err := DeleteCollection(c, ""); !client.IsNotFound(err) {
		t.Fatalf("DeleteCollection('') expected NotFound, got %v", err)
	}

	// AddProperty
	if err := AddProperty(c, "Articles", map[string]any{"name": "author", "dataType": []string{"text"}}); err != nil {
		t.Fatalf("AddProperty: %v", err)
	}
}

func TestResolveCollection(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/schema", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"classes": []map[string]any{
				{"class": "Articles"},
				{"class": "MTNews"},
				{"class": "Dupe1"},
				{"class": "DUPE1"},
			},
		})
	})
	mux.HandleFunc("GET /v1/schema/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "Articles" || name == "articles" || name == "MTNews" {
			_ = json.NewEncoder(w).Encode(map[string]any{"class": name})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	c, cleanup := newTestClient(mux)
	defer cleanup()

	// Empty string must return NotFound
	if _, err := ResolveCollection(c, ""); !client.IsNotFound(err) {
		t.Fatalf("ResolveCollection('') expected NotFound, got %v", err)
	}

	// Exact match
	name, err := ResolveCollection(c, "Articles")
	if err != nil || name != "Articles" {
		t.Fatalf("ResolveCollection('Articles') = %q, %v", name, err)
	}

	// CapitalizeName match (articles -> Articles)
	name, err = ResolveCollection(c, "articles")
	if err != nil || name != "Articles" {
		t.Fatalf("ResolveCollection('articles') = %q, %v", name, err)
	}

	// Case-insensitive match (mtnews -> MTNews)
	name, err = ResolveCollection(c, "mtnews")
	if err != nil || name != "MTNews" {
		t.Fatalf("ResolveCollection('mtnews') = %q, %v", name, err)
	}

	// Not found
	if _, err := ResolveCollection(c, "absent"); !client.IsNotFound(err) {
		t.Fatalf("ResolveCollection('absent') expected NotFound, got %v", err)
	}

	// Ambiguous match
	if _, err := ResolveCollection(c, "dupe1"); err == nil {
		t.Fatalf("ResolveCollection('dupe1') expected ambiguity error, got nil")
	}
}

func TestResolveAlias(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/aliases", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"aliases": []map[string]any{
				{"alias": "News", "class": "Articles"},
				{"alias": "MT_Alias", "class": "MTNews"},
				{"alias": "Amb1", "class": "C1"},
				{"alias": "amb1", "class": "C2"},
			},
		})
	})

	c, cleanup := newTestClient(mux)
	defer cleanup()

	// Empty string
	if _, err := ResolveAlias(c, ""); !client.IsNotFound(err) {
		t.Fatalf("ResolveAlias('') expected NotFound, got %v", err)
	}

	// Exact / CapitalizeName
	alias, err := ResolveAlias(c, "news")
	if err != nil || alias != "News" {
		t.Fatalf("ResolveAlias('news') = %q, %v", alias, err)
	}

	// Case-insensitive
	alias, err = ResolveAlias(c, "mt_alias")
	if err != nil || alias != "MT_Alias" {
		t.Fatalf("ResolveAlias('mt_alias') = %q, %v", alias, err)
	}

	// Not found
	if _, err := ResolveAlias(c, "nonexistent"); !client.IsNotFound(err) {
		t.Fatalf("ResolveAlias('nonexistent') expected NotFound, got %v", err)
	}

	// Ambiguous
	if _, err := ResolveAlias(c, "AMB1"); err == nil {
		t.Fatalf("ResolveAlias('AMB1') expected error, got nil")
	}
}

func TestShardsAndTenants(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/schema/{name}/shards", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"name": "shard-1", "status": "READY", "vectorQueueSize": 0},
		})
	})
	mux.HandleFunc("GET /v1/schema/{name}/tenants", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"name": "t1", "activityStatus": "HOT"},
		})
	})
	mux.HandleFunc("POST /v1/schema/{name}/tenants", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("PUT /v1/schema/{name}/tenants", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("DELETE /v1/schema/{name}/tenants", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	c, cleanup := newTestClient(mux)
	defer cleanup()

	// Shards
	shards, err := ListShards(c, "Articles")
	if err != nil || len(shards) != 1 || shards[0].Name != "shard-1" {
		t.Fatalf("ListShards: %v (%v)", err, shards)
	}

	// Tenants
	tenants, err := ListTenants(c, "Articles")
	if err != nil || len(tenants) != 1 || tenants[0].Name != "t1" {
		t.Fatalf("ListTenants: %v (%v)", err, tenants)
	}

	if err := CreateTenants(c, "Articles", []Tenant{{Name: "t2", ActivityStatus: "ACTIVE"}}); err != nil {
		t.Fatalf("CreateTenants: %v", err)
	}
	if err := UpdateTenants(c, "Articles", []Tenant{{Name: "t1", ActivityStatus: "COLD"}}); err != nil {
		t.Fatalf("UpdateTenants: %v", err)
	}
	if err := DeleteTenants(c, "Articles", []string{"t1"}); err != nil {
		t.Fatalf("DeleteTenants: %v", err)
	}
}

func TestAliasesEndpoints(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/aliases", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"aliases": []map[string]any{
				{"alias": "News", "class": "Articles"},
			},
		})
	})
	mux.HandleFunc("POST /v1/aliases", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("PUT /v1/aliases/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("DELETE /v1/aliases/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	c, cleanup := newTestClient(mux)
	defer cleanup()

	aliases, err := ListAliases(c)
	if err != nil || len(aliases) != 1 {
		t.Fatalf("ListAliases: %v", err)
	}
	if err := CreateAlias(c, Alias{Alias: "A2", Class: "Articles"}); err != nil {
		t.Fatalf("CreateAlias: %v", err)
	}
	if err := UpdateAlias(c, "News", "MTNews"); err != nil {
		t.Fatalf("UpdateAlias: %v", err)
	}
	if err := DeleteAlias(c, "News"); err != nil {
		t.Fatalf("DeleteAlias: %v", err)
	}
	if err := DeleteAlias(c, ""); !client.IsNotFound(err) {
		t.Fatalf("DeleteAlias('') expected NotFound, got %v", err)
	}
}

func TestRolesEndpoints(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/authz/roles", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"name": "admin", "permissions": []map[string]any{}},
		})
	})
	mux.HandleFunc("GET /v1/authz/roles/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "admin" {
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "admin", "permissions": []map[string]any{}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("POST /v1/authz/roles", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("DELETE /v1/authz/roles/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/authz/roles/{name}/add-permissions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/authz/roles/{name}/remove-permissions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	c, cleanup := newTestClient(mux)
	defer cleanup()

	roles, err := ListRoles(c)
	if err != nil || len(roles) != 1 {
		t.Fatalf("ListRoles: %v", err)
	}
	role, err := GetRole(c, "admin")
	if err != nil || role.Name != "admin" {
		t.Fatalf("GetRole: %v (%v)", err, role)
	}
	if _, err := GetRole(c, ""); !client.IsNotFound(err) {
		t.Fatalf("GetRole('') expected NotFound, got %v", err)
	}
	if err := CreateRole(c, Role{Name: "editor"}); err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if err := DeleteRole(c, "editor"); err != nil {
		t.Fatalf("DeleteRole: %v", err)
	}
	if err := DeleteRole(c, ""); !client.IsNotFound(err) {
		t.Fatalf("DeleteRole('') expected NotFound, got %v", err)
	}
	if err := AddPermissions(c, "admin", []map[string]any{{"action": "read_data"}}); err != nil {
		t.Fatalf("AddPermissions: %v", err)
	}
	if err := RemovePermissions(c, "admin", []map[string]any{{"action": "read_data"}}); err != nil {
		t.Fatalf("RemovePermissions: %v", err)
	}
}

func TestUsersEndpoints(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/users/db", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"userId": "alice", "roles": []string{"admin"}, "active": true},
		})
	})
	mux.HandleFunc("GET /v1/users/db/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "alice" {
			_ = json.NewEncoder(w).Encode(map[string]any{"userId": "alice", "roles": []string{"admin"}, "active": true})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("POST /v1/users/db/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"apikey": "secret-key-123"})
	})
	mux.HandleFunc("DELETE /v1/users/db/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/users/db/{id}/activate", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/users/db/{id}/deactivate", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/authz/users/{id}/assign", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/authz/users/{id}/revoke", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	c, cleanup := newTestClient(mux)
	defer cleanup()

	users, err := ListUsers(c)
	if err != nil || len(users) != 1 {
		t.Fatalf("ListUsers: %v", err)
	}
	user, err := GetUser(c, "alice")
	if err != nil || user.UserID != "alice" {
		t.Fatalf("GetUser: %v (%v)", err, user)
	}
	if _, err := GetUser(c, ""); !client.IsNotFound(err) {
		t.Fatalf("GetUser('') expected NotFound, got %v", err)
	}
	apiKey, err := CreateUser(c, "bob")
	if err != nil || apiKey != "secret-key-123" {
		t.Fatalf("CreateUser: %v (key=%q)", err, apiKey)
	}
	if err := DeleteUser(c, "bob"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if err := DeleteUser(c, ""); !client.IsNotFound(err) {
		t.Fatalf("DeleteUser('') expected NotFound, got %v", err)
	}
	if err := AssignRoles(c, "alice", []string{"editor"}); err != nil {
		t.Fatalf("AssignRoles: %v", err)
	}
	if err := RevokeRoles(c, "alice", []string{"editor"}); err != nil {
		t.Fatalf("RevokeRoles: %v", err)
	}
	if err := ActivateUser(c, "bob"); err != nil {
		t.Fatalf("ActivateUser: %v", err)
	}
	if err := DeactivateUser(c, "bob"); err != nil {
		t.Fatalf("DeactivateUser: %v", err)
	}
	if err := ActivateUser(c, ""); !client.IsNotFound(err) {
		t.Fatalf("ActivateUser('') expected NotFound, got %v", err)
	}
	if err := DeactivateUser(c, ""); !client.IsNotFound(err) {
		t.Fatalf("DeactivateUser('') expected NotFound, got %v", err)
	}
}

func TestNodesAndMetaEndpoints(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"nodes": []map[string]any{
				{"name": "node-1", "status": "HEALTHY", "version": "1.40.1"},
			},
		})
	})
	mux.HandleFunc("GET /v1/meta", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hostname": "http://localhost:8080",
			"version":  "1.40.1",
		})
	})

	c, cleanup := newTestClient(mux)
	defer cleanup()

	nodes, err := ListNodes(c)
	if err != nil || len(nodes) != 1 || nodes[0].Name != "node-1" {
		t.Fatalf("ListNodes: %v (%v)", err, nodes)
	}

	meta, err := GetMeta(c)
	if err != nil || meta.Version != "1.40.1" {
		t.Fatalf("GetMeta: %v (%v)", err, meta)
	}
}
