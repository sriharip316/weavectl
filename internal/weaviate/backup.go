package weaviate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/sriharip316/weavectl/internal/client"
)

// Backup ids must be URL-safe and work as filesystem paths: lowercase
// letters, digits, underscore and minus only.
var backupIDPattern = regexp.MustCompile(`^[a-z0-9_-]+$`)

// Backup phases, from the backup API. Phases move STARTED -> TRANSFERRING
// -> TRANSFERRED -> FINALIZING -> SUCCESS; failures and cancels end in
// FAILED or CANCELED.
const (
	BackupPhaseSuccess  = "SUCCESS"
	BackupPhaseFailed   = "FAILED"
	BackupPhaseCanceled = "CANCELED"
)

// BackupPhaseDone reports whether a backup/restore phase is final, and
// whether it ended successfully.
func BackupPhaseDone(status string) (terminal, success bool) {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case BackupPhaseSuccess:
		return true, true
	case BackupPhaseFailed, BackupPhaseCanceled:
		return true, false
	}
	return false, false
}

// ValidateBackupID enforces the server's backup id charset client-side so
// typos fail fast instead of as a 422 round trip.
func ValidateBackupID(id string) error {
	if !backupIDPattern.MatchString(id) {
		return fmt.Errorf("invalid backup id %q (lowercase letters, numbers, underscore and minus only)", id)
	}
	return nil
}

// NormalizeBackend lowercases a backend name ("FS" -> "fs").
func NormalizeBackend(backend string) string {
	return strings.ToLower(strings.TrimSpace(backend))
}

// PrepareBackupBody turns a backup manifest object (or an imperative
// flag-built body) into a POST /backups/{backend} request body: the
// scoping fields "kind" and "backend" are stripped, the rest is passed
// through verbatim. It validates the id and the include/exclude pairing.
func PrepareBackupBody(obj map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		if k == "kind" || k == "backend" {
			continue
		}
		out[k] = v
	}
	id, _ := out["id"].(string)
	if err := ValidateBackupID(id); err != nil {
		return nil, fmt.Errorf("backup id: %w", err)
	}
	if err := checkIncludeExclude(out); err != nil {
		return nil, fmt.Errorf("backup %q: %w", id, err)
	}
	return out, nil
}

// PrepareRestoreBody turns a restore manifest object into a POST
// /backups/{backend}/{id}/restore request body: the scoping fields "kind",
// "backend" and "id" are stripped, the rest passes through verbatim.
func PrepareRestoreBody(obj map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		if k == "kind" || k == "backend" || k == "id" {
			continue
		}
		out[k] = v
	}
	if err := checkIncludeExclude(out); err != nil {
		return nil, err
	}
	return out, nil
}

func checkIncludeExclude(body map[string]any) error {
	if hasLeafList(body, "include") && hasLeafList(body, "exclude") {
		return fmt.Errorf("include and exclude are mutually exclusive")
	}
	return nil
}

func hasLeafList(obj map[string]any, key string) bool {
	switch v := obj[key].(type) {
	case []any:
		return len(v) > 0
	case []string:
		return len(v) > 0
	}
	return false
}

// StatusQuery carries the optional bucket/path query parameters the
// backup status and cancel endpoints accept for backends that need them.
type StatusQuery struct {
	Bucket string
	Path   string
}

// StatusQueryFromManifest extracts the optional bucket/path location
// override from a backup/restore document's config section, accepting
// either lowercase or API-cased keys. Documents stored outside the
// backend's default location must carry it into every status lookup and
// cancel: the server only finds them there when asked.
func StatusQueryFromManifest(obj map[string]any) StatusQuery {
	cfg, _ := obj["config"].(map[string]any)
	if cfg == nil {
		return StatusQuery{}
	}
	return StatusQuery{
		Bucket: configString(cfg, "bucket", "Bucket"),
		Path:   configString(cfg, "path", "Path"),
	}
}

func configString(cfg map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := cfg[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func (q StatusQuery) query() string {
	v := url.Values{}
	if q.Bucket != "" {
		v.Set("bucket", q.Bucket)
	}
	if q.Path != "" {
		v.Set("path", q.Path)
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// ---- Backup endpoint types ----

// BackupCreateResponse is the immediate reply to starting a backup.
type BackupCreateResponse struct {
	ID      string   `json:"id"`
	Classes []string `json:"classes"`
	Backend string   `json:"backend"`
	Bucket  string   `json:"bucket"`
	Path    string   `json:"path"`
	Error   string   `json:"error"`
	Status  string   `json:"status"`
}

// BackupStatus is the creation status of one backup.
type BackupStatus struct {
	ID                      string  `json:"id"`
	Backend                 string  `json:"backend"`
	Path                    string  `json:"path"`
	Error                   string  `json:"error"`
	Status                  string  `json:"status"`
	StartedAt               string  `json:"startedAt"`
	CompletedAt             string  `json:"completedAt"`
	Size                    float64 `json:"size"`
	IncrementalBaseBackupID string  `json:"incremental_base_backup_id"`
}

// BackupListEntry is one backup in a backend listing.
type BackupListEntry struct {
	ID                      string   `json:"id"`
	Classes                 []string `json:"classes"`
	Status                  string   `json:"status"`
	StartedAt               string   `json:"startedAt"`
	CompletedAt             string   `json:"completedAt"`
	Size                    float64  `json:"size"`
	IncrementalBaseBackupID string   `json:"incremental_base_backup_id"`
}

// RestoreResponse is the immediate reply to starting a restore.
type RestoreResponse struct {
	ID      string   `json:"id"`
	Classes []string `json:"classes"`
	Backend string   `json:"backend"`
	Path    string   `json:"path"`
	Error   string   `json:"error"`
	Status  string   `json:"status"`
}

// RestoreStatus is the status of one restore attempt.
type RestoreStatus struct {
	ID      string `json:"id"`
	Backend string `json:"backend"`
	Path    string `json:"path"`
	Error   string `json:"error"`
	Status  string `json:"status"`
}

// CreateBackup starts a backup on the backend; the body is the raw API
// request (see PrepareBackupBody).
func CreateBackup(ctx context.Context, c *client.Client, backend string, body map[string]any) (*BackupCreateResponse, error) {
	raw, err := c.Do(ctx, http.MethodPost, "/backups/"+url.PathEscape(backend), body)
	if err != nil {
		return nil, err
	}
	var out BackupCreateResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse backup response: %w", err)
	}
	return &out, nil
}

// ListBackups returns the backups recorded on a backend, newest first
// (the server's default ordering).
func ListBackups(ctx context.Context, c *client.Client, backend string) ([]BackupListEntry, error) {
	raw, err := doGet(ctx, c, "/backups/"+url.PathEscape(backend))
	if err != nil {
		return nil, err
	}
	var out []BackupListEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse backups: %w", err)
	}
	return out, nil
}

// GetBackupStatus reports the creation status of one backup.
func GetBackupStatus(ctx context.Context, c *client.Client, backend, id string, q StatusQuery) (*BackupStatus, error) {
	raw, err := doGet(ctx, c, "/backups/"+url.PathEscape(backend)+"/"+url.PathEscape(id)+q.query())
	if err != nil {
		return nil, err
	}
	var out BackupStatus
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse backup status: %w", err)
	}
	return &out, nil
}

// CancelBackup cancels an in-progress backup.
func CancelBackup(ctx context.Context, c *client.Client, backend, id string, q StatusQuery) error {
	_, err := c.Do(ctx, http.MethodDelete, "/backups/"+url.PathEscape(backend)+"/"+url.PathEscape(id)+q.query(), nil)
	return err
}

// RestoreBackup starts restoring a backup; the body is the raw API request
// (see PrepareRestoreBody).
func RestoreBackup(ctx context.Context, c *client.Client, backend, id string, body map[string]any) (*RestoreResponse, error) {
	raw, err := c.Do(ctx, http.MethodPost, "/backups/"+url.PathEscape(backend)+"/"+url.PathEscape(id)+"/restore", body)
	if err != nil {
		return nil, err
	}
	var out RestoreResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse restore response: %w", err)
	}
	return &out, nil
}

// GetRestoreStatus reports the status of a restore attempt.
func GetRestoreStatus(ctx context.Context, c *client.Client, backend, id string, q StatusQuery) (*RestoreStatus, error) {
	raw, err := doGet(ctx, c, "/backups/"+url.PathEscape(backend)+"/"+url.PathEscape(id)+"/restore"+q.query())
	if err != nil {
		return nil, err
	}
	var out RestoreStatus
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse restore status: %w", err)
	}
	return &out, nil
}

// CancelRestore cancels an in-progress restore.
func CancelRestore(ctx context.Context, c *client.Client, backend, id string, q StatusQuery) error {
	_, err := c.Do(ctx, http.MethodDelete, "/backups/"+url.PathEscape(backend)+"/"+url.PathEscape(id)+"/restore"+q.query(), nil)
	return err
}
