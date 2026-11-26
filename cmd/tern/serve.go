package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tern-agent/tern/internal/config"
	"github.com/tern-agent/tern/internal/provider"
	"github.com/tern-agent/tern/internal/serve"
)

// defaultServePort is the default port for `tern serve`.
const defaultServePort = 7433

// serveOptions is the parsed `tern serve` command line.
type serveOptions struct {
	Host       string
	Port       int
	LAN        bool
	Password   string
	Auth       bool
	NoAuth     bool
	Model      string
	ModelSet   bool
	Local      bool
	Permission string
	PermSet    bool
	Continue   bool
	Resume     string
	Help       bool
}

func printServeUsage(w io.Writer) {
	fmt.Fprintln(w, `tern serve - a lightweight LAN web UI for tern

Usage:
  tern serve [flags]

Flags:
      --host <addr>         bind address (default 127.0.0.1)
      --port <n>            HTTP port (default 7433)
      --lan                 shorthand for --host 0.0.0.0
      --password <pw>       optional login password (or set TERN_SERVE_PASSWORD)
      --auth                generate a random login password automatically
      --no-auth             disable login password even if TERN_SERVE_PASSWORD is set
      --model <name>        model name
      --local               use Ollama at http://localhost:11434/v1
      --permission <mode>   ask, auto-edit, yolo or plan
  -c, --continue            continue the most recent session
      --resume <id>         resume a specific session
  -h, --help                show this help`)
}

func parseServeArgs(args []string) (*serveOptions, error) {
	opts := &serveOptions{Host: "127.0.0.1", Port: defaultServePort}
	fs := flag.NewFlagSet("tern serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.Host, "host", opts.Host, "bind address")
	fs.IntVar(&opts.Port, "port", opts.Port, "HTTP port")
	fs.BoolVar(&opts.LAN, "lan", false, "bind to 0.0.0.0")
	fs.StringVar(&opts.Password, "password", "", "optional login password")
	fs.BoolVar(&opts.Auth, "auth", false, "generate a random login password")
	fs.BoolVar(&opts.NoAuth, "no-auth", false, "disable login password")
	fs.StringVar(&opts.Model, "model", "", "model name")
	fs.BoolVar(&opts.Local, "local", false, "use a local OpenAI-compatible server")
	fs.StringVar(&opts.Permission, "permission", "", "permission mode")
	fs.BoolVar(&opts.Continue, "continue", false, "continue the most recent session")
	fs.BoolVar(&opts.Continue, "c", false, "continue the most recent session")
	fs.StringVar(&opts.Resume, "resume", "", "resume a specific session id")
	fs.BoolVar(&opts.Help, "help", false, "show help")
	fs.BoolVar(&opts.Help, "h", false, "show help")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if rest := fs.Args(); len(rest) > 0 {
		return nil, fmt.Errorf("unexpected argument %q", rest[0])
	}
	visited := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	opts.ModelSet = visited["model"]
	opts.PermSet = visited["permission"]
	if opts.LAN {
		opts.Host = "0.0.0.0"
	}
	if opts.Port < 0 || opts.Port > 65535 {
		return nil, fmt.Errorf("invalid port %d", opts.Port)
	}
	if strings.TrimSpace(opts.Host) == "" {
		return nil, errors.New("host must not be empty")
	}
	return opts, nil
}

// runServeCommand runs `tern serve`. It is wired into runWithOptions in
// cli.go; it also runs standalone in tests.
func runServeCommand(args []string, o runOptions) int {
	if o.In == nil {
		o.In = strings.NewReader("")
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Err == nil {
		o.Err = io.Discard
	}
	if o.Version == "" {
		o.Version = version
	}

	opts, err := parseServeArgs(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printServeUsage(o.Out)
			return 0
		}
		fmt.Fprintf(o.Err, "tern serve: %v\n", err)
		return 2
	}
	if opts.Help {
		printServeUsage(o.Out)
		return 0
	}

	workDir := strings.TrimSpace(o.WorkDir)
	if workDir == "" {
		workDir, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(o.Err, "tern serve: determine working directory: %v\n", err)
			return 1
		}
	}

	// Authentication is decided before anything heavy starts so a generated
	// password can be shown as part of the first banner.
	password, authEnabled, generated, err := resolveServeAuth(opts, os.Getenv("TERN_SERVE_PASSWORD"))
	if err != nil {
		fmt.Fprintf(o.Err, "tern serve: %v\n", err)
		return 1
	}

	flags := &config.FlagOverrides{}
	if opts.ModelSet {
		flags.Model = &opts.Model
	}
	if opts.Local {
		local := defaultLocalBaseURL
		flags.BaseURL = &local
	}
	if opts.PermSet {
		flags.PermissionMode = &opts.Permission
	}
	cfg, err := config.Load(config.LoadOptions{WorkDir: workDir, Flags: flags})
	if err != nil {
		fmt.Fprintf(o.Err, "tern serve: %v\n", err)
		return 1
	}

	// First-run authentication mirrors the CLI: loopback base URLs (Ollama,
	// LM Studio, vLLM) are offline endpoints without an API key.
	if strings.TrimSpace(cfg.APIKey) == "" {
		if isLoopbackBaseURL(cfg.BaseURL) {
			cfg.APIKey = "local"
		} else {
			key, authErr := ensureAPIKey(o.In, o.Out, o.Err, o.StdinTTY)
			if authErr != nil {
				fmt.Fprintf(o.Err, "tern serve: %v\n", authErr)
				return 1
			}
			cfg.APIKey = key
		}
	}

	configDir, err := config.ConfigDir()
	if err != nil {
		fmt.Fprintf(o.Err, "tern serve: %v\n", err)
		return 1
	}
	dataDir, err := config.DataDir()
	if err != nil {
		fmt.Fprintf(o.Err, "tern serve: %v\n", err)
		return 1
	}

	prov := o.Provider
	if prov == nil {
		client := provider.NewDeepSeekClient(cfg.APIKey)
		client.BaseURL = cfg.BaseURL
		prov = client
	}

	// Bind first so a busy port fails fast, before sessions and MCP clients
	// are set up.
	addr := net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(o.Err, "tern serve: listen on %s: %v\n", addr, err)
		return 1
	}
	defer func() { _ = listener.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rt, err := BuildRuntime(RuntimeOptions{
		Context:   ctx,
		Config:    cfg,
		WorkDir:   workDir,
		Provider:  prov,
		ConfigDir: configDir,
		DataDir:   dataDir,
		Resume:    opts.Resume,
		Continue:  opts.Continue,
	})
	if err != nil {
		fmt.Fprintf(o.Err, "tern serve: %v\n", err)
		return 1
	}
	defer func() { _ = rt.Close() }()
	for _, warning := range rt.Warnings {
		fmt.Fprintf(o.Err, "tern serve: warning: %s\n", warning)
	}

	srv := serve.New(serve.Options{
		Context:     ctx,
		Agent:       rt.Agent,
		Session:     rt.Session,
		Store:       rt.SessionStore,
		Config:      cfg,
		Permissions: rt.Permissions,
		Version:     o.Version,
		WorkDir:     workDir,
		Password:    password,
		AuthEnabled: authEnabled,
	})
	defer srv.Close()

	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	port := opts.Port
	if tcpAddr, ok := listener.Addr().(*net.TCPAddr); ok && tcpAddr.Port > 0 {
		port = tcpAddr.Port
	}
	printServeBanner(o.Out, o.Version, port, opts.Host, password, authEnabled, generated, cfg, rt.Session.ID())

	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.Serve(listener) }()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(o.Err, "tern serve: %v\n", err)
			return 1
		}
		return 0
	case <-ctx.Done():
		fmt.Fprintln(o.Err, "tern serve: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		srv.Close()
		return 0
	}
}

// resolveServeAuth decides whether the server requires a password. Password
// authentication is optional: it is enabled when --password,
// TERN_SERVE_PASSWORD, or --auth (random password) is provided, and can be
// explicitly disabled with --no-auth.
func resolveServeAuth(opts *serveOptions, envPassword string) (password string, enabled bool, generated bool, err error) {
	if opts.NoAuth {
		return "", false, false, nil
	}
	password = strings.TrimSpace(opts.Password)
	if password == "" {
		password = strings.TrimSpace(envPassword)
	}
	if password != "" {
		return password, true, false, nil
	}
	if opts.Auth {
		generatedPassword, genErr := serve.GeneratePassword()
		if genErr != nil {
			return "", false, false, fmt.Errorf("generate password: %w (pass --password)", genErr)
		}
		return generatedPassword, true, true, nil
	}
	return "", false, false, nil
}

// printServeBanner prints the local/LAN URLs, credentials and session info.
func printServeBanner(w io.Writer, version string, port int, host, password string, authEnabled, generated bool, cfg *config.Config, sessionID string) {
	fmt.Fprintf(w, "tern serve %s\n", version)
	fmt.Fprintf(w, "  %-11s %s\n", "local", fmt.Sprintf("http://127.0.0.1:%d", port))

	lanURLs := serve.LANAddresses()
	if isLoopbackHost(host) {
		// Only the loopback URL is reachable; keep the banner honest.
	} else if host != "0.0.0.0" && host != "::" {
		fmt.Fprintf(w, "  %-11s %s\n", "lan", fmt.Sprintf("http://%s:%d", host, port))
	} else if len(lanURLs) == 0 {
		fmt.Fprintf(w, "  %-11s %s\n", "lan", "(no LAN IPv4 address found)")
	} else {
		for _, ip := range lanURLs {
			fmt.Fprintf(w, "  %-11s %s\n", "lan", fmt.Sprintf("http://%s:%d", ip, port))
		}
	}

	switch {
	case !authEnabled:
		hint := "disabled (use --password <pw> or --auth to enable)"
		if !isLoopbackHost(host) {
			hint = "disabled (LAN open — use --password <pw> or --auth to protect)"
		}
		fmt.Fprintf(w, "  %-11s %s\n", "password", hint)
	case generated:
		fmt.Fprintf(w, "  %-11s %s\n", "password", password+"  (generated)")
	default:
		fmt.Fprintf(w, "  %-11s %s\n", "password", password)
	}

	effort := cfg.ReasoningEffort
	if effort == "" {
		effort = "-"
	}
	fmt.Fprintf(w, "  %-11s %s · effort %s · permission %s\n", "model", cfg.Model, effort, cfg.PermissionMode)
	if sessionID != "" {
		fmt.Fprintf(w, "  %-11s %s\n", "session", sessionID)
	}
	fmt.Fprintln(w, "press Ctrl+C to stop")
}

// isLoopbackHost reports whether host only listens on the loopback interface.
func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	switch host {
	case "", "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
