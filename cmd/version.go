package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sriharip316/weavectl/internal/weaviate"
)

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the weavectl and Weaviate server versions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "weavectl %s\n", Version)
			c, err := newClient()
			if err != nil {
				return err
			}
			meta, err := weaviate.GetMeta(c)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "Weaviate %s at %s\n", meta.Version, meta.Hostname)
			return nil
		},
	}
}
