package ext

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDiscoverPluginsAndLegacyExtensions(t *testing.T) {
	t.Parallel()
	global := t.TempDir()
	work := t.TempDir()

	// Primary plugin layouts.
	writeTestFile(t, filepath.Join(global, "plugins", "alpha", "plugin.toml"), `
name = "alpha"
description = "global plugin alpha"
[[tools]]
name = "ping"
command = "./tool.sh"
`)
	writeTestFile(t, filepath.Join(global, "plugins", "alpha", "tool.sh"), "#!/bin/sh\necho pong\n")
	writeTestFile(t, filepath.Join(work, ".tern", "plugins", "beta", "plugin.toml"), `
name = "beta"
description = "project plugin beta"
`)
	// Legacy layouts.
	writeTestFile(t, filepath.Join(global, "extensions", "legacy", "extension.toml"), `
name = "legacy"
description = "global legacy extension"
`)
	writeTestFile(t, filepath.Join(work, ".tern", "extensions", "legacy-project", "extension.toml"), `
name = "legacy-project"
description = "project legacy extension"
`)
	// Same name in both layouts: plugins/ must win.
	writeTestFile(t, filepath.Join(global, "extensions", "gamma", "extension.toml"), `
name = "gamma"
description = "legacy gamma"
`)
	writeTestFile(t, filepath.Join(global, "plugins", "gamma", "plugin.toml"), `
name = "gamma"
description = "plugin gamma"
[[tools]]
name = "hi"
command = "./tool.sh"
`)
	// plugin.toml wins over extension.toml inside the same directory.
	writeTestFile(t, filepath.Join(global, "plugins", "dual", "plugin.toml"), `
name = "dual"
description = "primary manifest"
`)
	writeTestFile(t, filepath.Join(global, "plugins", "dual", "extension.toml"), `
name = "dual-legacy"
description = "legacy manifest"
`)

	exts, warnings := DiscoverExtensions(work, global)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	got := map[string]*Extension{}
	for _, e := range exts {
		got[e.Manifest.Name] = e
	}
	for _, name := range []string{"alpha", "beta", "gamma", "dual", "legacy", "legacy-project"} {
		if got[name] == nil {
			t.Fatalf("plugin %q not discovered; got %v", name, extensionNames(exts))
		}
	}
	if _, ok := got["dual-legacy"]; ok {
		t.Fatal("extension.toml should be ignored when plugin.toml is present")
	}
	if desc := got["gamma"].Manifest.Description; desc != "plugin gamma" {
		t.Fatalf("gamma should come from plugins/, got description %q", desc)
	}
	if !strings.Contains(got["alpha"].Dir, filepath.Join("plugins", "alpha")) {
		t.Fatalf("alpha dir = %q, want plugins/alpha", got["alpha"].Dir)
	}
	if !strings.Contains(got["legacy"].Dir, filepath.Join("extensions", "legacy")) {
		t.Fatalf("legacy dir = %q, want extensions/legacy", got["legacy"].Dir)
	}

	// Disabled plugins are hidden from discovery but visible to ListAll.
	writeTestFile(t, filepath.Join(global, "plugins", "off", "plugin.toml"), `
name = "off"
enabled = false
`)
	if exts, _ := DiscoverExtensions(work, global); indexOfExtension(exts, "off") >= 0 {
		t.Fatal("disabled plugin should not be discovered")
	}
	all, _ := ListAllExtensions(work, global)
	if indexOfExtension(all, "off") < 0 {
		t.Fatal("disabled plugin should be listed by ListAllExtensions")
	}
}

func indexOfExtension(exts []*Extension, name string) int {
	for i, e := range exts {
		if e.Manifest.Name == name {
			return i
		}
	}
	return -1
}

func TestScaffoldPluginCreatesWorkingPlugin(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), "plugins")

	dir, err := ScaffoldPlugin(parent, "demo", "a demo plugin")
	if err != nil {
		t.Fatalf("ScaffoldPlugin: %v", err)
	}
	want := filepath.Join(parent, "demo")
	if dir != want {
		t.Fatalf("scaffold dir = %q, want %q", dir, want)
	}
	info, err := os.Stat(filepath.Join(dir, "tool.sh"))
	if err != nil {
		t.Fatalf("tool.sh missing: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("tool.sh is not executable: %v", info.Mode())
	}
	for _, name := range []string{PluginManifestFile, "README.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
	}

	// Discovery loads the scaffolded plugin with exactly one enabled tool.
	// parent is the plugins/ directory, so scan its parent.
	global := filepath.Dir(parent)
	exts, warnings := DiscoverExtensions("", global)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(exts) != 1 || exts[0].Manifest.Name != "demo" {
		t.Fatalf("discovered plugins = %v", extensionNames(exts))
	}
	if !exts[0].Manifest.IsEnabled() {
		t.Fatal("scaffolded plugin should be enabled")
	}
	set := NewExtensionSet(exts)
	tools := set.Tools()
	if len(tools) != 1 {
		t.Fatalf("scaffolded plugin tools = %d, want 1", len(tools))
	}
	if tools[0].Name != "ext_demo_hello" {
		t.Fatalf("tool name = %q, want ext_demo_hello", tools[0].Name)
	}
	if tools[0].Command != filepath.Join(dir, "tool.sh") {
		t.Fatalf("tool command = %q, want %q", tools[0].Command, filepath.Join(dir, "tool.sh"))
	}

	all, _ := ListAllExtensions("", global)
	if len(all) != 1 || len(all[0].Manifest.Tools) != 1 || !all[0].Manifest.IsEnabled() {
		t.Fatalf("ListAllExtensions = %+v", all)
	}

	// Scaffolding twice must not overwrite.
	if _, err := ScaffoldPlugin(parent, "demo", ""); err == nil {
		t.Fatal("expected error scaffolding over an existing plugin")
	}
}

func TestScaffoldPluginValidatesName(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	for _, name := range []string{"", ".", "..", "../escape", "a/b", `a\b`, "./x"} {
		if _, err := ScaffoldPlugin(parent, name, ""); err == nil {
			t.Errorf("ScaffoldPlugin(%q) succeeded, want rejection", name)
		}
	}
	if entries, err := os.ReadDir(parent); err == nil && len(entries) != 0 {
		t.Fatalf("invalid names created entries: %v", entries)
	}
}

func TestPluginGuideCoversManifest(t *testing.T) {
	t.Parallel()
	guide := PluginGuide()
	for _, want := range []string{"plugin.toml", "[[tools]]", "[[hooks]]", "[[mcp]]", "tern plugin new", "tern plugin list", "~/.config/tern/plugins", "./.tern/plugins", "stdin", "stdout"} {
		if !strings.Contains(guide, want) {
			t.Errorf("PluginGuide() missing %q", want)
		}
	}
}
