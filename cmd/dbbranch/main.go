package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"dbbranch/internal/lifecycle"
	"dbbranch/internal/provider/postgres"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "dbbranch:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Fprintln(out, "Usage: dbbranch <create|status|connection|destroy|gc> [flags]")
		fmt.Fprintln(out, "Set DBBRANCH_ADMIN_URL to a direct management database URL. See <command> --help.")
		return nil
	}
	cmd := args[0]
	flags := flag.NewFlagSet(cmd, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var scope lifecycle.Scope
	flags.Int64Var(&scope.RepoID, "repo-id", 0, "numeric repository ID")
	flags.StringVar(&scope.Env, "env", "preview", "preview environment name")
	if cmd != "gc" {
		flags.Int64Var(&scope.PR, "pr", 0, "pull request number (omit on status to list all)")
	}
	jsonOutput := flags.Bool("json", false, "write JSON output")
	timeout := flags.Duration("timeout", 15*time.Minute, "operation timeout")
	var id, revision, baseline, attempt, archive, path string
	var olderThan time.Duration
	var dryRun bool
	switch cmd {
	case "create":
		flags.StringVar(&revision, "revision", "", "candidate merge revision")
		flags.StringVar(&baseline, "baseline", "", "base revision")
		flags.StringVar(&attempt, "attempt", "default", "idempotency token for this deployment attempt")
		flags.StringVar(&archive, "archive", "", "trusted pg_dump custom archive")
	case "status":
	case "connection":
		flags.StringVar(&id, "id", "", "managed generation ID")
		flags.StringVar(&path, "file", "", "new mode-0600 file to receive the connection URL")
	case "destroy":
		flags.StringVar(&id, "id", "", "managed generation ID (omit to delete all generations of this PR)")
	case "gc":
		flags.DurationVar(&olderThan, "older-than", 24*time.Hour, "age threshold for unused generations")
		flags.BoolVar(&dryRun, "dry-run", false, "list candidates without removing resources")
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *timeout <= 0 {
		return errors.New("use named flags and a positive --timeout")
	}
	if cmd == "status" || cmd == "gc" {
		if scope.RepoID <= 0 || scope.PR < 0 || !lifecycle.ValidToken(scope.Env) {
			return errors.New("positive --repo-id and valid --env are required")
		}
	} else if err := scope.Validate(); err != nil {
		return err
	}
	switch cmd {
	case "create":
		if !lifecycle.ValidToken(revision) || !lifecycle.ValidToken(baseline) || !lifecycle.ValidToken(attempt) || archive == "" {
			return errors.New("--revision, --baseline, --archive and a valid --attempt are required")
		}
	case "connection":
		if !lifecycle.ValidID(id) || path == "" {
			return errors.New("connection requires --id and --file")
		}
	case "destroy":
		if id != "" && !lifecycle.ValidID(id) {
			return errors.New("invalid --id")
		}
	case "gc":
		if olderThan <= 0 {
			return errors.New("--older-than must be a positive duration")
		}
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	store, err := postgres.Open(ctx, os.Getenv("DBBRANCH_ADMIN_URL"))
	if err != nil {
		return err
	}
	defer store.Close()

	switch cmd {
	case "create":
		g, err := store.Create(ctx, scope, revision, baseline, attempt, archive)
		if err != nil {
			return err
		}
		if *jsonOutput {
			return json.NewEncoder(out).Encode(g)
		}
		return printGenerations(out, []lifecycle.Generation{g})
	case "status":
		list, err := store.Status(ctx, scope.RepoID, scope.Env, scope.PR)
		if err != nil {
			return err
		}
		if *jsonOutput {
			return json.NewEncoder(out).Encode(list)
		}
		return printGenerations(out, list)
	case "connection":
		if err := writeConnection(ctx, store, scope, id, path); err != nil {
			return err
		}
	case "destroy":
		if err := store.Destroy(ctx, scope, id); err != nil {
			return err
		}
	case "gc":
		list, err := store.GC(ctx, scope.RepoID, scope.Env, time.Now().Add(-olderThan), dryRun)
		if *jsonOutput {
			return errors.Join(err, json.NewEncoder(out).Encode(list))
		}
		return errors.Join(err, printGenerations(out, list))
	}
	if *jsonOutput {
		return json.NewEncoder(out).Encode(struct {
			OK bool `json:"ok"`
		}{true})
	}
	return nil
}

func printGenerations(out io.Writer, list []lifecycle.Generation) error {
	for _, g := range list {
		if _, err := fmt.Fprintf(out, "%s\t%s\tPR %d\t%s\n", g.ID, g.Database, g.PR, g.State); err != nil {
			return err
		}
	}
	return nil
}

func writeConnection(ctx context.Context, store *postgres.Store, scope lifecycle.Scope, id, path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		file.Close()
		if !complete {
			os.Remove(path)
		}
	}()
	connection, err := store.Connection(ctx, scope, id)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(file, connection); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}
