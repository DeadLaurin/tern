package tools

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// envLookup returns the value of the first entry named key, and how many
// entries use that name.
func envLookup(env []string, key string) (string, int) {
	value := ""
	count := 0
	for _, entry := range env {
		name, val, ok := strings.Cut(entry, "=")
		if ok && name == key {
			count++
			if count == 1 {
				value = val
			}
		}
	}
	return value, count
}

// unsetEnvForTest clears key for the duration of the test.
func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	old, ok := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(key, old)
			return
		}
		_ = os.Unsetenv(key)
	})
}

func TestNonInteractiveEnvAddsDefaults(t *testing.T) {
	base := []string{"PATH=/usr/bin:/bin", "HOME=/tmp/home"}
	env := nonInteractiveEnv(base)

	if len(env) != len(base)+len(nonInteractiveDefaults) {
		t.Fatalf("env has %d entries, want %d: %#v", len(env), len(base)+len(nonInteractiveDefaults), env)
	}
	want := map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"PAGER":               "cat",
		"GIT_PAGER":           "cat",
		"CI":                  "true",
		"DEBIAN_FRONTEND":     "noninteractive",
	}
	for key, value := range want {
		got, count := envLookup(env, key)
		if count != 1 {
			t.Fatalf("%s appears %d times in %#v", key, count, env)
		}
		if got != value {
			t.Fatalf("%s = %q, want %q", key, got, value)
		}
	}
	// Original entries are preserved verbatim and in order.
	for i, entry := range base {
		if env[i] != entry {
			t.Fatalf("env[%d] = %q, want %q", i, env[i], entry)
		}
	}
}

func TestNonInteractiveEnvDoesNotOverwriteUserValues(t *testing.T) {
	base := []string{
		"GIT_TERMINAL_PROMPT=1",
		"PAGER=less",
		"GIT_PAGER=delta",
		"CI=false",
		"DEBIAN_FRONTEND=dialog",
		"PATH=/usr/bin:/bin",
	}
	env := nonInteractiveEnv(base)

	if len(env) != len(base) {
		t.Fatalf("env grew to %d entries, want %d: %#v", len(env), len(base), env)
	}
	want := map[string]string{
		"GIT_TERMINAL_PROMPT": "1",
		"PAGER":               "less",
		"GIT_PAGER":           "delta",
		"CI":                  "false",
		"DEBIAN_FRONTEND":     "dialog",
	}
	for key, value := range want {
		got, count := envLookup(env, key)
		if count != 1 {
			t.Fatalf("%s appears %d times in %#v", key, count, env)
		}
		if got != value {
			t.Fatalf("%s = %q, want user value %q", key, got, value)
		}
	}
}

func TestNonInteractiveEnvNilBaseInheritsProcessEnv(t *testing.T) {
	unsetEnvForTest(t, "PAGER")
	t.Setenv("TERN_TEST_MARKER", "kept")

	env := nonInteractiveEnv(nil)
	if got, _ := envLookup(env, "TERN_TEST_MARKER"); got != "kept" {
		t.Fatalf("process env not inherited: %#v", env)
	}
	if got, _ := envLookup(env, "PAGER"); got != "cat" {
		t.Fatalf("PAGER = %q, want cat", got)
	}
}

func TestBashForegroundNonInteractiveEnv(t *testing.T) {
	for _, key := range []string{"GIT_TERMINAL_PROMPT", "PAGER", "GIT_PAGER", "CI", "DEBIAN_FRONTEND"} {
		unsetEnvForTest(t, key)
	}
	env := testEnv(t, t.TempDir())
	tool := NewBashTool()
	defer tool.Close()

	res, err := run(t, tool, env, map[string]any{
		"command": fmt.Sprintf("printf '%%s|%%s|%%s|%%s|%%s\\n' \"$%s\" \"$%s\" \"$%s\" \"$%s\" \"$%s\"",
			"GIT_TERMINAL_PROMPT", "PAGER", "GIT_PAGER", "CI", "DEBIAN_FRONTEND"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %q", res.Output)
	}
	if got, want := strings.TrimSpace(res.Output), "0|cat|cat|true|noninteractive"; got != want {
		t.Fatalf("foreground env = %q, want %q", got, want)
	}
}

func TestBashBackgroundNonInteractiveEnv(t *testing.T) {
	unsetEnvForTest(t, "PAGER")
	env := testEnv(t, t.TempDir())
	tool := NewBashTool()
	defer tool.Close()

	res, err := run(t, tool, env, map[string]any{
		"command":    `echo "PAGER_IS_${PAGER}"`,
		"background": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	jobID, _ := res.Metadata["job_id"].(string)
	if jobID == "" {
		t.Fatalf("missing job_id: %#v", res.Metadata)
	}

	// Wait for the job to finish, then inspect its captured output.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pres, err := run(t, tool, env, map[string]any{"action": "poll", "job_id": jobID})
		if err != nil {
			t.Fatal(err)
		}
		if pres.Metadata["status"] == string(JobCompleted) {
			if !strings.Contains(pres.Output, "PAGER_IS_cat") {
				t.Fatalf("background env = %q, want PAGER_IS_cat", pres.Output)
			}
			return
		}
		if pres.Metadata["status"] != string(JobRunning) {
			t.Fatalf("background job ended as %v: %q", pres.Metadata["status"], pres.Output)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("background job did not finish")
}

func TestBashForegroundLargeOutputStaysBounded(t *testing.T) {
	const outputCap = 16 * 1024
	env := ExecEnv{WorkDir: t.TempDir(), OutputCap: outputCap}
	tool := NewBashTool()
	defer tool.Close()

	// ~8 MB of output; the capture buffer must never hold all of it.
	res, err := run(t, tool, env, map[string]any{
		"command": `awk 'BEGIN { for (i = 0; i < 800000; i++) printf "0123456789" } END { printf "\nDONE\n" }'`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %q", res.Output)
	}
	if res.Metadata["exit_code"] != 0 {
		t.Fatalf("exit_code = %v", res.Metadata["exit_code"])
	}
	// The result must stay within the configured budget (plus the short
	// truncation marker), proving the capture buffer was bounded.
	if len(res.Output) > outputCap+256 {
		t.Fatalf("output is %d bytes, want <= %d", len(res.Output), outputCap+256)
	}
	if res.Output == "" {
		t.Fatal("output is empty")
	}
	if !strings.Contains(res.Output, "dropped") && !strings.Contains(res.Output, "truncated") {
		t.Fatalf("output does not report truncation: %q", res.Output)
	}
	// The head of the stream was dropped, so the capture kept recent bytes
	// rather than buffering the whole command.
	if strings.HasPrefix(res.Output, "0123456789") {
		t.Fatalf("expected head to be dropped: %q", res.Output[:64])
	}
}

func TestBashForegroundSmallOutputUnchanged(t *testing.T) {
	env := ExecEnv{WorkDir: t.TempDir(), OutputCap: DefaultOutputCap}
	tool := NewBashTool()
	defer tool.Close()

	res, err := run(t, tool, env, map[string]any{"command": `printf 'a\nb\n'`})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := res.Output, "a\nb\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %#v", res)
	}
}

func TestBashForegroundStdinIsClosed(t *testing.T) {
	// A command that reads stdin must see EOF instead of blocking until the
	// timeout.
	env := testEnv(t, t.TempDir())
	tool := NewBashTool()
	defer tool.Close()

	start := time.Now()
	res, err := run(t, tool, env, map[string]any{"command": `cat; echo eof`})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("command blocked on stdin for %v", elapsed)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %q", res.Output)
	}
	if !strings.Contains(res.Output, "eof") {
		t.Fatalf("output = %q, want it to contain eof", res.Output)
	}
}
