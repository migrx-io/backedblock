// Command bbctl deploys and operates backedblock.io storage clusters on AWS.
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/migrx-io/backedblock/internal/cli"
	"github.com/migrx-io/backedblock/internal/ui"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := cli.NewRoot().ExecuteContext(ctx)
	if err == nil {
		return
	}
	// Match bbctl's own type only: wrapped errors from terraform carry an
	// *exec.ExitError, which also has ExitCode() and must still be reported.
	var silent *cli.SilentExit
	if errors.As(err, &silent) {
		os.Exit(silent.ExitCode())
	}
	os.Exit(ui.PrintError(err))
}
