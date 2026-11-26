package ext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Extension layout, manifest and namespacing constants.
const (
	// ExtensionManifestFile is the preferred manifest filename inside an
	// extension directory.
	ExtensionManifestFile = "extension.toml"
	// AltManifestFile is accepted when extension.toml is absent.
	AltManifestFile = "manifest.toml"
	// ExtensionsDirName is the directory (below a config/work dir) that holds
	// installed extensions.
	ExtensionsDirName = "extensions"
	// ProjectExtensionsDir is the project-relative extensions directory.
	ProjectExtensionsDir = ".tern/extensions"

	// ExtensionToolPrefix namespaces tools contributed by extensions.
	ExtensionToolPrefix = "ext_"
	// DefaultExtensionToolTimeout is applied when a tool does not set one.
	DefaultExtensionToolTimeout = 30 * time.Second
	// DefaultToolParameters is the JSON schema used when a tool omits one.
	DefaultToolParameters = `{"type":"object","properties":{},"additionalProperties":false}`

	// ExtensionSourceGlobal marks extensions installed in the global config.
	ExtensionSourceGlobal = "global"
	// ExtensionSourceProject marks extensions found in the project tree.
	ExtensionSourceProject = "project"

	maxExtensionToolOutput = 64 << 10 // 64 KiB per stream
)

// Conventional extension subdirectories.
const (
	extensionCommandsDir = "commands"
	extensionSkillsDir   = "skills"
	extensionAgentsDir   = "agents"
)

// ErrManifestNotFound is returned when an extension directory has no manifest.
var ErrManifestNotFound = errors.New("ext: extension manifest not found")

// ExtensionToolConfig describes one tool contributed by an extension manifest.
type ExtensionToolConfig struct {
	Name           string   `toml:"name"`
	Description    string   `toml:"description"`
	Command        string   `toml:"command"`
	Args           []string `toml:"args"`
	Parameters     string   `toml:"parameters"`
	ReadOnly       bool     `toml:"read_only"`
	TimeoutSeconds int      `toml:"timeout_seconds"`
}

// ExtensionManifest is the parsed extension.toml / manifest.toml file.
type ExtensionManifest struct {
	Name        string                `toml:"name"`
	Version     string                `toml:"version"`
	Description string                `toml:"description"`
	Author      string                `toml:"author"`
	Enabled     *bool                 `toml:"enabled"`
	Tools       []ExtensionToolConfig `toml:"tools"`
	Hooks       []HookConfig          `toml:"hooks"`
	MCP         []ServerConfig        `toml:"mcp"`
}

// IsEnabled reports whether the extension should be loaded. Extensions are
// enabled unless the manifest explicitly says otherwise.
func (m ExtensionManifest) IsEnabled() bool {
	return m.Enabled == nil || *m.Enabled
}

// Extension is a discovered or installed extension.
type Extension struct {
	Manifest    ExtensionManifest
	Dir         string
	Source      string
	CommandsDir string
	SkillsDir   string
	AgentsDir   string
}

// ExtensionTool is an executable tool contributed by an extension.
type ExtensionTool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	ReadOnly    bool
	Timeout     time.Duration
	Command     string
	Args        []string
	ExtDir      string
}

// Execute runs the tool with args encoded as JSON on stdin.
//
// The process runs in workDir, receives TERN_EXT_DIR and TERN_WORK_DIR in its
// environment, and is bounded by the tool timeout and ctx. stdout is returned
// trimmed; a non-zero exit or timeout produces a descriptive error that
// includes the captured stderr.
func (t ExtensionTool) Execute(ctx context.Context, workDir string, args json.RawMessage) (string, error) {
	if strings.TrimSpace(t.Command) == "" {
		return "", fmt.Errorf("ext: tool %q has no command", t.Name)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	payload := bytes.TrimSpace(args)
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = DefaultExtensionToolTimeout
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, t.Command, t.Args...)
	cmd.Dir = workDir
	cmd.Stdin = bytes.NewReader(payload)
	stdout := &lockedBuffer{max: maxExtensionToolOutput}
	stderr := &lockedBuffer{max: maxExtensionToolOutput}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = append(os.Environ(),
		"TERN_EXT_DIR="+t.ExtDir,
		"TERN_WORK_DIR="+workDir,
	)
	cmd.WaitDelay = 2 * time.Second

	runErr := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	errOut := strings.TrimSpace(stderr.String())

	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		msg := fmt.Sprintf("ext: tool %q timed out after %s", t.Name, timeout)
		if errOut != "" {
			msg += ": " + errOut
		}
		return "", fmt.Errorf("%s: %w", msg, context.DeadlineExceeded)
	case ctx.Err() != nil:
		return "", fmt.Errorf("ext: tool %q canceled: %w", t.Name, ctx.Err())
	case runErr != nil:
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			code := exitErr.ExitCode()
			if code == 0 {
				code = -1
			}
			msg := fmt.Sprintf("ext: tool %q exited with code %d", t.Name, code)
			detail := firstNonEmpty(errOut, out)
			if detail != "" {
				msg += ": " + detail
			}
			return "", errors.New(msg)
		}
		return "", fmt.Errorf("ext: run tool %q: %w", t.Name, runErr)
	}
	return out, nil
}

// ExtensionSet is a collection of extensions with aggregate accessors.
type ExtensionSet []*Extension

// NewExtensionSet builds an ExtensionSet from extensions.
func NewExtensionSet(exts []*Extension) ExtensionSet {
	return ExtensionSet(exts)
}

// Tools returns every enabled extension tool in deterministic extension order.
func (s ExtensionSet) Tools() []ExtensionTool {
	var out []ExtensionTool
	for _, e := range s {
		if e == nil || !e.Manifest.IsEnabled() {
			continue
		}
		for _, cfg := range e.Manifest.Tools {
			params := strings.TrimSpace(cfg.Parameters)
			if params == "" {
				params = DefaultToolParameters
			}
			timeout := DefaultExtensionToolTimeout
			if cfg.TimeoutSeconds > 0 {
				timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
			}
			out = append(out, ExtensionTool{
				Name:        extensionToolName(e.Manifest.Name, cfg.Name),
				Description: cfg.Description,
				Parameters:  json.RawMessage(params),
				ReadOnly:    cfg.ReadOnly,
				Timeout:     timeout,
				Command:     resolveExtensionPath(e.Dir, cfg.Command),
				Args:        append([]string(nil), cfg.Args...),
				ExtDir:      e.Dir,
			})
		}
	}
	return out
}

// Hooks returns every enabled extension hook in deterministic extension order.
func (s ExtensionSet) Hooks() []HookConfig {
	var out []HookConfig
	for _, e := range s {
		if e == nil || !e.Manifest.IsEnabled() {
			continue
		}
		for _, cfg := range e.Manifest.Hooks {
			cfg.Command = resolveExtensionPath(e.Dir, cfg.Command)
			cfg.Args = append([]string(nil), cfg.Args...)
			out = append(out, cfg.WithDefaults())
		}
	}
	return out
}

// MCPServers returns every enabled extension MCP server in deterministic
// extension order.
func (s ExtensionSet) MCPServers() []ServerConfig {
	var out []ServerConfig
	for _, e := range s {
		if e == nil || !e.Manifest.IsEnabled() {
			continue
		}
		for _, cfg := range e.Manifest.MCP {
			cfg = cfg.normalized()
			if cfg.Transport == TransportStdio {
				cfg.Command = resolveExtensionPath(e.Dir, cfg.Command)
			}
			cfg.Args = append([]string(nil), cfg.Args...)
			cfg.Env = append([]string(nil), cfg.Env...)
			out = append(out, cfg)
		}
	}
	return out
}

// CommandDirs returns existing conventional command directories.
func (s ExtensionSet) CommandDirs() []string {
	return s.conventionalDirs(func(e *Extension) string { return e.CommandsDir })
}

// SkillDirs returns existing conventional skill directories.
func (s ExtensionSet) SkillDirs() []string {
	return s.conventionalDirs(func(e *Extension) string { return e.SkillsDir })
}

// AgentDirs returns existing conventional agent directories.
func (s ExtensionSet) AgentDirs() []string {
	return s.conventionalDirs(func(e *Extension) string { return e.AgentsDir })
}

func (s ExtensionSet) conventionalDirs(get func(*Extension) string) []string {
	var out []string
	for _, e := range s {
		if e == nil || !e.Manifest.IsEnabled() {
			continue
		}
		if dir := strings.TrimSpace(get(e)); dir != "" {
			out = append(out, dir)
		}
	}
	return out
}

// DiscoverExtensions scans global and project extension directories and
// returns enabled extensions sorted by name, plus non-fatal warnings for
// broken manifests. Project extensions override global extensions with the
// same manifest name.
func DiscoverExtensions(workDir, globalConfigDir string) ([]*Extension, []string) {
	return listExtensions(workDir, globalConfigDir, false)
}

// ListAllExtensions is like DiscoverExtensions but also returns disabled
// extensions, for `tern ext list` style commands.
func ListAllExtensions(workDir, globalConfigDir string) ([]*Extension, []string) {
	return listExtensions(workDir, globalConfigDir, true)
}

func listExtensions(workDir, globalConfigDir string, includeDisabled bool) ([]*Extension, []string) {
	var warnings []string
	byName := make(map[string]*Extension)

	addSource := func(root, source string) {
		if strings.TrimSpace(root) == "" {
			return
		}
		exts, warns := scanExtensionDir(root, source)
		warnings = append(warnings, warns...)
		for _, e := range exts {
			if !includeDisabled && !e.Manifest.IsEnabled() {
				continue
			}
			byName[e.Manifest.Name] = e
		}
	}

	if strings.TrimSpace(globalConfigDir) != "" {
		addSource(filepath.Join(globalConfigDir, ExtensionsDirName), ExtensionSourceGlobal)
	}
	if strings.TrimSpace(workDir) != "" {
		addSource(filepath.Join(workDir, filepath.FromSlash(ProjectExtensionsDir)), ExtensionSourceProject)
	}

	out := make([]*Extension, 0, len(byName))
	for _, e := range byName {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Manifest.Name < out[j].Manifest.Name
	})
	return out, warnings
}

func scanExtensionDir(root, source string) ([]*Extension, []string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []string{fmt.Sprintf("ext: read extensions directory %s: %v", root, err)}
	}

	var (
		out      []*Extension
		warnings []string
	)
	for _, entry := range entries {
		// os.ReadDir sorts by filename, which keeps warnings deterministic.
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		ext, err := loadExtension(dir, source)
		if err != nil {
			if errors.Is(err, ErrManifestNotFound) {
				continue
			}
			warnings = append(warnings, fmt.Sprintf("ext: skip extension %s: %v", dir, err))
			continue
		}
		out = append(out, ext)
	}
	return out, warnings
}

func loadExtension(dir, source string) (*Extension, error) {
	manifest, err := ReadExtensionManifest(dir)
	if err != nil {
		return nil, err
	}
	if err := normalizeManifest(&manifest, dir); err != nil {
		return nil, err
	}
	ext := &Extension{
		Manifest: manifest,
		Dir:      dir,
		Source:   source,
	}
	if isDir(filepath.Join(dir, extensionCommandsDir)) {
		ext.CommandsDir = filepath.Join(dir, extensionCommandsDir)
	}
	if isDir(filepath.Join(dir, extensionSkillsDir)) {
		ext.SkillsDir = filepath.Join(dir, extensionSkillsDir)
	}
	if isDir(filepath.Join(dir, extensionAgentsDir)) {
		ext.AgentsDir = filepath.Join(dir, extensionAgentsDir)
	}
	return ext, nil
}

// ReadExtensionManifest loads extension.toml (or manifest.toml) from dir.
func ReadExtensionManifest(dir string) (ExtensionManifest, error) {
	var manifest ExtensionManifest
	path := ""
	for _, name := range []string{ExtensionManifestFile, AltManifestFile} {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			path = candidate
			break
		}
	}
	if path == "" {
		return manifest, fmt.Errorf("%w in %s", ErrManifestNotFound, dir)
	}
	if _, err := toml.DecodeFile(path, &manifest); err != nil {
		return manifest, fmt.Errorf("ext: parse %s: %w", path, err)
	}
	return manifest, nil
}

func normalizeManifest(manifest *ExtensionManifest, dir string) error {
	manifest.Name = strings.TrimSpace(manifest.Name)
	if err := validateExtensionName(manifest.Name); err != nil {
		return err
	}
	for i := range manifest.Tools {
		tool := &manifest.Tools[i]
		tool.Name = strings.TrimSpace(tool.Name)
		if tool.Name == "" {
			return fmt.Errorf("ext: extension %q has a tool without a name", manifest.Name)
		}
		if strings.TrimSpace(tool.Command) == "" {
			return fmt.Errorf("ext: tool %q of extension %q has no command", tool.Name, manifest.Name)
		}
		if tool.TimeoutSeconds < 0 {
			return fmt.Errorf("ext: tool %q of extension %q has a negative timeout", tool.Name, manifest.Name)
		}
		if tool.TimeoutSeconds == 0 {
			tool.TimeoutSeconds = int(DefaultExtensionToolTimeout / time.Second)
		}
		tool.Parameters = strings.TrimSpace(tool.Parameters)
		if tool.Parameters == "" {
			tool.Parameters = DefaultToolParameters
		} else if !json.Valid([]byte(tool.Parameters)) {
			return fmt.Errorf("ext: tool %q of extension %q has invalid parameters JSON", tool.Name, manifest.Name)
		}
	}
	for i := range manifest.Hooks {
		hook := &manifest.Hooks[i]
		hook.Command = resolveExtensionPath(dir, hook.Command)
		hook.Args = append([]string(nil), hook.Args...)
		if err := hook.WithDefaults().Validate(); err != nil {
			return fmt.Errorf("ext: extension %q: %w", manifest.Name, err)
		}
		if hook.Timeout <= 0 {
			hook.Timeout = DefaultHookTimeout
		}
	}
	for i := range manifest.MCP {
		server := manifest.MCP[i].normalized()
		if server.Transport == TransportStdio {
			server.Command = resolveExtensionPath(dir, server.Command)
		}
		if err := server.validate(); err != nil {
			return fmt.Errorf("ext: extension %q: %w", manifest.Name, err)
		}
		server.Args = append([]string(nil), server.Args...)
		server.Env = append([]string(nil), server.Env...)
		manifest.MCP[i] = server
	}
	return nil
}

func extensionToolName(extName, toolName string) string {
	name := sanitizeToolIdentifier(strings.TrimSpace(toolName))
	ext := sanitizeToolIdentifier(strings.TrimSpace(extName))
	if name == "" {
		name = "tool"
	}
	if strings.HasPrefix(name, ExtensionToolPrefix) {
		return name
	}
	if ext == "" {
		return ExtensionToolPrefix + name
	}
	return ExtensionToolPrefix + ext + "_" + name
}

func sanitizeToolIdentifier(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		case r == '.', r == ' ', r == '/', r == '\\':
			b.WriteByte('_')
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// resolveExtensionPath resolves relative command paths ("./hook.sh",
// "bin/server", and existing plain filenames) against dir. Bare command names
// that are not present in the extension directory are left for PATH lookup.
func resolveExtensionPath(dir, command string) string {
	command = strings.TrimSpace(command)
	if command == "" || filepath.IsAbs(command) || dir == "" {
		return command
	}
	looksRelative := strings.HasPrefix(command, ".") || strings.ContainsAny(command, `/\`)
	if !looksRelative {
		if _, err := os.Stat(filepath.Join(dir, command)); err != nil {
			return command
		}
	}
	return filepath.Join(dir, command)
}

// InstallExtension installs an extension from a local directory or git URL into
// <globalConfigDir>/extensions/<name>/.
func InstallExtension(globalConfigDir, source string) (*Extension, error) {
	if strings.TrimSpace(globalConfigDir) == "" {
		return nil, errors.New("ext: global config directory is required")
	}
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, errors.New("ext: extension source is required")
	}

	var (
		srcDir  string
		cleanup func()
	)
	if info, err := os.Stat(source); err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("ext: extension source %q is not a directory", source)
		}
		srcDir = source
	} else if isGitSource(source) {
		tmp, err := os.MkdirTemp("", "tern-extension-*")
		if err != nil {
			return nil, fmt.Errorf("ext: create temporary directory: %w", err)
		}
		cleanup = func() { _ = os.RemoveAll(tmp) }
		srcDir = filepath.Join(tmp, "repo")
		if err := cloneExtensionSource(source, srcDir); err != nil {
			cleanup()
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("ext: extension source %q is not a directory or git URL", source)
	}
	if cleanup != nil {
		defer cleanup()
	}

	manifest, err := ReadExtensionManifest(srcDir)
	if err != nil {
		return nil, err
	}
	if err := normalizeManifest(&manifest, srcDir); err != nil {
		return nil, err
	}

	dest := filepath.Join(globalConfigDir, ExtensionsDirName, manifest.Name)
	if _, err := os.Stat(dest); err == nil {
		return nil, fmt.Errorf("ext: extension %q is already installed at %s", manifest.Name, dest)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("ext: check install destination: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, fmt.Errorf("ext: create extensions directory: %w", err)
	}
	if err := copyExtensionTree(srcDir, dest); err != nil {
		_ = os.RemoveAll(dest)
		return nil, fmt.Errorf("ext: install extension %q: %w", manifest.Name, err)
	}

	return loadExtension(dest, ExtensionSourceGlobal)
}

// RemoveExtension removes <globalConfigDir>/extensions/<name>/.
func RemoveExtension(globalConfigDir, name string) error {
	if strings.TrimSpace(globalConfigDir) == "" {
		return errors.New("ext: global config directory is required")
	}
	name = strings.TrimSpace(name)
	if err := validateExtensionName(name); err != nil {
		return err
	}
	dir := filepath.Join(globalConfigDir, ExtensionsDirName, name)
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("ext: extension %q is not installed", name)
		}
		return fmt.Errorf("ext: stat extension %q: %w", name, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("ext: extension %q is not a directory", name)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("ext: remove extension %q: %w", name, err)
	}
	return nil
}

func validateExtensionName(name string) error {
	if name == "" {
		return errors.New("ext: extension name is required")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("ext: invalid extension name %q", name)
	}
	if strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return fmt.Errorf("ext: invalid extension name %q", name)
	}
	if filepath.Base(name) != name {
		return fmt.Errorf("ext: invalid extension name %q", name)
	}
	return nil
}

func isGitSource(source string) bool {
	lower := strings.ToLower(source)
	switch {
	case strings.HasPrefix(lower, "https://"),
		strings.HasPrefix(lower, "http://"),
		strings.HasPrefix(lower, "ssh://"),
		strings.HasPrefix(lower, "git://"),
		strings.HasPrefix(lower, "git@"):
		return true
	case strings.HasSuffix(lower, ".git"):
		return true
	}
	return false
}

func cloneExtensionSource(source, dest string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", source, dest)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("ext: git clone %s timed out", source)
		}
		msg := strings.TrimSpace(string(output))
		if msg != "" {
			return fmt.Errorf("ext: git clone %s: %w: %s", source, err, msg)
		}
		return fmt.Errorf("ext: git clone %s: %w", source, err)
	}
	return nil
}

func copyExtensionTree(src, dst string) error {
	srcClean := filepath.Clean(src)
	dstClean := filepath.Clean(dst)
	return filepath.Walk(srcClean, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcClean, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dstClean, 0o755)
		}
		// Never copy VCS metadata into the installed extension.
		if info.IsDir() && info.Name() == ".git" {
			return filepath.SkipDir
		}
		target := filepath.Join(dstClean, rel)
		// Guard against malicious trees that would escape the destination.
		if target != dstClean && !strings.HasPrefix(target, dstClean+string(os.PathSeparator)) {
			return fmt.Errorf("refusing to copy outside destination: %s", rel)
		}
		switch {
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyFile(path, target, info.Mode().Perm())
		default:
			// Skip sockets, devices, and other non-regular files.
			return nil
		}
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// ---------------------------------------------------------------------------
// TOML decoding for shared hook/server configs
// ---------------------------------------------------------------------------

// UnmarshalTOML decodes a hook table, accepting durations as strings ("5s") or
// numeric seconds.
func (c *HookConfig) UnmarshalTOML(value any) error {
	table, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("ext: hook must be a table, got %T", value)
	}
	var cfg HookConfig
	for key, raw := range table {
		switch strings.ToLower(key) {
		case "event":
			s, err := tomlString(raw)
			if err != nil {
				return fmt.Errorf("hook.%s: %w", key, err)
			}
			cfg.Event = HookEvent(s)
		case "command":
			s, err := tomlString(raw)
			if err != nil {
				return fmt.Errorf("hook.%s: %w", key, err)
			}
			cfg.Command = s
		case "args":
			args, err := tomlStringSlice(raw)
			if err != nil {
				return fmt.Errorf("hook.%s: %w", key, err)
			}
			cfg.Args = args
		case "matcher":
			s, err := tomlString(raw)
			if err != nil {
				return fmt.Errorf("hook.%s: %w", key, err)
			}
			cfg.Matcher = s
		case "timeout", "timeout_seconds":
			d, err := tomlDuration(raw)
			if err != nil {
				return fmt.Errorf("hook.%s: %w", key, err)
			}
			cfg.Timeout = d
		}
	}
	*c = cfg
	return nil
}

// UnmarshalTOML decodes an MCP server table, accepting durations as strings
// ("30s") or numeric seconds.
func (c *ServerConfig) UnmarshalTOML(value any) error {
	table, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("ext: mcp server must be a table, got %T", value)
	}
	var cfg ServerConfig
	for key, raw := range table {
		switch strings.ToLower(key) {
		case "name":
			s, err := tomlString(raw)
			if err != nil {
				return fmt.Errorf("mcp.%s: %w", key, err)
			}
			cfg.Name = s
		case "transport":
			s, err := tomlString(raw)
			if err != nil {
				return fmt.Errorf("mcp.%s: %w", key, err)
			}
			cfg.Transport = s
		case "command":
			s, err := tomlString(raw)
			if err != nil {
				return fmt.Errorf("mcp.%s: %w", key, err)
			}
			cfg.Command = s
		case "args":
			args, err := tomlStringSlice(raw)
			if err != nil {
				return fmt.Errorf("mcp.%s: %w", key, err)
			}
			cfg.Args = args
		case "env":
			env, err := tomlEnv(raw)
			if err != nil {
				return fmt.Errorf("mcp.%s: %w", key, err)
			}
			cfg.Env = env
		case "url":
			s, err := tomlString(raw)
			if err != nil {
				return fmt.Errorf("mcp.%s: %w", key, err)
			}
			cfg.URL = s
		case "headers":
			headers, err := tomlStringMap(raw)
			if err != nil {
				return fmt.Errorf("mcp.%s: %w", key, err)
			}
			cfg.Headers = headers
		case "timeout", "timeout_seconds":
			d, err := tomlDuration(raw)
			if err != nil {
				return fmt.Errorf("mcp.%s: %w", key, err)
			}
			cfg.Timeout = d
		}
	}
	*c = cfg
	return nil
}

func tomlString(value any) (string, error) {
	if s, ok := value.(string); ok {
		return s, nil
	}
	return "", fmt.Errorf("expected a string, got %T", value)
}

func tomlStringSlice(value any) ([]string, error) {
	switch values := value.(type) {
	case []string:
		return append([]string(nil), values...), nil
	case []any:
		out := make([]string, 0, len(values))
		for _, item := range values {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("expected a list of strings, got %T element", item)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("expected a list of strings, got %T", value)
	}
}

func tomlStringMap(value any) (map[string]string, error) {
	table, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected a table, got %T", value)
	}
	out := make(map[string]string, len(table))
	for key, raw := range table {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("expected a string for %q, got %T", key, raw)
		}
		out[key] = s
	}
	return out, nil
}

func tomlEnv(value any) ([]string, error) {
	if table, ok := value.(map[string]any); ok {
		keys := make([]string, 0, len(table))
		for key := range table {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make([]string, 0, len(keys))
		for _, key := range keys {
			s, ok := table[key].(string)
			if !ok {
				return nil, fmt.Errorf("expected a string for %q, got %T", key, table[key])
			}
			out = append(out, key+"="+s)
		}
		return out, nil
	}
	return tomlStringSlice(value)
}

func tomlDuration(value any) (time.Duration, error) {
	switch v := value.(type) {
	case string:
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", v, err)
		}
		return d, nil
	case int64:
		return time.Duration(v) * time.Second, nil
	case int:
		return time.Duration(v) * time.Second, nil
	case float64:
		return time.Duration(v * float64(time.Second)), nil
	default:
		return 0, fmt.Errorf("expected a duration string or seconds, got %T", value)
	}
}
