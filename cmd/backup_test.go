package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sriharip316/weavectl/internal/client"
)

// backupMock is a stateful fake of the backup API. Each backup and
// restore moves through a scripted list of statuses, one per status GET,
// sticking on the last; that drives both apply convergence and --wait.
type backupMock struct {
	mu       sync.Mutex
	backups  map[string]*mockBackup
	restores map[string]*mockBackup
	bodies   []string // recorded JSON request bodies
	// failNew makes subsequently created backups fail: the status script
	// ends FAILED with createErrMsg.
	failNew      bool
	createErrMsg string
	// failStatusGets makes the next N creation-status GETs answer 503,
	// simulating a brief server hiccup in the middle of a --wait poll.
	failStatusGets int
}

type mockBackup struct {
	backend  string
	id       string
	classes  []string
	statuses []string
	gets     int
	errMsg   string
}

func (b *mockBackup) advance() string {
	i := b.gets
	if i >= len(b.statuses) {
		i = len(b.statuses) - 1
	}
	b.gets++
	return b.statuses[i]
}

func newBackupMock() *backupMock {
	return &backupMock{
		backups:  map[string]*mockBackup{},
		restores: map[string]*mockBackup{},
	}
}

func (m *backupMock) seedBackup(backend, id string, statuses ...string) *mockBackup {
	b := &mockBackup{backend: backend, id: id, classes: []string{"Articles"}, statuses: statuses}
	if len(statuses) == 0 {
		b.statuses = []string{"SUCCESS"}
	}
	m.backups[backend+"/"+id] = b
	return b
}

func (m *backupMock) handler() http.Handler {
	mux := http.NewServeMux()

	writeJSON := func(w http.ResponseWriter, v any) {
		_ = json.NewEncoder(w).Encode(v)
	}

	mux.HandleFunc("POST /v1/backups/{backend}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body)
		m.bodies = append(m.bodies, string(raw))
		backend := r.PathValue("backend")
		id, _ := body["id"].(string)
		classes := []string{}
		if inc, ok := body["include"].([]any); ok {
			for _, class := range inc {
				classes = append(classes, fmt.Sprint(class))
			}
		}
		b := &mockBackup{backend: backend, id: id, classes: classes,
			statuses: []string{"TRANSFERRING", "SUCCESS"}}
		if m.failNew {
			b.statuses = []string{"TRANSFERRING", "FAILED"}
			b.errMsg = m.createErrMsg
		}
		m.backups[backend+"/"+id] = b
		writeJSON(w, map[string]any{
			"id": id, "classes": classes, "backend": backend, "status": "STARTED",
		})
	})

	mux.HandleFunc("GET /v1/backups/{backend}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		backend := r.PathValue("backend")
		out := []map[string]any{}
		for _, b := range m.backups {
			if b.backend != backend {
				continue
			}
			out = append(out, map[string]any{
				"id": b.id, "classes": b.classes, "status": b.advance(),
				"startedAt": "2026-01-01T10:00:00Z", "completedAt": "2026-01-01T10:01:00Z",
				"size": 0.5, "incremental_base_backup_id": "",
			})
		}
		writeJSON(w, out)
	})

	statusReply := func(w http.ResponseWriter, b *mockBackup) {
		status := b.advance()
		resp := map[string]any{
			"id": b.id, "backend": b.backend, "status": status,
			"startedAt": "2026-01-01T10:00:00Z",
		}
		switch status {
		case "SUCCESS":
			resp["completedAt"] = "2026-01-01T10:01:00Z"
			resp["size"] = 0.012
		case "FAILED", "CANCELED":
			resp["error"] = b.errMsg
		}
		writeJSON(w, resp)
	}

	mux.HandleFunc("GET /v1/backups/{backend}/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.failStatusGets > 0 {
			m.failStatusGets--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		b, ok := m.backups[r.PathValue("backend")+"/"+r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		statusReply(w, b)
	})

	mux.HandleFunc("DELETE /v1/backups/{backend}/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		key := r.PathValue("backend") + "/" + r.PathValue("id")
		b, ok := m.backups[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b.statuses = []string{"CANCELED"}
		b.gets = 0
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /v1/backups/{backend}/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		backend, id := r.PathValue("backend"), r.PathValue("id")
		if _, ok := m.backups[backend+"/"+id]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body)
		m.bodies = append(m.bodies, string(raw))
		b := &mockBackup{backend: backend, id: id, classes: []string{"Articles"},
			statuses: []string{"TRANSFERRING", "SUCCESS"}}
		if m.failNew {
			b.statuses = []string{"TRANSFERRING", "FAILED"}
			b.errMsg = m.createErrMsg
		}
		m.restores[backend+"/"+id] = b
		writeJSON(w, map[string]any{
			"id": id, "classes": b.classes, "backend": backend, "status": "STARTED",
		})
	})

	mux.HandleFunc("GET /v1/backups/{backend}/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		b, ok := m.restores[r.PathValue("backend")+"/"+r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		status := b.advance()
		resp := map[string]any{"id": b.id, "backend": b.backend, "status": status}
		if status == "FAILED" || status == "CANCELED" {
			resp["error"] = b.errMsg
		}
		writeJSON(w, resp)
	})

	mux.HandleFunc("DELETE /v1/backups/{backend}/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if _, ok := m.restores[r.PathValue("backend")+"/"+r.PathValue("id")]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}

func writeTempManifest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const testBackupManifest = `kind: Backup
backend: filesystem
id: snap-1
include: [Articles]
`

func TestCLI_BackupApplyLifecycle(t *testing.T) {
	mock := newBackupMock()
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	file := writeTempManifest(t, testBackupManifest)

	out, _, err := executeCommand([]string{"apply", "-f", file, "--server", srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"backup/filesystem/snap-1 created",
		"started (collections: Articles)",
		"check progress: weavectl get backups --backend filesystem snap-1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("apply output missing %q:\n%s", want, out)
		}
	}

	out, _, err = executeCommand([]string{"apply", "-f", file, "--server", srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "backup/filesystem/snap-1 unchanged") ||
		!strings.Contains(out, "backup in progress: TRANSFERRING") {
		t.Errorf("re-apply output:\n%s", out)
	}

	out, _, err = executeCommand([]string{"apply", "-f", file, "--server", srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "backup/filesystem/snap-1 unchanged") ||
		!strings.Contains(out, "backup already exists") {
		t.Errorf("completed re-apply output:\n%s", out)
	}
}

func TestCLI_BackupApplyRestart(t *testing.T) {
	mock := newBackupMock()
	b := mock.seedBackup("filesystem", "snap-1", "FAILED")
	b.errMsg = "disk full"
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	file := writeTempManifest(t, testBackupManifest)

	out, _, err := executeCommand([]string{"apply", "-f", file, "--server", srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "created") || !strings.Contains(out, "previous attempt FAILED: disk full") {
		t.Errorf("restart output:\n%s", out)
	}
}

func TestCLI_BackupApplyValidation(t *testing.T) {
	mock := newBackupMock()
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	file := writeTempManifest(t, "kind: Backup\nbackend: filesystem\nid: snap-1\ninclude: [A]\nexclude: [B]\n")
	_, _, err := executeCommand([]string{"apply", "-f", file, "--server", srv.URL})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected mutual-exclusion error, got %v", err)
	}

	file = writeTempManifest(t, "kind: Backup\nbackend: filesystem\nid: Bad Id\n")
	_, _, err = executeCommand([]string{"apply", "-f", file, "--server", srv.URL})
	if err == nil || !strings.Contains(err.Error(), "invalid backup id") {
		t.Errorf("expected invalid-id error, got %v", err)
	}
}

func TestCLI_CreateBackup(t *testing.T) {
	mock := newBackupMock()
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	// Fire-and-forget: prints the start line and the progress hint.
	out, _, err := executeCommand([]string{
		"create", "backup", "FileSystem", "snap-1", "--include", "Articles",
		"--server", srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "backup/filesystem/snap-1 started (collections: Articles)") {
		t.Errorf("create output:\n%s", out)
	}
	if !strings.Contains(out, "check progress: weavectl get backups --backend filesystem snap-1") {
		t.Errorf("create output missing hint:\n%s", out)
	}
	// Backend is normalized; config keys keep the API's casing.
	joined := strings.Join(mock.bodies, "\n")
	if !strings.Contains(joined, `"id":"snap-1"`) || !strings.Contains(joined, `"include":["Articles"]`) {
		t.Errorf("recorded bodies: %v", mock.bodies)
	}

	// Flag validation.
	_, _, err = executeCommand([]string{
		"create", "backup", "filesystem", "snap-2", "--include", "A", "--exclude", "B",
		"--server", srv.URL,
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected mutual-exclusion error, got %v", err)
	}
	_, _, err = executeCommand([]string{"create", "backup", "filesystem", "Bad Id", "--server", srv.URL})
	if err == nil || !strings.Contains(err.Error(), "invalid backup id") {
		t.Errorf("expected invalid-id error, got %v", err)
	}
}

func TestCLI_CreateBackupWait(t *testing.T) {
	orig := backupPollInterval
	backupPollInterval = time.Millisecond
	defer func() { backupPollInterval = orig }()

	mock := newBackupMock()
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	out, _, err := executeCommand([]string{
		"create", "backup", "filesystem", "snap-1", "--include", "Articles",
		"--wait", "--server", srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "status: TRANSFERRING") || !strings.Contains(out, "status: SUCCESS") {
		t.Errorf("wait output:\n%s", out)
	}
	if !strings.Contains(out, "size 0.012 GiB") {
		t.Errorf("success detail missing:\n%s", out)
	}

	// A failed backup exits non-zero carrying the server's message.
	mock.failNew = true
	mock.createErrMsg = "disk full"
	_, _, err = executeCommand([]string{
		"create", "backup", "filesystem", "snap-2", "--include", "Articles",
		"--wait", "--server", srv.URL,
	})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("expected failure error, got %v", err)
	}
}

func TestCLI_GetBackups(t *testing.T) {
	mock := newBackupMock()
	mock.seedBackup("filesystem", "snap-1", "SUCCESS")
	mock.seedBackup("filesystem", "snap-2", "TRANSFERRING")
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	cases := []struct {
		name       string
		args       []string
		wantIn     string
		wantErr    bool
		wantErrMsg string
	}{
		{"list table", []string{"get", "backups", "--backend", "filesystem"}, "snap-1", false, ""},
		{"list headers", []string{"get", "backups", "--backend", "filesystem"}, "COMPLETED", false, ""},
		{"single status", []string{"get", "backups", "--backend", "filesystem", "snap-1"}, "SUCCESS", false, ""},
		{"single -o json", []string{"get", "backups", "--backend", "filesystem", "snap-1", "-o", "json"}, `"status": "SUCCESS"`, false, ""},
		{"missing backend", []string{"get", "backups"}, "", true, "requires a backend"},
		{"unknown backup", []string{"get", "backups", "--backend", "filesystem", "nope"}, "", true, "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append(tc.args, "--server", srv.URL)
			out, _, err := executeCommand(args)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrMsg) {
					t.Fatalf("want error %q, got %v", tc.wantErrMsg, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, tc.wantIn) {
				t.Errorf("output missing %q:\n%s", tc.wantIn, out)
			}
		})
	}
}

func TestCLI_GetBackupsYAMLRoundTrip(t *testing.T) {
	mock := newBackupMock()
	mock.seedBackup("filesystem", "snap-1", "SUCCESS")
	mock.seedBackup("filesystem", "snap-2", "SUCCESS")
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	out, _, err := executeCommand([]string{"get", "backups", "--backend", "filesystem", "-o", "yaml", "--server", srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kind: Backup", "backend: filesystem", "id: snap-1", "id: snap-2", "---"} {
		if !strings.Contains(out, want) {
			t.Fatalf("yaml output missing %q:\n%s", want, out)
		}
	}

	// The exported YAML must apply cleanly: both backups exist and are
	// complete, so apply reports them unchanged.
	file := writeTempManifest(t, out)
	out, _, err = executeCommand([]string{"apply", "-f", file, "--server", srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "backup/filesystem/snap-1 unchanged") ||
		!strings.Contains(out, "backup/filesystem/snap-2 unchanged") {
		t.Errorf("round-trip apply output:\n%s", out)
	}
}

func TestCLI_DeleteBackup(t *testing.T) {
	mock := newBackupMock()
	mock.seedBackup("filesystem", "snap-1", "TRANSFERRING")
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	out, _, err := executeCommand([]string{"delete", "backup", "filesystem", "snap-1", "--server", srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `backup "filesystem/snap-1" canceled`) {
		t.Errorf("delete output:\n%s", out)
	}

	_, _, err = executeCommand([]string{"delete", "backup", "filesystem", "nope", "--server", srv.URL})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected NotFound, got %v", err)
	}
}

func TestCLI_Restore(t *testing.T) {
	orig := backupPollInterval
	backupPollInterval = time.Millisecond
	defer func() { backupPollInterval = orig }()

	mock := newBackupMock()
	mock.seedBackup("filesystem", "snap-1", "SUCCESS")
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	// Manifest-driven restore.
	file := writeTempManifest(t, "kind: Restore\nbackend: filesystem\nid: snap-1\ninclude: [Articles]\nnode_mapping:\n  n1: n2\n")
	out, _, err := executeCommand([]string{"apply", "-f", file, "--server", srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "restore/filesystem/snap-1 created") ||
		!strings.Contains(out, "restore started") {
		t.Errorf("restore apply output:\n%s", out)
	}
	if !strings.Contains(strings.Join(mock.bodies, "\n"), `"node_mapping":{"n1":"n2"}`) {
		t.Errorf("restore body missing node_mapping: %v", mock.bodies)
	}

	// Imperative restore with --wait.
	out, _, err = executeCommand([]string{
		"restore", "filesystem", "snap-1", "--include", "Articles",
		"--wait", "--server", srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "restore/filesystem/snap-1 started") || !strings.Contains(out, "status: SUCCESS") {
		t.Errorf("restore --wait output:\n%s", out)
	}

	// Status and cancel.
	out, _, err = executeCommand([]string{"restore", "status", "filesystem", "snap-1", "--server", srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "snap-1") || !strings.Contains(out, "SUCCESS") {
		t.Errorf("restore status output:\n%s", out)
	}
	out, _, err = executeCommand([]string{"restore", "cancel", "filesystem", "snap-1", "--server", srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `restore "filesystem/snap-1" canceled`) {
		t.Errorf("restore cancel output:\n%s", out)
	}

	// Restoring an unknown backup surfaces NotFound.
	_, _, err = executeCommand([]string{"restore", "filesystem", "nope", "--server", srv.URL})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected NotFound, got %v", err)
	}
}

func TestCLI_CreateBackupFromFile(t *testing.T) {
	// create -f keeps insert-only semantics: an existing backup id is
	// rejected instead of reported unchanged.
	mock := newBackupMock()
	mock.seedBackup("filesystem", "snap-1", "SUCCESS")
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	file := writeTempManifest(t, testBackupManifest)

	_, _, err := executeCommand([]string{"create", "-f", file, "--server", srv.URL})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected AlreadyExists error, got %v", err)
	}
}

// ---- waitTerminal: cancellation and transient-error resilience ----

func TestWaitTerminalCancelDuringSleep(t *testing.T) {
	origInterval, origRetries := backupPollInterval, backupPollRetries
	backupPollInterval = 10 * time.Second
	defer func() { backupPollInterval, backupPollRetries = origInterval, origRetries }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	polls := 0
	start := time.Now()
	err := waitTerminal(ctx, io.Discard, time.Minute, func(context.Context) (string, string, error) {
		polls++
		cancel() // the user hits Ctrl+C while waitTerminal is between polls
		return "TRANSFERRING", "", nil
	})
	if time.Since(start) > 5*time.Second {
		t.Fatalf("cancel during the poll sleep took %s to notice", time.Since(start))
	}
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("want canceled error, got %v", err)
	}
	if polls != 1 {
		t.Errorf("polls = %d, want 1", polls)
	}
}

func TestWaitTerminalRetriesTransientErrors(t *testing.T) {
	origInterval := backupPollInterval
	backupPollInterval = time.Millisecond
	defer func() { backupPollInterval = origInterval }()

	polls := 0
	err := waitTerminal(context.Background(), io.Discard, 10*time.Second, func(context.Context) (string, string, error) {
		polls++
		if polls <= 2 {
			return "", "", errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")
		}
		return "SUCCESS", "", nil
	})
	if err != nil {
		t.Fatalf("transient poll errors should be retried, got %v", err)
	}
	if polls != 3 {
		t.Errorf("polls = %d, want 3", polls)
	}
}

func TestWaitTerminalFailsFastOnAPIError(t *testing.T) {
	origInterval := backupPollInterval
	backupPollInterval = time.Millisecond
	defer func() { backupPollInterval = origInterval }()

	polls := 0
	err := waitTerminal(context.Background(), io.Discard, 10*time.Second, func(context.Context) (string, string, error) {
		polls++
		return "", "", &client.APIError{StatusCode: http.StatusNotFound, Op: "GET /v1/backups/filesystem/snap-1"}
	})
	if err == nil {
		t.Fatal("definitive API errors must fail fast, got nil")
	}
	if polls != 1 {
		t.Errorf("polls = %d, want 1 (no retries for a definitive 404)", polls)
	}
}

func TestWaitTerminalTransientThresholdExhausted(t *testing.T) {
	origInterval, origRetries := backupPollInterval, backupPollRetries
	backupPollInterval, backupPollRetries = time.Millisecond, 2
	defer func() { backupPollInterval, backupPollRetries = origInterval, origRetries }()

	polls := 0
	err := waitTerminal(context.Background(), io.Discard, 10*time.Second, func(context.Context) (string, string, error) {
		polls++
		return "", "", errors.New("read tcp: connection reset by peer")
	})
	if err == nil {
		t.Fatal("expected the last transient error after exhausting retries")
	}
	if polls != 3 { // 1 initial + backupPollRetries retries
		t.Errorf("polls = %d, want 3", polls)
	}
}

func TestTransientPollError(t *testing.T) {
	cases := []struct {
		err       error
		transient bool
	}{
		{&client.APIError{StatusCode: 500}, true},
		{&client.APIError{StatusCode: http.StatusServiceUnavailable}, true},
		{&client.APIError{StatusCode: http.StatusTooManyRequests}, true},
		{&client.APIError{StatusCode: http.StatusNotFound}, false},
		{&client.APIError{StatusCode: http.StatusUnauthorized}, false},
		{&client.APIError{StatusCode: http.StatusUnprocessableEntity}, false},
		{errors.New("dial tcp: connection refused"), true},
		{io.ErrUnexpectedEOF, true},
	}
	for _, tc := range cases {
		if got := transientPollError(tc.err); got != tc.transient {
			t.Errorf("transientPollError(%v) = %v, want %v", tc.err, got, tc.transient)
		}
	}
}

func TestCLI_BackupWaitRecoversFromTransientErrors(t *testing.T) {
	origInterval := backupPollInterval
	backupPollInterval = time.Millisecond
	defer func() { backupPollInterval = origInterval }()

	mock := newBackupMock()
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	// The server hiccups twice mid-wait; the wait must ride it out.
	mock.failStatusGets = 2
	out, _, err := executeCommand([]string{
		"create", "backup", "filesystem", "snap-blink", "--include", "Articles",
		"--wait", "--server", srv.URL,
	})
	if err != nil {
		t.Fatalf("wait should survive transient status failures, got %v", err)
	}
	if !strings.Contains(out, "status poll failed (1/5)") {
		t.Errorf("retry progress missing from output:\n%s", out)
	}
	if !strings.Contains(out, "status: SUCCESS") {
		t.Errorf("wait did not reach SUCCESS:\n%s", out)
	}
}
