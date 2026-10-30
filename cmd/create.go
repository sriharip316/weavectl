package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sriharip316/weavectl/internal/apply"
	"github.com/sriharip316/weavectl/internal/client"
	"github.com/sriharip316/weavectl/internal/manifest"
	"github.com/sriharip316/weavectl/internal/weaviate"
)

func newCreateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create an object from a file (-f), or a simple one from subcommands",
		Long: `Create objects from a manifest file (-f FILE, "-" for stdin), or use
a subcommand for simple objects:

  weavectl create collection NAME [--replication-factor N] [--vectorizer MODULE]
                                  [--shard-count N] [--multi-tenancy]
  weavectl create user NAME
  weavectl create alias NAME --collection CLASS
  weavectl create tenant COLLECTION NAME...
  weavectl create backup BACKEND ID [--include A,B | --exclude C] [--wait]

The file holds raw Weaviate definitions in JSON or YAML (multi-document
YAML allowed); the object kind is detected from the fields, or taken from
an explicit "kind" key. Unlike apply, create fails when the object
already exists.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			file, _ := cmd.Flags().GetString("filename")
			if file == "" {
				return fmt.Errorf("specify -f FILE or a subcommand (see weavectl create --help)")
			}
			docs, err := manifest.Load(file)
			if err != nil {
				return err
			}
			c, err := newClient()
			if err != nil {
				return err
			}
			return createFromDocs(cmd, c, docs)
		},
	}
	cmd.Flags().StringP("filename", "f", "", "manifest file (\"-\" for stdin)")
	cmd.AddCommand(
		newCreateCollectionCommand(),
		newCreateUserCommand(),
		newCreateAliasCommand(),
		newCreateTenantCommand(),
		newCreateBackupCommand(),
	)
	return cmd
}

func createFromDocs(cmd *cobra.Command, c *client.Client, docs []manifest.Document) error {
	declaredCollections := make(map[string]bool)
	seen := make(map[string]bool)
	seenTenants := make(map[string]bool)

	for _, doc := range docs {
		if doc.Kind == manifest.KindUnknown {
			return fmt.Errorf("cannot determine object kind for the manifest; add a \"kind\" field")
		}

		if doc.Kind == manifest.KindTenants {
			td, err := doc.TenantsDoc()
			if err != nil {
				return err
			}
			col := weaviate.CapitalizeName(td.Collection)
			for _, want := range td.Tenants {
				name, _ := want["name"].(string)
				if name != "" {
					tKey := col + "/" + name
					if seenTenants[tKey] {
						return newServerError("AlreadyExists", "%s %q already exists", doc.Kind, doc.Name)
					}
					seenTenants[tKey] = true
				}
			}
		} else {
			key := docIdentity(doc)
			if key != "" {
				if seen[key] {
					return newServerError("AlreadyExists", "%s %q already exists", doc.Kind, doc.Name)
				}
				seen[key] = true
			}
		}

		if err := ensureAbsent(c, doc, declaredCollections); err != nil {
			return err
		}

		if doc.Kind == manifest.KindCollection || doc.Kind == manifest.KindAlias {
			declaredCollections[weaviate.CapitalizeName(doc.Name)] = true
		}
	}
	results, err := apply.Apply(c, docs)
	if err != nil {
		return err
	}
	printResults(cmd, results)
	return nil
}

func docIdentity(doc manifest.Document) string {
	switch doc.Kind {
	case manifest.KindCollection:
		return fmt.Sprintf("collection/%s", weaviate.CapitalizeName(doc.Name))
	case manifest.KindRole:
		return fmt.Sprintf("role/%s", doc.Name)
	case manifest.KindUser:
		return fmt.Sprintf("user/%s", doc.Name)
	case manifest.KindAlias:
		return fmt.Sprintf("alias/%s", weaviate.CapitalizeName(doc.Name))
	case manifest.KindBackup:
		return fmt.Sprintf("backup/%s/%s", doc.Backend(), doc.BackupID())
	case manifest.KindRestore:
		return fmt.Sprintf("restore/%s/%s", doc.Backend(), doc.BackupID())
	default:
		return ""
	}
}

// ensureAbsent turns apply's upsert into create's insert-only semantics.
func ensureAbsent(c *client.Client, doc manifest.Document, declaredCollections map[string]bool) error {
	exists, err := objectExists(c, doc, declaredCollections)
	if err != nil {
		return err
	}
	if exists {
		return newServerError("AlreadyExists", "%s %q already exists", doc.Kind, doc.Name)
	}
	return nil
}

func objectExists(c *client.Client, doc manifest.Document, declaredCollections map[string]bool) (bool, error) {
	switch doc.Kind {
	case manifest.KindCollection:
		_, err := weaviate.ResolveCollection(c, doc.Name)
		return !client.IsNotFound(err), errUnlessNotFound(err)
	case manifest.KindRole:
		_, err := weaviate.GetRole(c, doc.Name)
		return !client.IsNotFound(err), errUnlessNotFound(err)
	case manifest.KindUser:
		_, err := weaviate.GetUser(c, doc.Name)
		return !client.IsNotFound(err), errUnlessNotFound(err)
	case manifest.KindAlias:
		_, err := weaviate.ResolveAlias(c, doc.Name)
		return !client.IsNotFound(err), errUnlessNotFound(err)
	case manifest.KindTenants:
		td, err := doc.TenantsDoc()
		if err != nil {
			return false, err
		}
		colName := weaviate.CapitalizeName(td.Collection)
		if declaredCollections != nil && declaredCollections[colName] {
			return false, nil
		}
		collection, err := weaviate.ResolveCollection(c, td.Collection)
		if err != nil {
			return false, err
		}
		current, err := weaviate.ListTenants(c, collection)
		if err != nil {
			return false, err
		}
		have := map[string]bool{}
		for _, t := range current {
			have[t.Name] = true
		}
		for _, want := range td.Tenants {
			if n, _ := want["name"].(string); have[n] {
				return true, nil
			}
		}
		return false, nil
	case manifest.KindBackup:
		_, err := weaviate.GetBackupStatus(context.Background(), c, doc.Backend(), doc.BackupID(), weaviate.StatusQuery{})
		return !client.IsNotFound(err), errUnlessNotFound(err)
	case manifest.KindRestore:
		_, err := weaviate.GetRestoreStatus(context.Background(), c, doc.Backend(), doc.BackupID(), weaviate.StatusQuery{})
		return !client.IsNotFound(err), errUnlessNotFound(err)
	}
	return false, fmt.Errorf("unsupported kind %q", doc.Kind)
}

func errUnlessNotFound(err error) error {
	if client.IsNotFound(err) {
		return nil
	}
	return err
}

// create collection NAME with default settings plus quick flags.
func newCreateCollectionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collection NAME",
		Short: "Create a collection with default settings",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := weaviate.CapitalizeName(args[0])

			body := map[string]any{"class": name}
			if v, _ := cmd.Flags().GetString("vectorizer"); v != "" {
				body["vectorizer"] = v
			}
			if rf, _ := cmd.Flags().GetInt("replication-factor"); rf > 0 {
				body["replicationConfig"] = map[string]any{"factor": rf}
			}
			if sc, _ := cmd.Flags().GetInt("shard-count"); sc > 0 {
				body["shardingConfig"] = map[string]any{"desiredCount": sc}
			}
			if mt, _ := cmd.Flags().GetBool("multi-tenancy"); mt {
				body["multiTenancyConfig"] = map[string]any{"enabled": true}
			}

			c, err := newClient()
			if err != nil {
				return err
			}
			if _, err := weaviate.CreateCollection(c, body); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "collection/%s created\n", name)
			return nil
		},
	}
	cmd.Flags().String("vectorizer", "", "vectorizer module (default: server default)")
	cmd.Flags().Int("replication-factor", 0, "replication factor (default 1)")
	cmd.Flags().Int("shard-count", 0, "desired shard count (default 1)")
	cmd.Flags().Bool("multi-tenancy", false, "enable multi-tenancy")
	return cmd
}

// create user NAME: the API key is displayed exactly once.
func newCreateUserCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "user NAME",
		Short: "Create a db user and display its API key (shown once)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			apiKey, err := weaviate.CreateUser(c, args[0])
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "user/%s created\n", args[0])
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "API key (shown once, store it now): %s\n", apiKey)
			return nil
		},
	}
}

func newCreateAliasCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "alias NAME --collection CLASS",
		Short: "Create an alias pointing at a collection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, _ := cmd.Flags().GetString("collection")
			if target == "" {
				return fmt.Errorf("--collection is required")
			}
			c, err := newClient()
			if err != nil {
				return err
			}
			name := weaviate.CapitalizeName(args[0])
			if err := weaviate.CreateAlias(c, weaviate.Alias{
				Alias: name, Class: weaviate.CapitalizeName(target),
			}); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "alias/%s created\n", name)
			return nil
		},
	}
	cmd.Flags().String("collection", "", "target collection (required)")
	_ = cmd.MarkFlagRequired("collection")
	return cmd
}

func newCreateTenantCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tenant COLLECTION NAME...",
		Short: "Create tenants in a multi-tenant collection",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			status, _ := cmd.Flags().GetString("activity-status")
			c, err := newClient()
			if err != nil {
				return err
			}
			collection, err := weaviate.ResolveCollection(c, args[0])
			if err != nil {
				return notFoundWrap(err, "collection", args[0])
			}
			tenants := make([]weaviate.Tenant, 0, len(args)-1)
			for _, n := range args[1:] {
				tenants = append(tenants, weaviate.Tenant{Name: n, ActivityStatus: status})
			}
			if err := weaviate.CreateTenants(c, collection, tenants); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "tenant/%s created: %s\n", collection, join(args[1:]))
			return nil
		},
	}
	cmd.Flags().String("activity-status", "ACTIVE", "initial activity status")
	return cmd
}
