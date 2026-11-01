package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sriharip316/weavectl/internal/client"
	"github.com/sriharip316/weavectl/internal/manifest"
	"github.com/sriharip316/weavectl/internal/weaviate"
)

func newDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete TYPE NAME | delete -f FILE",
		Short: "Delete objects by type and name, or from a manifest file",
		Long: `Delete objects:

  weavectl delete collection NAME     (also: role, user, alias)
  weavectl delete tenant COLLECTION NAME...
  weavectl delete backup BACKEND ID   (cancels an in-progress backup)
  weavectl delete -f FILE             ("-" for stdin; kind+name from the file)

No confirmation prompts; deleting a collection deletes all its data.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			file, _ := cmd.Flags().GetString("filename")
			if file == "" {
				return fmt.Errorf("specify -f FILE or a subcommand (see weavectl delete --help)")
			}
			docs, err := manifest.Load(file)
			if err != nil {
				return err
			}
			c, err := newClient()
			if err != nil {
				return err
			}
			for _, doc := range docs {
				if err := deleteDoc(cmd, c, doc); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringP("filename", "f", "", "manifest file (\"-\" for stdin)")
	cmd.AddCommand(
		newDeleteTypedCommand("collection", "Delete a collection and all its data",
			func(c *client.Client, name string) (string, error) {
				resolved, err := weaviate.ResolveCollection(c, name)
				if err != nil {
					return "", err
				}
				return resolved, weaviate.DeleteCollection(c, resolved)
			}),
		newDeleteTypedCommand("role", "Delete an authorization role",
			func(c *client.Client, name string) (string, error) {
				return name, weaviate.DeleteRole(c, name)
			}),
		newDeleteTypedCommand("user", "Delete a db user",
			func(c *client.Client, name string) (string, error) {
				return name, weaviate.DeleteUser(c, name)
			}),
		newDeleteTypedCommand("alias", "Delete an alias",
			func(c *client.Client, name string) (string, error) {
				resolved, err := weaviate.ResolveAlias(c, name)
				if err != nil {
					return "", err
				}
				return resolved, weaviate.DeleteAlias(c, resolved)
			}),
		newDeleteTenantCommand(),
		newDeleteBackupCommand(),
	)
	return cmd
}

func newDeleteTypedCommand(kind, short string, del func(*client.Client, string) (string, error)) *cobra.Command {
	return &cobra.Command{
		Use:     fmt.Sprintf("%s NAME", kind),
		Short:   short,
		Args:    cobra.ExactArgs(1),
		Aliases: nil,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			resolvedName, err := del(c, args[0])
			if err != nil {
				return notFoundWrap(err, kind, args[0])
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %q deleted\n", kind, resolvedName)
			return nil
		},
	}
}

func newDeleteTenantCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "tenant COLLECTION NAME...",
		Short: "Delete tenants from a collection",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			collection, err := weaviate.ResolveCollection(c, args[0])
			if err != nil {
				return notFoundWrap(err, "collection", args[0])
			}
			if err := weaviate.DeleteTenants(c, collection, args[1:]); err != nil {
				return notFoundWrap(err, "collection", collection)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "tenant %q deleted: %s\n", collection, join(args[1:]))
			return nil
		},
	}
}

func deleteDoc(cmd *cobra.Command, c *client.Client, doc manifest.Document) error {
	out := cmd.OutOrStdout()
	switch doc.Kind {
	case manifest.KindCollection:
		resolved, err := weaviate.ResolveCollection(c, doc.Name)
		if err != nil {
			return notFoundWrap(err, "collection", doc.Name)
		}
		if err := notFoundWrap(weaviate.DeleteCollection(c, resolved), "collection", resolved); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "collection %q deleted\n", resolved)
	case manifest.KindRole:
		if err := notFoundWrap(weaviate.DeleteRole(c, doc.Name), "role", doc.Name); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "role %q deleted\n", doc.Name)
	case manifest.KindUser:
		if err := notFoundWrap(weaviate.DeleteUser(c, doc.Name), "user", doc.Name); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "user %q deleted\n", doc.Name)
	case manifest.KindAlias:
		resolved, err := weaviate.ResolveAlias(c, doc.Name)
		if err != nil {
			return notFoundWrap(err, "alias", doc.Name)
		}
		if err := notFoundWrap(weaviate.DeleteAlias(c, resolved), "alias", resolved); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "alias %q deleted\n", resolved)
	case manifest.KindTenants:
		td, err := doc.TenantsDoc()
		if err != nil {
			return err
		}
		names := make([]string, 0, len(td.Tenants))
		for _, t := range td.Tenants {
			names = append(names, str(t["name"]))
		}
		collection, err := weaviate.ResolveCollection(c, td.Collection)
		if err != nil {
			return notFoundWrap(err, "collection", td.Collection)
		}
		if err := notFoundWrap(weaviate.DeleteTenants(c, collection, names), "collection", collection); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "tenant %q deleted: %s\n", collection, join(names))
	case manifest.KindBackup:
		name := weaviate.NormalizeBackend(doc.Backend()) + "/" + doc.BackupID()
		if err := notFoundWrap(weaviate.CancelBackup(c, doc.Backend(), doc.BackupID(), weaviate.StatusQueryFromManifest(doc.Object)), "backup", name); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "backup %q canceled\n", name)
	case manifest.KindRestore:
		name := weaviate.NormalizeBackend(doc.Backend()) + "/" + doc.BackupID()
		if err := notFoundWrap(weaviate.CancelRestore(c, doc.Backend(), doc.BackupID(), weaviate.StatusQueryFromManifest(doc.Object)), "restore", name); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "restore %q canceled\n", name)
	default:
		return fmt.Errorf("cannot determine object kind for the manifest; add a \"kind\" field")
	}
	return nil
}
