package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/nightio/pg-pull/internal/app"
	"github.com/nightio/pg-pull/internal/cli"
	"github.com/nightio/pg-pull/internal/update"
)

var (
	version = "dev"
	commit  = "unknown"
	builtAt = "unknown"
)

func main() {
	if handled, code := update.FinishIfRequested(os.Args[1:]); handled {
		os.Exit(code)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ui := cli.New(ctx, os.Stdin, os.Stdout, os.Stderr)
	application := &app.App{UI: ui, Version: fmt.Sprintf("%s (commit %s, built %s)", version, commit, builtAt), RawVersion: version}
	os.Exit(application.Run(ctx, os.Args[1:]))
}
