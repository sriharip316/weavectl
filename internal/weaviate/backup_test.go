package weaviate

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sriharip316/weavectl/internal/client"
)

func TestCreateBackup(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/backups/{backend}", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "snap-1", "classes": []string{"Articles", "Books"},
			"backend": "filesystem", "status": "STARTED",
		})
	})
	c, done := newTestClient(mux)
	defer done()

	resp, err := CreateBackup(c, "filesystem", map[string]any{
		"id":      "snap-1",
		"include": []string{"Articles", "Books"},
		"config":  map[string]any{"CPUPercentage": 60},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/backups/filesystem" {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody["id"] != "snap-1" {
		t.Errorf("body id = %v", gotBody["id"])
	}
	cfg, _ := gotBody["config"].(map[string]any)
	if cfg["CPUPercentage"] != float64(60) {
		t.Errorf("config key casing lost: %v", gotBody["config"])
	}
	if resp.Status != "STARTED" || len(resp.Classes) != 2 {
		t.Errorf("response = %+v", resp)
	}
}

func TestListAndGetBackupStatus(t *testing.T) {
	var gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/backups/{backend}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"id": "snap-1", "classes": []string{"Articles"}, "status": "SUCCESS",
			"startedAt": "2026-01-01T10:00:00Z", "completedAt": "2026-01-01T10:01:00Z",
			"size": 0.5, "incremental_base_backup_id": "base-0",
		}})
	})
	mux.HandleFunc("GET /v1/backups/{backend}/{id}", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		if r.PathValue("id") == "missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "snap-1", "backend": "filesystem", "status": "TRANSFERRING",
			"error": "", "size": 0.25,
		})
	})
	c, done := newTestClient(mux)
	defer done()

	list, err := ListBackups(c, "filesystem")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "snap-1" || list[0].Size != 0.5 ||
		list[0].IncrementalBaseBackupID != "base-0" || len(list[0].Classes) != 1 {
		t.Fatalf("list = %+v", list)
	}

	st, err := GetBackupStatus(context.Background(), c, "filesystem", "snap-1", StatusQuery{Bucket: "b", Path: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "TRANSFERRING" {
		t.Errorf("status = %q", st.Status)
	}
	if want := "bucket=b&path=p"; gotQuery != want {
		t.Errorf("query = %q, want %q", gotQuery, want)
	}

	_, err = GetBackupStatus(context.Background(), c, "filesystem", "missing", StatusQuery{})
	if !client.IsNotFound(err) {
		t.Errorf("expected NotFound, got %v", err)
	}
}

func TestCancelBackup(t *testing.T) {
	var gotMethod, gotPath string
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v1/backups/{backend}/{id}", func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	c, done := newTestClient(mux)
	defer done()

	if err := CancelBackup(c, "filesystem", "snap-1", StatusQuery{}); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v1/backups/filesystem/snap-1" {
		t.Errorf("cancel: %s %s", gotMethod, gotPath)
	}
}

func TestRestoreEndpoints(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/backups/{backend}/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "snap-1", "classes": []string{"Articles"}, "status": "STARTED",
		})
	})
	mux.HandleFunc("GET /v1/backups/{backend}/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "snap-1", "status": "SUCCESS", "error": "",
		})
	})
	mux.HandleFunc("DELETE /v1/backups/{backend}/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	c, done := newTestClient(mux)
	defer done()

	resp, err := RestoreBackup(c, "filesystem", "snap-1", map[string]any{
		"include":        []string{"Articles"},
		"node_mapping":   map[string]string{"n1": "n2"},
		"overwriteAlias": true,
		"config":         map[string]any{"rolesOptions": "all"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/backups/filesystem/snap-1/restore" || resp.Status != "STARTED" {
		t.Errorf("restore: path=%q resp=%+v", gotPath, resp)
	}
	if gotBody["overwriteAlias"] != true {
		t.Errorf("body = %v", gotBody)
	}
	if nm, _ := gotBody["node_mapping"].(map[string]any); nm["n1"] != "n2" {
		t.Errorf("node_mapping = %v", gotBody["node_mapping"])
	}

	st, err := GetRestoreStatus(context.Background(), c, "filesystem", "snap-1", StatusQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "SUCCESS" {
		t.Errorf("restore status = %q", st.Status)
	}
	if err := CancelRestore(c, "filesystem", "snap-1", StatusQuery{}); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareBackupBody(t *testing.T) {
	body, err := PrepareBackupBody(map[string]any{
		"kind": "Backup", "backend": "filesystem", "id": "snap-1",
		"include": []any{"Articles"},
		"config":  map[string]any{"CPUPercentage": 60},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := body["kind"]; leaked {
		t.Error("kind must be stripped")
	}
	if _, leaked := body["backend"]; leaked {
		t.Error("backend must be stripped")
	}
	if body["id"] != "snap-1" {
		t.Errorf("id = %v", body["id"])
	}

	if _, err := PrepareBackupBody(map[string]any{
		"id": "snap-1", "include": []any{"A"}, "exclude": []any{"B"},
	}); err == nil {
		t.Error("expected include+exclude rejection")
	}

	if _, err := PrepareBackupBody(map[string]any{"id": "Bad Id"}); err == nil {
		t.Error("expected invalid id rejection")
	}
	if _, err := PrepareBackupBody(map[string]any{}); err == nil {
		t.Error("expected missing id rejection")
	}
}

func TestPrepareRestoreBody(t *testing.T) {
	body, err := PrepareRestoreBody(map[string]any{
		"kind": "Restore", "backend": "filesystem", "id": "snap-1",
		"include": []any{"Articles"}, "overwriteAlias": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"kind", "backend", "id"} {
		if _, leaked := body[key]; leaked {
			t.Errorf("%s must be stripped", key)
		}
	}
	if body["overwriteAlias"] != true {
		t.Errorf("body = %v", body)
	}

	if _, err := PrepareRestoreBody(map[string]any{
		"include": []any{"A"}, "exclude": []any{"B"},
	}); err == nil {
		t.Error("expected include+exclude rejection")
	}
}

func TestStatusQueryFromManifest(t *testing.T) {
	cases := []struct {
		name string
		obj  map[string]any
		want StatusQuery
	}{
		{"lowercase config", map[string]any{
			"id": "x", "config": map[string]any{"bucket": "b", "path": "p"},
		}, StatusQuery{Bucket: "b", Path: "p"}},
		{"api-cased config", map[string]any{
			"id": "x", "config": map[string]any{"Bucket": "b", "Path": "p"},
		}, StatusQuery{Bucket: "b", Path: "p"}},
		{"mixed casing", map[string]any{
			"id": "x", "config": map[string]any{"Bucket": "b", "path": "p"},
		}, StatusQuery{Bucket: "b", Path: "p"}},
		{"path only", map[string]any{
			"id": "x", "config": map[string]any{"path": "p"},
		}, StatusQuery{Path: "p"}},
		{"no config", map[string]any{"id": "x"}, StatusQuery{}},
		{"config not a map", map[string]any{"id": "x", "config": "b"}, StatusQuery{}},
		{"non-string ignored", map[string]any{
			"id": "x", "config": map[string]any{"bucket": 42, "path": "p"},
		}, StatusQuery{Path: "p"}},
		{"empty strings ignored", map[string]any{
			"id": "x", "config": map[string]any{"bucket": "", "path": "p"},
		}, StatusQuery{Path: "p"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StatusQueryFromManifest(tc.obj); got != tc.want {
				t.Errorf("StatusQueryFromManifest() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestBackupHelpers(t *testing.T) {
	for id, ok := range map[string]bool{
		"snap-1": true, "a_b-c9": true, "": false, "Snap": false,
		"with space": false, "café": false,
	} {
		if err := ValidateBackupID(id); (err == nil) != ok {
			t.Errorf("ValidateBackupID(%q) ok=%v, want %v", id, err == nil, ok)
		}
	}

	if NormalizeBackend("FileSystem ") != "filesystem" {
		t.Error("NormalizeBackend must lowercase and trim")
	}

	for status, want := range map[string][2]bool{
		"SUCCESS":      {true, true},
		"FAILED":       {true, false},
		"CANCELED":     {true, false},
		"STARTED":      {false, false},
		"TRANSFERRING": {false, false},
		"FINALIZING":   {false, false},
		"CANCELLING":   {false, false},
	} {
		terminal, success := BackupPhaseDone(status)
		if terminal != want[0] || success != want[1] {
			t.Errorf("BackupPhaseDone(%q) = %v/%v, want %v/%v", status, terminal, success, want[0], want[1])
		}
	}
}
