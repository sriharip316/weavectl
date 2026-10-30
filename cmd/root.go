// Package cmd wires the weavectl command tree.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/sriharip316/weavectl/internal/client"
	"github.com/sriharip316/weavectl/internal/config"
)

// Version is the weavectl client version, set at build time via -ldflags.
var Version = "dev"

var (
	flagServer   string
	flagAPIKey   string
	flagContext  string
	flagConfig   string
	flagNoConfig bool
)

func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "weavectl",
		Short: "Manage Weaviate collections, roles, users, aliases, tenants and backups",
		Long: `weavectl is a DBA control-plane tool for Weaviate databases, inspired
by kubectl. It manages schema-level objects (collections, roles, users,
aliases, tenants) and orchestrates backups and restores; apart from
backup and restore it never touches object data.

Configuration is resolved from flags, then WEAVECTL_SERVER /
WEAVECTL_API_KEY environment variables, then the named context in
~/.weavectl/config.yaml, and finally defaults to http://localhost:8080.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	pf := root.PersistentFlags()
	pf.StringVar(&flagServer, "server", "", "Weaviate base URL (default http://localhost:8080)")
	pf.StringVar(&flagAPIKey, "api-key", "", "API key or bearer token (default $WEAVECTL_API_KEY)")
	pf.StringVar(&flagContext, "context", "", "config file context to use")
	pf.StringVar(&flagConfig, "config", "", "path to config file (default ~/.weavectl/config.yaml)")
	pf.BoolVar(&flagNoConfig, "no-config", false, "ignore the config file entirely")

	root.AddCommand(
		newGetCommand(),
		newCreateCommand(),
		newApplyCommand(),
		newDeleteCommand(),
		newRotateCommand(),
		newRestoreCommand(),
		newVersionCommand(),
	)
	return root
}

// newClient resolves configuration and returns a ready API client.
func newClient() (*client.Client, error) {
	cfg, err := config.Load(config.Options{
		ServerFlag:   flagServer,
		APIKeyFlag:   flagAPIKey,
		ContextFlag:  flagContext,
		ConfigPath:   flagConfig,
		NoConfigFile: flagNoConfig,
	})
	if err != nil {
		return nil, err
	}
	return client.New(cfg.Server, cfg.APIKey), nil
}

// serverError is a translated server response (e.g. a 404 on a named
// object). The kubectl-style "Error from server (...)" rendering is
// applied at display time so the error string itself stays lowercase.
type serverError struct {
	reason string
	msg    string
}

func (e *serverError) Error() string { return e.msg }

func newServerError(reason, format string, args ...any) *serverError {
	return &serverError{reason: reason, msg: fmt.Sprintf(format, args...)}
}

// friendlyMessage renders an error kubectl-style for display.
func friendlyMessage(err error) string {
	if apiErr, ok := errors.AsType[*client.APIError](err); ok {
		return fmt.Sprintf("Error from server (%s): %v", apiErr.Reason(), err)
	}
	if se, ok := errors.AsType[*serverError](err); ok {
		return fmt.Sprintf("Error from server (%s): %s", se.reason, se.msg)
	}
	return fmt.Sprintf("Error: %v", err)
}

// resourceTypes maps every accepted alias to the canonical plural type.
var resourceTypes = map[string]string{
	"collection": "collections", "collections": "collections",
	"role": "roles", "roles": "roles",
	"user": "users", "users": "users",
	"alias": "aliases", "aliases": "aliases",
	"tenant": "tenants", "tenants": "tenants",
	"shard": "shards", "shards": "shards",
	"node": "nodes", "nodes": "nodes",
	"backup": "backups", "backups": "backups",
}

func resolveType(arg string) (string, error) {
	if t, ok := resourceTypes[strings.ToLower(arg)]; ok {
		return t, nil
	}
	return "", fmt.Errorf("unknown resource type %q (want one of: collections, roles, users, aliases, tenants, shards, nodes, backups)", arg)
}

func setupSignalHandler() (context.Context, func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	return setupSignalHandlerWith(sigCh, os.Exit)
}

func setupSignalHandlerWith(sigCh chan os.Signal, exit func(int)) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-done:
			return
		}
		select {
		case <-sigCh:
			exit(130)
		case <-done:
			return
		}
	}()

	cleanup := func() {
		signal.Stop(sigCh)
		close(done)
		cancel()
	}
	return ctx, cleanup
}

// Execute runs the root command with a signal-aware context: the first
// SIGINT/SIGTERM cancels the context (so context-aware work like
// `backup --wait` stops gracefully), the second one exits hard in case a
// command ignores cancellation.
func Execute() {
	ctx, cleanup := setupSignalHandler()
	defer cleanup()
	if err := NewRootCommand().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, friendlyMessage(err))
		os.Exit(1)
	}
}
