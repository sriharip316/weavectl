package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sriharip316/weavectl/internal/apply"
	"github.com/sriharip316/weavectl/internal/manifest"
)

func newApplyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply -f FILE",
		Short: "Create or update objects from a manifest file",
		Long: `Create or update objects to match a manifest file (\"-\" for stdin).

The file holds raw Weaviate definitions in JSON or YAML; multi-document
YAML (--- separated) applies each document in order. If an object does
not exist it is created; if it exists it is updated to match. A single
apply may issue several API calls, handled automatically:

  collection  one PUT for mutable settings, plus one POST per new
              property (existing properties are immutable except their
              description, which is updated in place)
  role        an add/remove permissions diff against the stored role
  user        create (printing the one-time API key) or converge role
              assignments
  alias       create, or retarget to the new collection
  tenants     create missing tenants and update activity status;
              tenants are never deleted implicitly`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			file, _ := cmd.Flags().GetString("filename")
			if file == "" {
				return fmt.Errorf("--filename/-f is required")
			}
			docs, err := manifest.Load(file)
			if err != nil {
				return err
			}
			c, err := newClient()
			if err != nil {
				return err
			}
			results, err := apply.Apply(c, docs)
			if len(results) > 0 {
				printResults(cmd, results)
			}
			if err != nil {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringP("filename", "f", "", "manifest file (\"-\" for stdin)")
	_ = cmd.MarkFlagRequired("filename")
	return cmd
}

// printResults renders apply/create outcomes kubectl-style.
func printResults(cmd *cobra.Command, results []apply.Result) {
	out := cmd.OutOrStdout()
	for _, r := range results {
		_, _ = fmt.Fprintf(out, "%s/%s %s\n", r.Kind, r.Name, r.Action)
		for _, info := range r.Info {
			_, _ = fmt.Fprintf(out, "  %s\n", info)
		}
	}
}
