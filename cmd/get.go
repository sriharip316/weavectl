package cmd

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sriharip316/weavectl/internal/client"
	"github.com/sriharip316/weavectl/internal/printer"
	"github.com/sriharip316/weavectl/internal/weaviate"
)

func newGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get TYPE [NAME]",
		Short: "Display one or many objects",
		Long: `Display one or many objects as a table, or as JSON/YAML with -o.

Types: collections, roles, users, aliases, tenants, shards, nodes,
backups. For collections, roles, users and aliases an optional NAME
narrows the result to a single object. tenants and shards take a
COLLECTION name instead; nodes lists cluster nodes; backups are listed
per backend with --backend.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := printer.ParseFormat(outputFlag(cmd))
			if err != nil {
				return err
			}
			typ, err := resolveType(args[0])
			if err != nil {
				return err
			}
			c, err := newClient()
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 2 {
				name = args[1]
			}
			return runGet(cmd, c, typ, name, format)
		},
	}
	cmd.Flags().StringP("output", "o", "table", "output format: table, json or yaml")
	cmd.Flags().String("backend", "", "backup backend (required for the backups type)")
	return cmd
}

func outputFlag(cmd *cobra.Command) string {
	v, _ := cmd.Flags().GetString("output")
	return v
}

func runGet(cmd *cobra.Command, c *client.Client, typ, name string, format printer.Format) error {
	out := cmd.OutOrStdout()
	single := name != ""

	switch typ {
	case "collections":
		if single {
			resolved, err := weaviate.ResolveCollection(c, name)
			if err != nil {
				return notFoundWrap(err, "collection", name)
			}
			obj, err := weaviate.GetCollection(c, resolved)
			if err != nil {
				return notFoundWrap(err, "collection", name)
			}
			return printObjects(out, format, true, []any{obj}, collectionHeaders, collectionRow(obj))
		}
		list, err := weaviate.ListCollections(c)
		if err != nil {
			return err
		}
		sort.Slice(list, func(i, j int) bool {
			return fmt.Sprint(list[i]["class"]) < fmt.Sprint(list[j]["class"])
		})
		items := make([]any, len(list))
		rows := make([][]string, len(list))
		for i, obj := range list {
			items[i] = obj
			rows[i] = collectionRow(obj)
		}
		return printObjects(out, format, false, items, collectionHeaders, rows...)

	case "roles":
		if single {
			obj, err := weaviate.GetRole(c, name)
			if err != nil {
				return notFoundWrap(err, "role", name)
			}
			return printObjects(out, format, true, []any{obj}, roleHeaders, roleRow(*obj))
		}
		list, err := weaviate.ListRoles(c)
		if err != nil {
			return err
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		items := make([]any, len(list))
		rows := make([][]string, len(list))
		for i, obj := range list {
			items[i] = obj
			rows[i] = roleRow(obj)
		}
		return printObjects(out, format, false, items, roleHeaders, rows...)

	case "users":
		if single {
			obj, err := weaviate.GetUser(c, name)
			if err != nil {
				return notFoundWrap(err, "user", name)
			}
			return printObjects(out, format, true, []any{obj}, userHeaders, userRow(*obj))
		}
		list, err := weaviate.ListUsers(c)
		if err != nil {
			return err
		}
		sort.Slice(list, func(i, j int) bool { return list[i].UserID < list[j].UserID })
		items := make([]any, len(list))
		rows := make([][]string, len(list))
		for i, obj := range list {
			items[i] = obj
			rows[i] = userRow(obj)
		}
		return printObjects(out, format, false, items, userHeaders, rows...)

	case "aliases":
		list, err := weaviate.ListAliases(c)
		if err != nil {
			return err
		}
		if single {
			resolved, err := weaviate.ResolveAlias(c, name)
			if err != nil {
				return notFoundWrap(err, "alias", name)
			}
			filtered := list[:0]
			for _, a := range list {
				if a.Alias == resolved {
					filtered = append(filtered, a)
				}
			}
			list = filtered
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Alias < list[j].Alias })
		items := make([]any, len(list))
		rows := make([][]string, len(list))
		for i, obj := range list {
			items[i] = obj
			rows[i] = aliasRow(obj)
		}
		return printObjects(out, format, single, items, aliasHeaders, rows...)

	case "tenants":
		if !single {
			return fmt.Errorf("get tenants requires a collection name (weavectl get tenants COLLECTION)")
		}
		resolved, err := weaviate.ResolveCollection(c, name)
		if err != nil {
			return notFoundWrap(err, "collection", name)
		}
		list, err := weaviate.ListTenants(c, resolved)
		if err != nil {
			return err
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		items := make([]any, len(list))
		rows := make([][]string, len(list))
		for i, t := range list {
			items[i] = t
			rows[i] = []string{t.Name, t.ActivityStatus}
		}
		return printObjects(out, format, false, items, []string{"NAME", "STATUS"}, rows...)

	case "shards":
		if !single {
			return fmt.Errorf("get shards requires a collection name (weavectl get shards COLLECTION)")
		}
		resolved, err := weaviate.ResolveCollection(c, name)
		if err != nil {
			return notFoundWrap(err, "collection", name)
		}
		list, err := weaviate.ListShards(c, resolved)
		if err != nil {
			return err
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		items := make([]any, len(list))
		rows := make([][]string, len(list))
		for i, s := range list {
			items[i] = s
			rows[i] = []string{s.Name, s.Status, strconv.Itoa(s.VectorQueueSize)}
		}
		return printObjects(out, format, false, items, []string{"NAME", "STATUS", "VECTOR QUEUE"}, rows...)

	case "nodes":
		if single {
			return fmt.Errorf("get nodes takes no name argument")
		}
		list, err := weaviate.ListNodes(c)
		if err != nil {
			return err
		}
		items := make([]any, len(list))
		rows := make([][]string, len(list))
		for i, n := range list {
			items[i] = n
			shardCount := "-"
			if shards, ok := n.Shards.([]any); ok {
				shardCount = strconv.Itoa(len(shards))
			}
			rows[i] = []string{n.Name, n.Status, n.Version, n.OperationalMode, shardCount}
		}
		return printObjects(out, format, false, items, []string{"NAME", "STATUS", "VERSION", "MODE", "SHARDS"}, rows...)

	case "backups":
		backend, _ := cmd.Flags().GetString("backend")
		if backend == "" {
			return fmt.Errorf("get backups requires a backend (weavectl get backups --backend filesystem [ID])")
		}
		backend = weaviate.NormalizeBackend(backend)
		if single {
			st, err := weaviate.GetBackupStatus(c, backend, name, weaviate.StatusQuery{})
			if err != nil {
				return notFoundWrap(err, "backup", backend+"/"+name)
			}
			return printObjects(out, format, true, []any{st}, backupStatusHeaders, backupStatusRow(*st))
		}
		list, err := weaviate.ListBackups(c, backend)
		if err != nil {
			return err
		}
		items := make([]any, len(list))
		rows := make([][]string, len(list))
		for i, e := range list {
			// Structured output is manifest-shaped so exported YAML
			// round-trips through apply.
			items[i] = backupManifest(backend, e)
			rows[i] = backupRow(e)
		}
		return printObjects(out, format, false, items, backupHeaders, rows...)
	}
	return fmt.Errorf("unsupported type %q", typ)
}

func notFoundWrap(err error, typ, name string) error {
	if client.IsNotFound(err) {
		return newServerError("NotFound", "%s %q not found", typ, name)
	}
	return err
}

// printObjects renders either the table or the structured formats. single
// marks a named lookup versus a list query: JSON list output is always an
// array, a named lookup prints as a bare object.
func printObjects(out interface{ Write([]byte) (int, error) }, format printer.Format, single bool,
	items []any, headers []string, rows ...[]string) error {
	switch format {
	case printer.FormatTable:
		printer.PrintTable(out, headers, rows)
		return nil
	default:
		if len(items) == 0 {
			return nil // like kubectl: structured formats print nothing when empty
		}
		if single {
			return printer.PrintItem(out, format, items[0])
		}
		return printer.Print(out, format, items)
	}
}

// ---- table column definitions ----

var (
	collectionHeaders = []string{"NAME", "VECTORIZER", "REPLICATION", "SHARDS", "PROPERTIES", "MULTI-TENANT"}
	roleHeaders       = []string{"NAME", "PERMISSIONS"}
	userHeaders       = []string{"USER ID", "TYPE", "ACTIVE", "ROLES"}
	aliasHeaders      = []string{"ALIAS", "COLLECTION"}
)

func collectionRow(obj map[string]any) []string {
	props := 0
	if raw, ok := obj["properties"].([]any); ok {
		props = len(raw)
	}
	return []string{
		str(obj["class"]),
		strOr(obj["vectorizer"], "-"),
		fmt.Sprint(nested(obj, "replicationConfig", "factor", "1")),
		fmt.Sprint(nested(obj, "shardingConfig", "desiredCount", "1")),
		strconv.Itoa(props),
		boolStr(nested(obj, "multiTenancyConfig", "enabled", false)),
	}
}

func roleRow(r weaviate.Role) []string {
	return []string{r.Name, strconv.Itoa(len(r.Permissions))}
}

func userRow(u weaviate.DBUser) []string {
	roles := "-"
	if len(u.Roles) > 0 {
		roles = join(u.Roles)
	}
	return []string{u.UserID, u.DBUserType, boolStr(u.Active), roles}
}

func aliasRow(a weaviate.Alias) []string {
	return []string{a.Alias, a.Class}
}

// ---- small map-plumbing helpers ----

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strOr(v any, fallback string) string {
	if s := str(v); s != "" {
		return s
	}
	return fallback
}

func join(xs []string) string {
	var out strings.Builder
	for i, x := range xs {
		if i > 0 {
			out.WriteString(",")
		}
		out.WriteString(x)
	}
	return out.String()
}

func boolStr(v any) string {
	if b, ok := v.(bool); ok {
		if b {
			return "true"
		}
		return "false"
	}
	return "false"
}

// nested walks obj[a][b] and returns its value, or fallback.
func nested(obj map[string]any, a, b string, fallback any) any {
	sub, ok := obj[a].(map[string]any)
	if !ok {
		return fallback
	}
	v, ok := sub[b]
	if !ok || v == nil {
		return fallback
	}
	return v
}
