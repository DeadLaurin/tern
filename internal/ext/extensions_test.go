package ext

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// helperExtTool is the subprocess entry point for extension tool tests. It is
// dispatched by TestMain in ext_test.go via the shared TERN_EXT_HELPER pattern.
func helperExtTool() {
	mode := ""
	if len(os.Args) > 2 {
		mode = os.Args[2]
	}
	switch mode {
	case "fail":
		fmt.Fprintln(os.Stderr, "extension tool boom")
		os.Exit(7)
	case "sleep":
		ms, _ := strconv.Atoi(os.Getenv("EXT_TOOL_SLEEP_MS"))
		if ms <= 0 {
			ms = 5000
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		os.Exit(0)
	}

	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(4)
	}
	cwd, _ := os.Getwd()
	out := map[string]string{
		"stdin":    strings.TrimSpace(string(data)),
		"ext_dir":  os.Getenv("TERN_EXT_DIR"),
		"work_dir": os.Getenv("TERN_WORK_DIR"),
		"cwd":      cwd,
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(5)
	}
	os.Exit(0)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func extensionNames(exts []*Extension) []string {
	names := make([]string, 0, len(exts))
	for _, e := range exts {
		names = append(names, e.Manifest.Name)
	}
	return names
}

func TestDiscoverExtensionsSortsAndOverrides(t *testing.T) {
	t.Parallel()
	global := t.TempDir()
	work := t.TempDir()

	writeTestFile(t, filepath.Join(global, "extensions", "alpha", "extension.toml"), `
name = "alpha"
description = "global alpha"
[[tools]]
name = "run"
command = "echo"
`)
	writeTestFile(t, filepath.Join(global, "extensions", "zeta", "manifest.toml"), `
name = "zeta"
`)
	writeTestFile(t, filepath.Join(global, "extensions", "disabled", "extension.toml"), `
name = "disabled"
enabled = false
`)
	// A directory without a manifest is ignored rather than reported broken.
	if err := os.MkdirAll(filepath.Join(global, "extensions", "not-an-extension"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Project alpha overrides global alpha by manifest name.
	writeTestFile(t, filepath.Join(work, ".tern", "extensions", "project-alpha", "extension.toml"), `
name = "alpha"
description = "project alpha"
[[tools]]
name = "project_tool"
command = "echo"
`)
	writeTestFile(t, filepath.Join(work, ".tern", "extensions", "beta", "extension.toml"), `
name = "beta"
`)

	exts, warnings := DiscoverExtensions(work, global)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if got, want := extensionNames(exts), []string{"alpha", "beta", "zeta"}; !equalStrings(got, want) {
		t.Fatalf("discover names = %v, want %v", got, want)
	}
	if exts[0].Source != ExtensionSourceProject || exts[0].Manifest.Description != "project alpha" {
		t.Fatalf("project override not applied: %+v", exts[0])
	}
	if exts[0].Dir != filepath.Join(work, ".tern", "extensions", "project-alpha") {
		t.Fatalf("project dir = %s", exts[0].Dir)
	}
	if exts[2].Source != ExtensionSourceGlobal {
		t.Fatalf("zeta source = %s, want global", exts[2].Source)
	}

	// Discovery is stable across repeated scans.
	for i := 0; i < 3; i++ {
		again, warns := DiscoverExtensions(work, global)
		if len(warns) != 0 {
			t.Fatalf("unexpected warnings on repeat: %v", warns)
		}
		if got := extensionNames(again); !equalStrings(got, []string{"alpha", "beta", "zeta"}) {
			t.Fatalf("repeat %d names = %v", i, got)
		}
	}

	all, warnings := ListAllExtensions(work, global)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings from ListAll: %v", warnings)
	}
	if got, want := extensionNames(all), []string{"alpha", "beta", "disabled", "zeta"}; !equalStrings(got, want) {
		t.Fatalf("ListAll names = %v, want %v", got, want)
	}
	for _, e := range all {
		if e.Manifest.Name == "disabled" && e.Manifest.IsEnabled() {
			t.Fatal("disabled extension reported as enabled")
		}
	}

	set := NewExtensionSet(exts)
	tools := set.Tools()
	if len(tools) != 1 {
		t.Fatalf("tools = %+v, want exactly the project alpha tool", tools)
	}
	if tools[0].Name != "ext_alpha_project_tool" {
		t.Fatalf("tool name = %q", tools[0].Name)
	}
	if string(tools[0].Parameters) != DefaultToolParameters {
		t.Fatalf("default parameters = %s", tools[0].Parameters)
	}
	if tools[0].Timeout != DefaultExtensionToolTimeout {
		t.Fatalf("default timeout = %s", tools[0].Timeout)
	}
	if tools[0].ReadOnly {
		t.Fatalf("read_only default should be false")
	}
}

func TestExtensionToolNamespacingAndConventionalDirs(t *testing.T) {
	t.Parallel()
	global := t.TempDir()
	extDir := filepath.Join(global, "extensions", "demo")
	writeTestFile(t, filepath.Join(extDir, "extension.toml"), `
name = "demo"
[[tools]]
name = "search"
command = "true"
read_only = true
timeout_seconds = 2
[[tools]]
name = "ext_custom"
command = "true"
`)
	for _, sub := range []string{"commands", "skills", "agents"} {
		if err := os.MkdirAll(filepath.Join(extDir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	exts, warnings := DiscoverExtensions("", global)
	if len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}
	if len(exts) != 1 {
		t.Fatalf("extensions = %d", len(exts))
	}
	ext := exts[0]
	if ext.CommandsDir != filepath.Join(extDir, "commands") ||
		ext.SkillsDir != filepath.Join(extDir, "skills") ||
		ext.AgentsDir != filepath.Join(extDir, "agents") {
		t.Fatalf("conventional dirs not discovered: %+v", ext)
	}

	set := NewExtensionSet(exts)
	tools := set.Tools()
	if len(tools) != 2 {
		t.Fatalf("tools = %+v", tools)
	}
	if tools[0].Name != "ext_demo_search" {
		t.Fatalf("namespaced name = %q", tools[0].Name)
	}
	if !tools[0].ReadOnly || tools[0].Timeout != 2*time.Second {
		t.Fatalf("tool settings = %+v", tools[0])
	}
	if tools[1].Name != "ext_custom" {
		t.Fatalf("already-prefixed name = %q", tools[1].Name)
	}
	if got := set.CommandDirs(); !equalStrings(got, []string{filepath.Join(extDir, "commands")}) {
		t.Fatalf("CommandDirs = %v", got)
	}
	if got := set.SkillDirs(); !equalStrings(got, []string{filepath.Join(extDir, "skills")}) {
		t.Fatalf("SkillDirs = %v", got)
	}
	if got := set.AgentDirs(); !equalStrings(got, []string{filepath.Join(extDir, "agents")}) {
		t.Fatalf("AgentDirs = %v", got)
	}
}

func TestExtensionToolExecuteSubprocess(t *testing.T) {
	// Not parallel: t.Setenv mutates process environment.
	t.Setenv(helperEnv, "1")
	extDir := t.TempDir()
	workDir := t.TempDir()
	tool := ExtensionTool{
		Name:    "ext_demo_echo",
		Command: os.Args[0],
		Args:    []string{helperModeExtTool},
		ExtDir:  extDir,
		Timeout: 5 * time.Second,
	}

	out, err := tool.Execute(context.Background(), workDir, json.RawMessage(`{"path":"."}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode output %q: %v", out, err)
	}
	if got["stdin"] != `{"path":"."}` {
		t.Errorf("stdin = %q", got["stdin"])
	}
	if got["ext_dir"] != extDir {
		t.Errorf("TERN_EXT_DIR = %q, want %q", got["ext_dir"], extDir)
	}
	if got["work_dir"] != workDir {
		t.Errorf("TERN_WORK_DIR = %q, want %q", got["work_dir"], workDir)
	}
	if got["cwd"] != workDir {
		resolved, _ := filepath.EvalSymlinks(workDir)
		if got["cwd"] != resolved {
			t.Errorf("cwd = %q, want %q", got["cwd"], workDir)
		}
	}
}

func TestExtensionToolExecuteDefaultsStdinAndReportsFailure(t *testing.T) {
	t.Setenv(helperEnv, "1")
	extDir := t.TempDir()
	workDir := t.TempDir()

	// Empty args default to an empty JSON object.
	okTool := ExtensionTool{
		Name:    "ext_demo_echo",
		Command: os.Args[0],
		Args:    []string{helperModeExtTool},
		ExtDir:  extDir,
	}
	out, err := okTool.Execute(context.Background(), workDir, nil)
	if err != nil {
		t.Fatalf("Execute with nil args: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["stdin"] != "{}" {
		t.Fatalf("default stdin = %q, want {}", got["stdin"])
	}

	failTool := ExtensionTool{
		Name:    "ext_demo_fail",
		Command: os.Args[0],
		Args:    []string{helperModeExtTool, "fail"},
		ExtDir:  extDir,
	}
	if _, err := failTool.Execute(context.Background(), workDir, json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected failure for non-zero exit")
	} else if !strings.Contains(err.Error(), "exited with code 7") || !strings.Contains(err.Error(), "extension tool boom") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExtensionToolExecuteTimeout(t *testing.T) {
	t.Setenv(helperEnv, "1")
	t.Setenv("EXT_TOOL_SLEEP_MS", "5000")
	tool := ExtensionTool{
		Name:    "ext_demo_sleep",
		Command: os.Args[0],
		Args:    []string{helperModeExtTool, "sleep"},
		ExtDir:  t.TempDir(),
		Timeout: 100 * time.Millisecond,
	}
	start := time.Now()
	if _, err := tool.Execute(context.Background(), t.TempDir(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected timeout error")
	} else if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout took too long: %s", elapsed)
	}
}

func TestRelativeHookAndMCPPathsResolve(t *testing.T) {
	t.Parallel()
	global := t.TempDir()
	extDir := filepath.Join(global, "extensions", "relative")
	writeTestFile(t, filepath.Join(extDir, "extension.toml"), `
name = "relative"
[[tools]]
name = "local"
command = "./bin/local-tool"
[[hooks]]
event = "before_tool"
command = "./hook.sh"
args = ["one", "two"]
matcher = "shell*"
timeout = "3s"
[[hooks]]
event = "session_start"
command = "bin/nested-hook"
[[mcp]]
name = "local-mcp"
transport = "stdio"
command = "./mcp-server"
args = ["--stdio"]
`)
	writeTestFile(t, filepath.Join(extDir, "hook.sh"), "#!/bin/sh\n")

	exts, warnings := DiscoverExtensions("", global)
	if len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}
	if len(exts) != 1 {
		t.Fatalf("extensions = %d", len(exts))
	}

	set := NewExtensionSet(exts)
	hooks := set.Hooks()
	if len(hooks) != 2 {
		t.Fatalf("hooks = %+v", hooks)
	}
	if want := filepath.Join(extDir, "hook.sh"); hooks[0].Command != want {
		t.Errorf("relative hook command = %q, want %q", hooks[0].Command, want)
	}
	if hooks[0].Timeout != 3*time.Second || hooks[0].Matcher != "shell*" {
		t.Errorf("hook config = %+v", hooks[0])
	}
	if want := filepath.Join(extDir, "bin", "nested-hook"); hooks[1].Command != want {
		t.Errorf("nested hook command = %q, want %q", hooks[1].Command, want)
	}

	servers := set.MCPServers()
	if len(servers) != 1 {
		t.Fatalf("mcp servers = %+v", servers)
	}
	if want := filepath.Join(extDir, "mcp-server"); servers[0].Command != want {
		t.Errorf("relative mcp command = %q, want %q", servers[0].Command, want)
	}
	if servers[0].Transport != TransportStdio || len(servers[0].Args) != 1 {
		t.Errorf("mcp server = %+v", servers[0])
	}

	tools := set.Tools()
	if len(tools) != 1 {
		t.Fatalf("tools = %+v", tools)
	}
	if want := filepath.Join(extDir, "bin", "local-tool"); tools[0].Command != want {
		t.Errorf("relative tool command = %q, want %q", tools[0].Command, want)
	}
}

func TestInstallAndRemoveExtension(t *testing.T) {
	t.Parallel()
	global := t.TempDir()
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "extension.toml"), `
name = "installed"
version = "1.2.3"
description = "installed from dir"
[[tools]]
name = "hello"
command = "./tool.sh"
`)
	writeTestFile(t, filepath.Join(src, "tool.sh"), "#!/bin/sh\necho hello\n")
	writeTestFile(t, filepath.Join(src, "commands", "greet.md"), "# greet\n")
	writeTestFile(t, filepath.Join(src, ".git", "config"), "[core]\n")

	ext, err := InstallExtension(global, src)
	if err != nil {
		t.Fatalf("InstallExtension: %v", err)
	}
	if ext.Manifest.Name != "installed" || ext.Source != ExtensionSourceGlobal {
		t.Fatalf("installed extension = %+v", ext)
	}
	dest := filepath.Join(global, "extensions", "installed")
	if ext.Dir != dest {
		t.Fatalf("installed dir = %q, want %q", ext.Dir, dest)
	}
	if _, err := os.Stat(filepath.Join(dest, "extension.toml")); err != nil {
		t.Fatalf("manifest not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "tool.sh")); err != nil {
		t.Fatalf("tool not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "commands", "greet.md")); err != nil {
		t.Fatalf("commands not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); !os.IsNotExist(err) {
		t.Fatalf(".git should not be copied, stat err = %v", err)
	}
	if ext.CommandsDir != filepath.Join(dest, "commands") {
		t.Fatalf("commands dir = %q", ext.CommandsDir)
	}

	// Installing the same manifest again is rejected.
	if _, err := InstallExtension(global, src); err == nil {
		t.Fatal("expected duplicate install error")
	}

	// The installed extension is visible to discovery.
	exts, warnings := DiscoverExtensions("", global)
	if len(warnings) != 0 || len(exts) != 1 || exts[0].Manifest.Name != "installed" {
		t.Fatalf("discovery after install: %v %v", extensionNames(exts), warnings)
	}

	if err := RemoveExtension(global, "installed"); err != nil {
		t.Fatalf("RemoveExtension: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("extension still present after removal: %v", err)
	}
	if err := RemoveExtension(global, "installed"); err == nil {
		t.Fatal("expected error removing missing extension")
	}
}

func TestInstallRejectsInvalidManifestName(t *testing.T) {
	t.Parallel()
	global := t.TempDir()
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "extension.toml"), `
name = "../escape"
`)
	if _, err := InstallExtension(global, src); err == nil {
		t.Fatal("expected error for path-traversing manifest name")
	}
	if _, err := os.Stat(filepath.Join(global, "escape")); !os.IsNotExist(err) {
		t.Fatalf("install escaped the extensions directory: %v", err)
	}

	missing := t.TempDir()
	if _, err := InstallExtension(global, missing); err == nil {
		t.Fatal("expected error for directory without manifest")
	}
	if _, err := InstallExtension(global, "https://example.invalid/does-not-exist.git"); err == nil {
		t.Fatal("expected git clone failure")
	}
}

func TestRemoveExtensionRejectsPathTraversal(t *testing.T) {
	t.Parallel()
	global := t.TempDir()
	outside := filepath.Join(global, "victim")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"", ".", "..", "../victim", "a/b", `a\b`, "./x"} {
		if err := RemoveExtension(global, name); err == nil {
			t.Errorf("RemoveExtension(%q) succeeded, want rejection", name)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside directory was removed: %v", err)
	}
}

func TestMalformedManifestProducesWarning(t *testing.T) {
	t.Parallel()
	global := t.TempDir()
	writeTestFile(t, filepath.Join(global, "extensions", "broken", "extension.toml"), "name = [\n")
	writeTestFile(t, filepath.Join(global, "extensions", "good", "extension.toml"), "name = \"good\"\n")
	writeTestFile(t, filepath.Join(global, "extensions", "bad-tool", "extension.toml"), `
name = "bad-tool"
[[tools]]
name = "x"
command = "true"
parameters = "not json"
`)

	exts, warnings := DiscoverExtensions("", global)
	if got := extensionNames(exts); !equalStrings(got, []string{"good"}) {
		t.Fatalf("extensions = %v, want [good]", got)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want 2", warnings)
	}
	for _, want := range []string{"broken", "bad-tool"} {
		found := false
		for _, warning := range warnings {
			if strings.Contains(warning, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no warning mentions %q: %v", want, warnings)
		}
	}

	all, _ := ListAllExtensions("", global)
	if got := extensionNames(all); !equalStrings(got, []string{"good"}) {
		t.Fatalf("ListAll extensions = %v, want [good]", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
