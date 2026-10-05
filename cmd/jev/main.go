// Command jev is a Jev toolkit for coding agents.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
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
	// A jev-claude or jev-codex symlink to this binary runs that launcher, as
	// the npm jev-gateway's commands of the same names did.
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if agent, ok := strings.CutPrefix(name, "jev-"); ok && (agent == "claude" || agent == "codex") {
		root.SetArgs(append([]string{agent}, os.Args[1:]...))
	}
	// jev --claude ARGS / jev --codex ARGS: everything after the flag belongs to
	// the agent, so it is dispatched before cobra parses any flags.
	if len(os.Args) > 1 && (os.Args[1] == "--claude" || os.Args[1] == "--codex") {
		root.SetArgs(append([]string{strings.TrimPrefix(os.Args[1], "--")}, os.Args[2:]...))
	}
	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		return 0
	}
	var exit cli.ExitError
	if errors.As(err, &exit) {
		return exit.Code
	}
	name = "jev"
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
