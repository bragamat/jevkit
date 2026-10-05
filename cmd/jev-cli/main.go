// Command jev-cli is a Jev toolkit for coding agents.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/bragamat/jevkit/internal/cli"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	root := cli.NewRoot(cli.NewApp(), version)
	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		return
	}
	name := "jev-cli"
	if cmd != nil && cmd != root {
		name += " " + cmd.Name()
	}
	if errors.Is(err, context.Canceled) {
		err = errors.New("interrupted")
	}
	fmt.Fprintf(os.Stderr, "%s: %s\n", name, strings.TrimSpace(err.Error()))
	os.Exit(1)
}
