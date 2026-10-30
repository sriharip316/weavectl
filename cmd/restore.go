package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/sriharip316/weavectl/internal/printer"
	"github.com/sriharip316/weavectl/internal/weaviate"
)

// newRestoreCommand restores a backup: fire-and-forget by default,
// polling only with --wait. status and cancel inspect/stop a restore.
func newRestoreCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restore BACKEND ID",
		Short: "Restore collections from a backup",
		Long: `Restore collections from a backup on the given backend and
return immediately; the restore runs asynchronously on the server.

Requirements: the target cluster must have the same number of nodes as
the source cluster, node names must match (unless --node-mapping
overrides them), and the restored collections must not already exist.

With --wait the command polls until the restore reaches SUCCESS, FAILED
or CANCELED and exits non-zero on failure.

  weavectl restore filesystem my-snapshot
  weavectl restore status filesystem my-snapshot
  weavectl restore cancel filesystem my-snapshot`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			backend := weaviate.NormalizeBackend(args[0])
			id := args[1]
			if err := weaviate.ValidateBackupID(id); err != nil {
				return err
			}
			body, err := restoreBodyFromFlags(cmd)
			if err != nil {
				return err
			}
			c, err := newClient()
			if err != nil {
				return err
			}
			resp, err := weaviate.RestoreBackup(c, backend, id, body)
			if err != nil {
				return notFoundWrap(err, "backup", backend+"/"+id)
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "restore/%s/%s started%s\n", backend, id, classesSuffix(resp.Classes))
			wait, _ := cmd.Flags().GetBool("wait")
			if !wait {
				_, _ = fmt.Fprintf(out, "check progress: weavectl restore status %s %s\n", backend, id)
				return nil
			}
			timeout, _ := cmd.Flags().GetDuration("wait-timeout")
			return waitTerminal(cmd.Context(), out, timeout, func(ctx context.Context) (string, string, error) {
				st, err := weaviate.GetRestoreStatus(ctx, c, backend, id, weaviate.StatusQuery{})
				if err != nil {
					return "", "", err
				}
				detail := ""
				if terminal, success := weaviate.BackupPhaseDone(st.Status); terminal && !success {
					detail = st.Error
				}
				return st.Status, detail, nil
			})
		},
	}
	f := cmd.Flags()
	f.StringSlice("include", nil, "collections to restore (comma list, wildcards allowed; omit for all)")
	f.StringSlice("exclude", nil, "collections to skip (comma list; mutually exclusive with --include)")
	f.StringToString("node-mapping", nil, "remap source node names, old=new (comma list)")
	f.Bool("overwrite-alias", false, "overwrite a collection alias if it conflicts")
	f.String("bucket", "", "backup bucket/container/volume")
	f.String("path", "", "path within the bucket")
	f.Int("cpu-percentage", 0, "cpu core utilization 1-80")
	f.String("restore-roles", "", "restore rbac roles from the backup: noRestore or all")
	f.String("restore-users", "", "restore db users from the backup: noRestore or all")
	f.Bool("wait", false, "wait for the restore to finish")
	f.Duration("wait-timeout", 10*time.Minute, "give up waiting after this duration (with --wait)")
	cmd.AddCommand(newRestoreStatusCommand(), newRestoreCancelCommand())
	return cmd
}

func newRestoreStatusCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status BACKEND ID",
		Short: "Show restore progress for a backup",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := printer.ParseFormat(outputFlag(cmd))
			if err != nil {
				return err
			}
			c, err := newClient()
			if err != nil {
				return err
			}
			backend := weaviate.NormalizeBackend(args[0])
			st, err := weaviate.GetRestoreStatus(cmd.Context(), c, backend, args[1], statusQueryFromFlags(cmd))
			if err != nil {
				return notFoundWrap(err, "restore", backend+"/"+args[1])
			}
			row := []string{st.ID, st.Status, strOr(st.Error, "-")}
			return printObjects(cmd.OutOrStdout(), format, true, []any{st}, restoreStatusHeaders, row)
		},
	}
	cmd.Flags().StringP("output", "o", "table", "output format: table, json or yaml")
	cmd.Flags().String("bucket", "", "backup bucket/container/volume")
	cmd.Flags().String("path", "", "path within the bucket")
	return cmd
}

func newRestoreCancelCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cancel BACKEND ID",
		Short: "Cancel an in-progress restore",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			backend := weaviate.NormalizeBackend(args[0])
			if err := weaviate.CancelRestore(c, backend, args[1], statusQueryFromFlags(cmd)); err != nil {
				return notFoundWrap(err, "restore", backend+"/"+args[1])
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "restore %q canceled\n", backend+"/"+args[1])
			return nil
		},
	}
	cmd.Flags().String("bucket", "", "backup bucket/container/volume")
	cmd.Flags().String("path", "", "path within the bucket")
	return cmd
}

// restoreBodyFromFlags assembles the POST .../restore body, running it
// through the same validation as manifest-driven restores.
func restoreBodyFromFlags(cmd *cobra.Command) (map[string]any, error) {
	f := cmd.Flags()
	body := map[string]any{}
	if v, _ := f.GetStringSlice("include"); len(v) > 0 {
		body["include"] = v
	}
	if v, _ := f.GetStringSlice("exclude"); len(v) > 0 {
		body["exclude"] = v
	}
	if v, _ := f.GetStringToString("node-mapping"); len(v) > 0 {
		body["node_mapping"] = v
	}
	if v, _ := f.GetBool("overwrite-alias"); v {
		body["overwriteAlias"] = true
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
	if v, _ := f.GetString("restore-roles"); v != "" {
		config["rolesOptions"] = v
	}
	if v, _ := f.GetString("restore-users"); v != "" {
		config["usersOptions"] = v
	}
	if len(config) > 0 {
		body["config"] = config
	}
	return weaviate.PrepareRestoreBody(body)
}
