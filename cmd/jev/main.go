// Command jev is a Jev toolkit for coding agents.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/bragamat/jevkit/internal/cli"
)

// version, commit and date are set at release time with -ldflags "-X main.version=...".
var (
	version = ""
	commit  = ""
	date    = ""
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	root := cli.NewRoot(cli.NewApp(), versionString())
	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		return 0
	}
	name := "jev"
	if cmd != nil && cmd != root {
		name += " " + cmd.Name()
	}
	code := 1
	switch {
	case errors.Is(err, context.Canceled):
		err, code = errors.New("interrupted"), 130
	case cli.IsUsageError(err):
		code = 2
	}
	fmt.Fprintf(os.Stderr, "%s: %s\n", name, strings.TrimSpace(err.Error()))
	return code
}

// versionString prefers the release ldflags and falls back to the module version
// that `go install ...@vX.Y.Z` records in the binary.
func versionString() string {
	v := version
	if v == "" {
		v = "dev"
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = strings.TrimPrefix(info.Main.Version, "v")
		}
	}
	if commit != "" {
		v += " (" + commit
		if date != "" {
			v += ", " + date
		}
		v += ")"
	}
	return v
}
