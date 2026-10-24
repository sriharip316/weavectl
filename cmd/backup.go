package cmd

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sriharip316/weavectl/internal/client"
	"github.com/sriharip316/weavectl/internal/weaviate"
)

// backupPollInterval separates status polls while waiting with --wait;
// a var so tests can shorten it.
var backupPollInterval = time.Second

// newCreateBackupCommand starts a backup: fire-and-forget by default,
// polling only with --wait.
func newCreateBackupCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup BACKEND ID",
		Short: "Start a backup of selected collections",
		Long: `Start a backup on a backend (filesystem, s3, gcs, azure) and
return immediately; the backup itself runs asynchronously on the server.

Without --include/--exclude every collection the caller may back up is
included; both flags together are rejected. Backend credentials come
from the server's own configuration, not from weavectl.

With --wait the command polls until the backup reaches SUCCESS, FAILED
or CANCELED and exits non-zero on failure.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			backend := weaviate.NormalizeBackend(args[0])
			id := args[1]
			if err := weaviate.ValidateBackupID(id); err != nil {
				return err
			}
			body, err := backupBodyFromFlags(cmd, id)
			if err != nil {
				return err
			}
			c, err := newClient()
			if err != nil {
				return err
			}
			resp, err := weaviate.CreateBackup(c, backend, body)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "backup/%s/%s started%s\n", backend, id, classesSuffix(resp.Classes))
			return finishBackup(cmd, c, backend, id)
		},
	}
	f := cmd.Flags()
	f.StringSlice("include", nil, "collections to include (comma list, wildcards allowed)")
	f.StringSlice("exclude", nil, "collections to exclude (comma list, wildcards allowed)")
	f.StringSlice("include-users", nil, "db users to include (wildcards allowed; omit for all)")
	f.StringSlice("include-roles", nil, "rbac roles to include (wildcards allowed; omit for all)")
	f.Int("cpu-percentage", 0, "cpu core utilization 1-80")
	f.String("compression-level", "", "compression level (e.g. BestSpeed, ZstdBestCompression)")
	f.String("bucket", "", "backup bucket/container/volume")
	f.String("path", "", "path within the bucket")
	f.String("incremental-of", "", "base backup id for a file-based incremental backup")
	f.Bool("wait", false, "wait for the backup to finish")
	f.Duration("wait-timeout", 10*time.Minute, "give up waiting after this duration (with --wait)")
	return cmd
}

// newDeleteBackupCommand cancels an in-progress backup (the API's DELETE).
func newDeleteBackupCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "backup BACKEND ID",
		Short:   "Cancel an in-progress backup",
		Args:    cobra.ExactArgs(2),
		Aliases: nil,
		RunE: func(cmd *cobra.Command, args []string) error {
			backend := weaviate.NormalizeBackend(args[0])
			id := args[1]
			if err := weaviate.ValidateBackupID(id); err != nil {
				return err
			}
			c, err := newClient()
			if err != nil {
				return err
			}
			if err := weaviate.CancelBackup(c, backend, id, statusQueryFromFlags(cmd)); err != nil {
				return notFoundWrap(err, "backup", backend+"/"+id)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "backup %q canceled\n", backend+"/"+id)
			return nil
		},
	}
	cmd.Flags().String("bucket", "", "backup bucket/container/volume")
	cmd.Flags().String("path", "", "path within the bucket")
	return cmd
}

// finishBackup honors --wait: poll the creation status or print the hint.
func finishBackup(cmd *cobra.Command, c *client.Client, backend, id string) error {
	out := cmd.OutOrStdout()
	wait, _ := cmd.Flags().GetBool("wait")
	if !wait {
		_, _ = fmt.Fprintf(out, "check progress: weavectl get backups --backend %s %s\n", backend, id)
		return nil
	}
	timeout, _ := cmd.Flags().GetDuration("wait-timeout")
	return waitTerminal(out, timeout, func() (string, string, error) {
		st, err := weaviate.GetBackupStatus(c, backend, id, weaviate.StatusQuery{})
		if err != nil {
			return "", "", err
		}
		return st.Status, backupDetail(st.Status, st.Error, st.Size, st.CompletedAt), nil
	})
}

// waitTerminal polls until a terminal backup/restore phase, printing each
// status transition. detail decorates the terminal line (backup size, or
// the server's failure message) and is carried into the returned error.
func waitTerminal(out io.Writer, timeout time.Duration, poll func() (status, detail string, err error)) error {
	var last string
	deadline := time.Now().Add(timeout)
	for {
		status, detail, err := poll()
		if err != nil {
			return err
		}
		if status != last {
			if detail != "" {
				_, _ = fmt.Fprintf(out, "status: %s (%s)\n", status, detail)
			} else {
				_, _ = fmt.Fprintf(out, "status: %s\n", status)
			}
			last = status
		}
		terminal, success := weaviate.BackupPhaseDone(status)
		if terminal {
			if success {
				return nil
			}
			msg := strings.ToLower(status)
			if detail != "" {
				msg += ": " + detail
			}
			return fmt.Errorf("%s", msg)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("timed out after %s waiting (last status: %s)", timeout, status)
		}
		time.Sleep(backupPollInterval)
	}
}

// backupDetail summarizes a terminal backup status.
func backupDetail(status, errMsg string, size float64, completedAt string) string {
	terminal, success := weaviate.BackupPhaseDone(status)
	if !terminal {
		return ""
	}
	if !success {
		return errMsg
	}
	detail := fmt.Sprintf("size %.3f GiB", size)
	if completedAt != "" {
		detail += ", completed at " + completedAt
	}
	return detail
}

// backupBodyFromFlags assembles the POST /backups/{backend} body, running
// it through the same validation as manifest-driven backups.
func backupBodyFromFlags(cmd *cobra.Command, id string) (map[string]any, error) {
	f := cmd.Flags()
	body := map[string]any{"id": id}
	if v, _ := f.GetStringSlice("include"); len(v) > 0 {
		body["include"] = v
	}
	if v, _ := f.GetStringSlice("exclude"); len(v) > 0 {
		body["exclude"] = v
	}
	if v, _ := f.GetStringSlice("include-users"); len(v) > 0 {
		body["includeUsers"] = v
	}
	if v, _ := f.GetStringSlice("include-roles"); len(v) > 0 {
		body["includeRoles"] = v
	}
	config := map[string]any{}
	if v, _ := f.GetString("bucket"); v != "" {
		config["Bucket"] = v
	}
	if v, _ := f.GetString("path"); v != "" {
		config["Path"] = v
	}
	if n, _ := f.GetInt("cpu-percentage"); n > 0 {
		config["CPUPercentage"] = n
	}
	if v, _ := f.GetString("compression-level"); v != "" {
		config["CompressionLevel"] = v
	}
	if len(config) > 0 {
		body["config"] = config
	}
	if v, _ := f.GetString("incremental-of"); v != "" {
		body["incremental_base_backup_id"] = v
	}
	return weaviate.PrepareBackupBody(body)
}

func statusQueryFromFlags(cmd *cobra.Command) weaviate.StatusQuery {
	bucket, _ := cmd.Flags().GetString("bucket")
	path, _ := cmd.Flags().GetString("path")
	return weaviate.StatusQuery{Bucket: bucket, Path: path}
}

func classesSuffix(classes []string) string {
	if len(classes) == 0 {
		return ""
	}
	return " (collections: " + join(classes) + ")"
}

// ---- get backups rendering ----

var (
	backupHeaders        = []string{"ID", "STATUS", "CLASSES", "SIZE (GIB)", "STARTED", "COMPLETED", "BASE"}
	backupStatusHeaders  = []string{"ID", "STATUS", "SIZE (GIB)", "STARTED", "COMPLETED", "ERROR"}
	restoreStatusHeaders = []string{"ID", "STATUS", "ERROR"}
)

// backupManifest shapes a list entry as an apply-ready manifest document
// so `get backups -o yaml | apply -f -` round-trips.
func backupManifest(backend string, e weaviate.BackupListEntry) map[string]any {
	m := map[string]any{"kind": "Backup", "backend": backend, "id": e.ID}
	if len(e.Classes) > 0 {
		m["include"] = e.Classes
	}
	return m
}

func backupRow(e weaviate.BackupListEntry) []string {
	return []string{
		e.ID,
		e.Status,
		strconv.Itoa(len(e.Classes)),
		sizeStr(e.Size),
		strOr(e.StartedAt, "-"),
		strOr(e.CompletedAt, "-"),
		strOr(e.IncrementalBaseBackupID, "-"),
	}
}

func backupStatusRow(st weaviate.BackupStatus) []string {
	return []string{
		st.ID,
		st.Status,
		sizeStr(st.Size),
		strOr(st.StartedAt, "-"),
		strOr(st.CompletedAt, "-"),
		strOr(st.Error, "-"),
	}
}

func sizeStr(size float64) string {
	return fmt.Sprintf("%.3f", size)
}
