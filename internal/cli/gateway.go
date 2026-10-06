package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/bragamat/jevkit/internal/dashboard"
	"github.com/bragamat/jevkit/internal/gateway"
)

// ExitError carries a child process's exit status through to main unchanged.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return "exit status " + strconv.Itoa(e.Code) }

func (a *App) gatewayCommands() []*cobra.Command {
	gw := &cobra.Command{
		Use:   "gateway",
		Short: "Run the routing gateway for Claude Code and Codex, with a live dashboard",
		Long: `The gateway sits between a coding agent and its LLM. For each request it asks Jev which tool
comes next and steers the request toward it: a hint for Claude Code (keeps the prompt cache and
extended thinking), a forced tool_choice for Codex. If Jev is slow, unsure or unreachable, the
request goes through unchanged.

Claude Code listens on 127.0.0.1:8789, Codex on 127.0.0.1:8790; both serve /dashboard.
Start agents through it with "jev --claude [ARGS]" and "jev --codex [ARGS]".`,
		GroupID: "gateway",
	}
	run := &cobra.Command{
		Use:   "run",
		Short: "Run the gateway in the foreground",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  func(cmd *cobra.Command, _ []string) error { return a.runGateway(cmd.Context()) },
	}
	start := &cobra.Command{
		Use:   "start",
		Short: "Start the gateway in the background, unless it is already up",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  func(cmd *cobra.Command, _ []string) error { return a.startGateway(cmd.Context()) },
	}
	stop := &cobra.Command{
		Use:   "stop",
		Short: "Stop the background gateway",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  func(cmd *cobra.Command, _ []string) error { return a.stopGateway(cmd.Context()) },
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether each gateway port answers",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  func(cmd *cobra.Command, _ []string) error { return a.gatewayStatus(cmd.Context()) },
	}
	gw.AddCommand(run, start, stop, status)

	agent := func(name, short string) *cobra.Command {
		return &cobra.Command{
			Use:                name + " [ARGS...]",
			Short:              short,
			Hidden:             true, // reached through jev --claude / --codex
			DisableFlagParsing: true,
			RunE:               func(cmd *cobra.Command, args []string) error { return a.runAgent(cmd.Context(), name, args) },
		}
	}
	return []*cobra.Command{
		gw,
		agent("claude", "Start Claude Code through the gateway (arguments go to claude)"),
		agent("codex", "Start Codex through the gateway (arguments go to codex)"),
	}
}

func pidFile() string { return filepath.Join(stateDir(), "gateway.pid") }

func (a *App) runGateway(ctx context.Context) error {
	cfg := gateway.FromEnv(stateDir())
	client, err := a.getClient()
	if err != nil {
		return err
	}
	events, err := gateway.OpenEvents(cfg.EventLog)
	if err != nil {
		return err
	}
	defer func() { _ = events.Close() }()
	gw := gateway.New(cfg, client, a.model, events)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	dash := dashboard.New(ctx, gw)

	// Bind every port before serving any, so a busy port fails the whole start.
	var servers []*http.Server
	var listeners []net.Listener
	for _, l := range cfg.Listeners {
		ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(l.Port)))
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}
			return err
		}
		mux := http.NewServeMux()
		mux.Handle("/dashboard", dash)
		mux.Handle("/dashboard/", dash)
		mux.Handle("/", gw.Handler(l))
		listeners = append(listeners, ln)
		servers = append(servers, &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second})
		_, _ = fmt.Fprintf(a.Stdout, "jev gateway: %s on %s → %s (dashboard %s/dashboard)\n",
			l.Client, ln.Addr(), l.Upstream, localURL(cfg.Host, l.Port))
	}
	if err := os.WriteFile(pidFile(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "jev gateway: could not write %s: %v\n", pidFile(), err)
	}
	defer removeOwnPidFile()

	g, gctx := errgroup.WithContext(ctx)
	for i := range servers {
		srv, ln := servers[i], listeners[i]
		g.Go(func() error {
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
	}
	g.Go(func() error {
		<-gctx.Done()
		// Streams can run for minutes; give them a moment, then cut them.
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		for _, srv := range servers {
			if srv.Shutdown(sctx) != nil {
				_ = srv.Close()
			}
		}
		return nil
	})
	// A signal ends the servers with ErrServerClosed: a clean exit.
	return g.Wait()
}

func removeOwnPidFile() {
	b, err := os.ReadFile(pidFile())
	if err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(os.Getpid()) {
		_ = os.Remove(pidFile())
	}
}

func localURL(host string, port int) string {
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port))
}

type health struct {
	Status   string `json:"status"`
	PID      int    `json:"pid"`
	Client   string `json:"client"`
	Upstream string `json:"upstream"`
	Routing  *bool  `json:"routing"`
}

// probe asks one port for /health. Any gateway that answers counts, including
// the npm jev-gateway this one replaces.
func probe(ctx context.Context, base string) (*health, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("health returned %s", resp.Status)
	}
	var h health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return nil, err
	}
	return &h, nil
}

func (a *App) allUp(ctx context.Context, cfg gateway.Config) bool {
	for _, l := range cfg.Listeners {
		if _, err := probe(ctx, localURL(cfg.Host, l.Port)); err != nil {
			return false
		}
	}
	return true
}

func (a *App) startGateway(ctx context.Context) error {
	cfg := gateway.FromEnv(stateDir())
	if a.allUp(ctx, cfg) {
		return a.gatewayStatus(ctx)
	}
	if _, err := a.getClient(); err != nil {
		return err // fail here, not silently in the background
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	logPath := filepath.Join(stateDir(), "gateway.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	args := []string{"gateway", "run"}
	if a.model != "" {
		args = append(args, "--model", a.model)
	}
	// WithoutCancel: the detached gateway must outlive this command.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), self, args...) //nolint:gosec // re-runs this same binary
	cmd.Stdout, cmd.Stderr = logFile, logFile
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return fmt.Errorf("the gateway exited at start (%w); see %s", err, logPath)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		if a.allUp(ctx, cfg) {
			_ = cmd.Process.Release()
			return a.gatewayStatus(ctx)
		}
	}
	return fmt.Errorf("the gateway did not answer within 10s; see %s", logPath)
}

func (a *App) stopGateway(ctx context.Context) error {
	b, err := os.ReadFile(pidFile())
	if errors.Is(err, os.ErrNotExist) {
		return inputError{"no background gateway (no " + pidFile() + ")"}
	}
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return fmt.Errorf("%s holds no pid", pidFile())
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := terminate(p); err != nil {
		_ = os.Remove(pidFile())
		return fmt.Errorf("gateway pid %d is not running: %w", pid, err)
	}
	cfg := gateway.FromEnv(stateDir())
	for range 50 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		time.Sleep(200 * time.Millisecond)
		if !a.anyUp(ctx, cfg, pid) {
			_, _ = fmt.Fprintf(a.Stdout, "stopped gateway pid %d\n", pid)
			return nil
		}
	}
	return fmt.Errorf("gateway pid %d is still answering after 10s", pid)
}

// anyUp reports whether a port still answers from pid.
func (a *App) anyUp(ctx context.Context, cfg gateway.Config, pid int) bool {
	for _, l := range cfg.Listeners {
		if h, err := probe(ctx, localURL(cfg.Host, l.Port)); err == nil && h.PID == pid {
			return true
		}
	}
	return false
}

func (a *App) gatewayStatus(ctx context.Context) error {
	cfg := gateway.FromEnv(stateDir())
	down := 0
	for _, l := range cfg.Listeners {
		base := localURL(cfg.Host, l.Port)
		h, err := probe(ctx, base)
		if err != nil {
			down++
			_, _ = fmt.Fprintf(a.Stdout, "%-6s %s  down\n", l.Client, base)
			continue
		}
		routing := ""
		if h.Routing != nil {
			routing = "  routing " + map[bool]string{true: "on", false: "off"}[*h.Routing]
		}
		_, _ = fmt.Fprintf(a.Stdout, "%-6s %s  up  pid %d  → %s%s\n", l.Client, base, h.PID, h.Upstream, routing)
	}
	if down == 0 {
		_, _ = fmt.Fprintf(a.Stdout, "dashboard %s/dashboard\n", localURL(cfg.Host, cfg.Listeners[0].Port))
		return nil
	}
	return errors.New("gateway not running; start it with: jev gateway start")
}

// runAgent makes sure the gateway is up, then runs the agent with its base
// URL pointed at the gateway and returns the agent's exit status.
func (a *App) runAgent(ctx context.Context, agent string, args []string) error {
	// The npm jev-claude / jev-codex launchers took these flags; keep them working.
	if len(args) == 1 {
		switch args[0] {
		case "--start":
			return a.startGateway(ctx)
		case "--stop":
			return a.stopGateway(ctx)
		case "--status":
			return a.gatewayStatus(ctx)
		case "--dashboard":
			cfg := gateway.FromEnv(stateDir())
			_, _ = fmt.Fprintf(a.Stdout, "%s/dashboard\n", localURL(cfg.Host, cfg.Listeners[0].Port))
			return nil
		}
	}
	cfg := gateway.FromEnv(stateDir())
	var l gateway.Listener
	for _, x := range cfg.Listeners {
		if x.Client == agent {
			l = x
		}
	}
	base := localURL(cfg.Host, l.Port)
	if _, err := probe(ctx, base); err != nil {
		out := a.Stdout
		a.Stdout = os.Stderr // keep the agent's stdout clean, e.g. for claude -p
		err := a.startGateway(ctx)
		a.Stdout = out
		if err != nil {
			return err
		}
	}
	bin, err := exec.LookPath(agent)
	if err != nil {
		return inputError{agent + " is not installed or not on PATH"}
	}
	// WithoutCancel: Ctrl-C belongs to the agent (it interrupts a turn), not a reason to kill it.
	agentCtx := context.WithoutCancel(ctx)
	var cmd *exec.Cmd
	switch agent {
	case "claude":
		cmd = exec.CommandContext(agentCtx, bin, args...) //nolint:gosec // the user's own agent
		cmd.Env = claudeEnv(os.Environ(), base)
	case "codex":
		provider := []string{
			"-c", `model_provider="jev-gateway"`,
			"-c", `model_providers.jev-gateway.name="jev-gateway"`,
			"-c", `model_providers.jev-gateway.base_url="` + base + `/v1"`,
			"-c", `model_providers.jev-gateway.wire_api="responses"`,
			"-c", `model_providers.jev-gateway.requires_openai_auth=true`,
		}
		cmd = exec.CommandContext(agentCtx, bin, append(provider, args...)...) //nolint:gosec // the user's own agent
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	// Ctrl-C reaches the agent directly (same process group); wait for it to
	// finish instead of exiting under it.
	err = cmd.Wait()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return ExitError{Code: exitErr.ExitCode()}
	}
	return err
}

// claudeEnv points Claude Code at the gateway. Claude Code turns tool search off when
// ANTHROPIC_BASE_URL is not Anthropic's, which puts every tool definition (MCP servers included) in
// every request; the gateway forwards tool search unchanged, so it is turned back on unless the user
// chose a value.
func claudeEnv(environ []string, base string) []string {
	env := make([]string, 0, len(environ)+2)
	env = append(env, environ...)
	env = append(env, "ANTHROPIC_BASE_URL="+base)
	for _, kv := range environ {
		if strings.HasPrefix(kv, "ENABLE_TOOL_SEARCH=") {
			return env
		}
	}
	return append(env, "ENABLE_TOOL_SEARCH=true")
}
