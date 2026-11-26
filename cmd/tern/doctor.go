package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tern-agent/tern/internal/config"
	terncontext "github.com/tern-agent/tern/internal/context"
	"github.com/tern-agent/tern/internal/ext"
	"github.com/tern-agent/tern/internal/session"
)

// doctorReport is the machine-readable result of `tern doctor --json`. It is
// deliberately free of secrets: APIKeyStatus only ever contains a status
// marker, never the key itself.
type doctorReport struct {
	Version    string          `json:"version"`
	OS         string          `json:"os"`
	Arch       string          `json:"arch"`
	Executable string          `json:"executable,omitempty"`
	ConfigDir  string          `json:"config_dir"`
	DataDir    string          `json:"data_dir"`
	WorkDir    string          `json:"work_dir"`
	Model      string          `json:"model"`
	Reasoning  string          `json:"reasoning_effort"`
	BaseURL    string          `json:"base_url"`
	APIKey     string          `json:"api_key"`
	Toolchain  doctorToolchain `json:"toolchain"`
	Workspace  doctorWorkspace `json:"workspace"`
	Warnings   []string        `json:"warnings,omitempty"`
}

// doctorToolchain describes the external tools tern relies on.
type doctorToolchain struct {
	RipgrepFound bool   `json:"ripgrep_found"`
	RipgrepPath  string `json:"ripgrep_path,omitempty"`
	GitFound     bool   `json:"git_found"`
	GitPath      string `json:"git_path,omitempty"`
	GitBranch    string `json:"git_branch,omitempty"`
	Shell        string `json:"shell"`
	ShellSource  string `json:"shell_source"`
}

// doctorWorkspace summarizes the indexed workspace and its extensions.
type doctorWorkspace struct {
	RepoFiles     int    `json:"repo_files"`
	RepoDirs      int    `json:"repo_dirs"`
	RepoIgnored   int    `json:"repo_ignored"`
	RepoSummary   string `json:"repo_summary,omitempty"`
	RepoError     string `json:"repo_error,omitempty"`
	Skills        int    `json:"skills"`
	Extensions    int    `json:"extensions"`
	MCPServers    int    `json:"mcp_servers"`
	Hooks         int    `json:"hooks"`
	SavedSessions int    `json:"saved_sessions"`
}

func printDoctorUsage(w io.Writer) {
	fmt.Fprintln(w, `tern doctor - diagnose the local tern installation

Usage:
  tern doctor [--json]

Prints the tern version, platform, executable path, config/data/work
directories, effective model settings, API key status (never the key itself),
toolchain availability (ripgrep, git, shell), and workspace diagnostics
(repo index, skills, extensions, MCP servers, hooks, saved sessions).

Flags:
      --json    emit the diagnostic report as JSON
  -h, --help    show this help`)
}

// runDoctorCommand implements `tern doctor`.
func runDoctorCommand(args []string, o runOptions) int {
	fs := flag.NewFlagSet("tern doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonOut, help bool
	fs.BoolVar(&jsonOut, "json", false, "emit the diagnostic report as JSON")
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printDoctorUsage(o.Out)
			return 0
		}
		fmt.Fprintf(o.Err, "tern doctor: %v\n", err)
		return 2
	}
	if help {
		printDoctorUsage(o.Out)
		return 0
	}
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintf(o.Err, "tern doctor: unexpected argument %q\n", rest[0])
		return 2
	}

	workDir, err := commandWorkDir(o)
	if err != nil {
		fmt.Fprintf(o.Err, "tern doctor: determine working directory: %v\n", err)
		return 1
	}
	cfg, err := config.Load(config.LoadOptions{WorkDir: workDir})
	if err != nil {
		fmt.Fprintf(o.Err, "tern doctor: %v\n", err)
		return 1
	}
	report, err := collectDoctorReport(o.Version, workDir, cfg)
	if err != nil {
		fmt.Fprintf(o.Err, "tern doctor: %v\n", err)
		return 1
	}

	if jsonOut {
		enc := json.NewEncoder(o.Out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(o.Err, "tern doctor: %v\n", err)
			return 1
		}
		return 0
	}
	writeDoctorReport(o.Out, report)
	return 0
}

// collectDoctorReport gathers every diagnostic without failing on optional
// checks: missing tools, unreadable extensions and indexing errors become
// warnings instead of a non-zero exit.
func collectDoctorReport(version, workDir string, cfg *config.Config) (*doctorReport, error) {
	configDir, err := config.ConfigDir()
	if err != nil {
		return nil, err
	}
	dataDir, err := config.DataDir()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(version) == "" {
		version = "dev"
	}

	report := &doctorReport{
		Version:   version,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		ConfigDir: configDir,
		DataDir:   dataDir,
		WorkDir:   workDir,
		Model:     cfg.Model,
		Reasoning: cfg.ReasoningEffort,
		BaseURL:   cfg.BaseURL,
		APIKey:    apiKeyStatus(cfg),
		Toolchain: doctorToolchain{},
		Workspace: doctorWorkspace{},
	}
	if exe, err := os.Executable(); err == nil {
		report.Executable = exe
	}

	// Toolchain.
	if path, err := exec.LookPath("rg"); err == nil {
		report.Toolchain.RipgrepFound = true
		report.Toolchain.RipgrepPath = path
	}
	if path, err := exec.LookPath("git"); err == nil {
		report.Toolchain.GitFound = true
		report.Toolchain.GitPath = path
		report.Toolchain.GitBranch = gitBranch(workDir)
	}
	report.Toolchain.Shell, report.Toolchain.ShellSource = detectDoctorShell(cfg.Shell)

	// Workspace index.
	if index, err := terncontext.IndexRepo(workDir, 0); err != nil {
		report.Workspace.RepoError = err.Error()
		report.Warnings = append(report.Warnings, "repo index: "+err.Error())
	} else {
		report.Workspace.RepoFiles = index.FileCount
		report.Workspace.RepoDirs = index.DirCount
		report.Workspace.RepoIgnored = index.IgnoredCount
		report.Workspace.RepoSummary = index.Summary()
	}

	// Skills.
	if skills, err := terncontext.ListSkills(workDir, configDir); err != nil {
		report.Warnings = append(report.Warnings, "skills: "+err.Error())
	} else {
		report.Workspace.Skills = len(skills)
	}

	// Extensions, plus the hooks and MCP servers they contribute.
	extensions, extWarnings := ext.DiscoverExtensions(workDir, configDir)
	report.Workspace.Extensions = len(extensions)
	report.Warnings = append(report.Warnings, extWarnings...)
	extSet := ext.NewExtensionSet(extensions)

	hookConfigs, hookWarnings := loadHookConfigs(configDir, workDir)
	report.Warnings = append(report.Warnings, hookWarnings...)
	report.Workspace.Hooks = len(hookConfigs) + len(extSet.Hooks())

	mcpConfigs, mcpWarnings := loadMCPConfigs(configDir, workDir)
	report.Warnings = append(report.Warnings, mcpWarnings...)
	report.Workspace.MCPServers = len(mcpConfigs) + len(extSet.MCPServers())

	// Saved sessions.
	if store, err := session.NewStore(dataDir); err != nil {
		report.Warnings = append(report.Warnings, "sessions: "+err.Error())
	} else if sessions, err := store.List(""); err != nil {
		report.Warnings = append(report.Warnings, "sessions: "+err.Error())
	} else {
		report.Workspace.SavedSessions = len(sessions)
	}

	return report, nil
}

// apiKeyStatus renders the API key as a status marker. The key itself must
// never be printed by this command.
func apiKeyStatus(cfg *config.Config) string {
	if cfg == nil || strings.TrimSpace(cfg.APIKey) == "" {
		return "[not set]"
	}
	if isLoopbackBaseURL(cfg.BaseURL) {
		return "[local]"
	}
	return "[set]"
}

// gitBranch returns the current branch of the repository containing workDir,
// or an empty string when workDir is not inside a git repository.
func gitBranch(workDir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", workDir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// detectDoctorShell resolves the shell used by the bash tool and reports where
// the value came from.
func detectDoctorShell(configured string) (string, string) {
	if shell := strings.TrimSpace(configured); shell != "" {
		return shell, "configured"
	}
	if shell := strings.TrimSpace(os.Getenv("SHELL")); shell != "" {
		return shell, "detected"
	}
	if runtime.GOOS == "windows" {
		if path, err := exec.LookPath("powershell"); err == nil {
			return path, "detected"
		}
		return "powershell", "default"
	}
	if path, err := exec.LookPath("sh"); err == nil {
		return path, "detected"
	}
	return "sh", "default"
}

// writeDoctorReport renders the human-readable report.
func writeDoctorReport(w io.Writer, r *doctorReport) {
	fmt.Fprintln(w, "tern doctor")
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	section := func(title string) {
		fmt.Fprintln(tw, title)
	}
	row := func(tw *tabwriter.Writer, label, value string) {
		fmt.Fprintf(tw, "  %s\t%s\n", label, value)
	}
	section("System")
	row(tw, "Version", r.Version)
	row(tw, "OS/arch", r.OS+"/"+r.Arch)
	row(tw, "Executable", doctorValue(r.Executable))

	section("\nPaths")
	row(tw, "Config dir", r.ConfigDir)
	row(tw, "Data dir", r.DataDir)
	row(tw, "Work dir", r.WorkDir)

	section("\nModel")
	row(tw, "Model", doctorValue(r.Model))
	row(tw, "Reasoning effort", doctorValue(r.Reasoning))
	row(tw, "Base URL", doctorValue(r.BaseURL))
	row(tw, "API key", r.APIKey)

	section("\nToolchain")
	if r.Toolchain.RipgrepFound {
		row(tw, "ripgrep", "found ("+r.Toolchain.RipgrepPath+")")
	} else {
		row(tw, "ripgrep", "not found (using built-in Go grep fallback)")
	}
	if r.Toolchain.GitFound {
		branch := "not a git repository"
		if r.Toolchain.GitBranch != "" {
			branch = "branch " + r.Toolchain.GitBranch
		}
		row(tw, "git", "found ("+r.Toolchain.GitPath+"), "+branch)
	} else {
		row(tw, "git", "not found")
	}
	row(tw, "shell", fmt.Sprintf("%s (%s)", r.Toolchain.Shell, r.Toolchain.ShellSource))

	section("\nWorkspace & extensions")
	if r.Workspace.RepoError != "" {
		row(tw, "Repo index", "error: "+r.Workspace.RepoError)
	} else {
		summary := r.Workspace.RepoSummary
		if summary == "" {
			summary = fmt.Sprintf("%d files · %d dirs · %d ignored",
				r.Workspace.RepoFiles, r.Workspace.RepoDirs, r.Workspace.RepoIgnored)
		}
		row(tw, "Repo index", summary)
	}
	row(tw, "Skills", formatStatCount(r.Workspace.Skills))
	row(tw, "Extensions", formatStatCount(r.Workspace.Extensions))
	row(tw, "MCP servers", formatStatCount(r.Workspace.MCPServers))
	row(tw, "Hooks", formatStatCount(r.Workspace.Hooks))
	row(tw, "Saved sessions", formatStatCount(r.Workspace.SavedSessions))
	_ = tw.Flush()

	if len(r.Warnings) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Warnings")
		for _, warning := range r.Warnings {
			fmt.Fprintf(w, "  - %s\n", warning)
		}
	}
}

func doctorValue(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}
