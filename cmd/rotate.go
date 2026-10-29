package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sriharip316/weavectl/internal/weaviate"
)

// newRotateCommand performs credential-lifecycle actions that replace a
// secret in place, as opposed to create (new object) or apply (converge).
func newRotateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Rotate a secret, replacing the old one (see subcommands)",
	}
	cmd.AddCommand(newRotateUserCommand())
	return cmd
}

// rotate user NAME: the old key is revoked immediately; the new API key is
// displayed exactly once.
func newRotateUserCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "user NAME",
		Short: "Rotate a db user's API key (revokes the old key, shows the new one once)",
		Long: `Revokes the user's current API key and generates a new one.

The old key stops working immediately — update every consumer of it. The
new key is printed exactly once, like create user; store it now.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			apiKey, err := weaviate.RotateUserKey(c, args[0])
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "user/%s rotated\n", args[0])
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "API key (shown once, store it now): %s\n", apiKey)
			return nil
		},
	}
}
