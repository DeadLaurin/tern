package permission

import (
	"strconv"
	"strings"
	"testing"
)

// fixedHome replaces HOME so that "~" expansion is deterministic.
func fixedHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", "/home/tester")
}

func TestParseMode(t *testing.T) {
	tests := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"", ModeAsk, false},
		{"ask", ModeAsk, false},
		{"auto-edit", ModeAutoEdit, false},
		{" AUTO-EDIT ", ModeAutoEdit, false},
		{"yolo", ModeYolo, false},
		{"plan", ModePlan, false},
		{"always", ModeAsk, true},
		{"readonly", ModeAsk, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseMode(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseMode(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("ParseMode(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseWebPolicy(t *testing.T) {
	tests := []struct {
		in      string
		want    WebPolicy
		wantErr bool
	}{
		{"", WebPolicyAsk, false},
		{"ask", WebPolicyAsk, false},
		{"allow", WebPolicyAllow, false},
		{"deny", WebPolicyDeny, false},
		{" Deny ", WebPolicyDeny, false},
		{"sometimes", WebPolicyAsk, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseWebPolicy(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseWebPolicy(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("ParseWebPolicy(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestModeRankOrdering(t *testing.T) {
	order := []Mode{ModePlan, ModeAsk, ModeAutoEdit, ModeYolo}
	for i := 1; i < len(order); i++ {
		if order[i-1].Rank() >= order[i].Rank() {
			t.Fatalf("mode %q should rank below %q", order[i-1], order[i])
		}
	}
}

func TestCheckModes(t *testing.T) {
	const workDir = "/work/project"
	tests := []struct {
		name string
		mode Mode
		req  CheckRequest
		want DecisionKind
	}{
		// ask
		{"ask read tool", ModeAsk, CheckRequest{ToolName: "read", IsReadOnly: true, Path: "main.go", WorkDir: workDir}, DecisionAllow},
		{"ask write in workspace", ModeAsk, CheckRequest{ToolName: "write", Path: "main.go", WorkDir: workDir}, DecisionAsk},
		{"ask write outside", ModeAsk, CheckRequest{ToolName: "edit", Path: "/tmp/main.go", WorkDir: workDir}, DecisionAsk},
		{"ask benign bash", ModeAsk, CheckRequest{ToolName: "bash", Command: "npm install", WorkDir: workDir}, DecisionAsk},
		{"ask read-only bash", ModeAsk, CheckRequest{ToolName: "bash", Command: "git status", WorkDir: workDir}, DecisionAllow},
		{"ask web", ModeAsk, CheckRequest{ToolName: "web_fetch", IsReadOnly: true, IsWeb: true, WorkDir: workDir}, DecisionAsk},

		// auto-edit
		{"auto-edit write in workspace", ModeAutoEdit, CheckRequest{ToolName: "write", Path: "src/main.go", WorkDir: workDir}, DecisionAllow},
		{"auto-edit edit absolute in workspace", ModeAutoEdit, CheckRequest{ToolName: "edit", Path: workDir + "/pkg/x.go", WorkDir: workDir}, DecisionAllow},
		{"auto-edit multi_edit in workspace", ModeAutoEdit, CheckRequest{ToolName: "multi_edit", Path: "a.go", WorkDir: workDir}, DecisionAllow},
		{"auto-edit write outside workspace", ModeAutoEdit, CheckRequest{ToolName: "write", Path: "/tmp/x.go", WorkDir: workDir}, DecisionAsk},
		{"auto-edit bash", ModeAutoEdit, CheckRequest{ToolName: "bash", Command: "npm install", WorkDir: workDir}, DecisionAsk},
		{"auto-edit read-only bash", ModeAutoEdit, CheckRequest{ToolName: "bash", Command: "go test ./...", WorkDir: workDir}, DecisionAllow},
		{"auto-edit read tool", ModeAutoEdit, CheckRequest{ToolName: "read", IsReadOnly: true, Path: "/etc/hosts", WorkDir: workDir}, DecisionAllow},

		// yolo
		{"yolo write outside", ModeYolo, CheckRequest{ToolName: "write", Path: "/tmp/x.go", WorkDir: workDir}, DecisionAllow},
		{"yolo bash", ModeYolo, CheckRequest{ToolName: "bash", Command: "npm install", WorkDir: workDir}, DecisionAllow},
		{"yolo web", ModeYolo, CheckRequest{ToolName: "web_search", IsWeb: true, WorkDir: workDir}, DecisionAllow},
		{"yolo destructive bash", ModeYolo, CheckRequest{ToolName: "bash", Command: "rm -rf /", WorkDir: workDir}, DecisionDeny},
		{"yolo sensitive write", ModeYolo, CheckRequest{ToolName: "write", Path: "/etc/passwd", WorkDir: workDir}, DecisionDeny},
		{"yolo denied tool", ModeYolo, CheckRequest{ToolName: "bash", WorkDir: workDir}, DecisionAllow},

		// plan
		{"plan read", ModePlan, CheckRequest{ToolName: "read", IsReadOnly: true, Path: "main.go", WorkDir: workDir}, DecisionAllow},
		{"plan write", ModePlan, CheckRequest{ToolName: "write", Path: "main.go", WorkDir: workDir}, DecisionDeny},
		{"plan bash read-only", ModePlan, CheckRequest{ToolName: "bash", Command: "git log --oneline", WorkDir: workDir}, DecisionAllow},
		{"plan bash mutating", ModePlan, CheckRequest{ToolName: "bash", Command: "touch new.txt", WorkDir: workDir}, DecisionDeny},
		{"plan bash destructive", ModePlan, CheckRequest{ToolName: "bash", Command: "rm -rf /", WorkDir: workDir}, DecisionDeny},
		{"plan web ask", ModePlan, CheckRequest{ToolName: "web_fetch", IsWeb: true, WorkDir: workDir}, DecisionAsk},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := New(Policy{Mode: tt.mode, WorkDir: workDir})
			got := e.Check(tt.req)
			if got.Kind != tt.want {
				t.Fatalf("Check(%+v) = %v, want %v", tt.req, got, tt.want)
			}
			if got.Reason == "" {
				t.Fatalf("Check(%+v) returned an empty reason", tt.req)
			}
		})
	}
}

func TestTaskAlwaysAllowedWithoutPrompt(t *testing.T) {
	const workDir = "/work/project"
	for _, mode := range []Mode{ModeAsk, ModeAutoEdit, ModeYolo, ModePlan} {
		t.Run(string(mode), func(t *testing.T) {
			e := New(Policy{Mode: mode, WorkDir: workDir})
			// The task tool reports itself as mutating, which would otherwise
			// prompt in ask/auto-edit or be denied in plan mode.
			d := e.Check(CheckRequest{ToolName: "task", WorkDir: workDir})
			if !d.IsAllow() {
				t.Fatalf("task in %s mode: got %v, want allow", mode, d)
			}
			if d.Reason == "" {
				t.Fatalf("task in %s mode returned an empty reason", mode)
			}
		})
	}
}

func TestTaskExplicitDenyStillWins(t *testing.T) {
	const workDir = "/work/project"
	for _, mode := range []Mode{ModeAsk, ModeAutoEdit, ModeYolo, ModePlan} {
		for _, deny := range []string{"task", "ta*"} {
			t.Run(string(mode)+"/"+deny, func(t *testing.T) {
				e := New(Policy{Mode: mode, WorkDir: workDir, DenyTools: []string{deny}})
				d := e.Check(CheckRequest{ToolName: "task", WorkDir: workDir})
				if !d.IsDeny() {
					t.Fatalf("task denied by %q in %s mode: got %v, want deny", deny, mode, d)
				}
			})
		}
	}
}

func TestSubAgentPermissionStillEnforced(t *testing.T) {
	const workDir = "/work/project"
	tests := []struct {
		name     string
		parent   Mode
		override string
		forceRO  bool
		denyTool []string
		req      CheckRequest
		want     DecisionKind
	}{
		{
			name: "ask child cannot escalate writes", parent: ModeAsk, override: "yolo",
			req: CheckRequest{ToolName: "write", Path: workDir + "/main.go", WorkDir: workDir}, want: DecisionAsk,
		},
		{
			name: "ask child cannot escalate bash", parent: ModeAsk, override: "yolo",
			req: CheckRequest{ToolName: "bash", Command: "npm install", WorkDir: workDir}, want: DecisionAsk,
		},
		{
			name: "auto-edit child still asks for bash", parent: ModeYolo, override: "auto-edit",
			req: CheckRequest{ToolName: "bash", Command: "npm install", WorkDir: workDir}, want: DecisionAsk,
		},
		{
			name: "auto-edit child allows workspace write", parent: ModeYolo, override: "auto-edit",
			req: CheckRequest{ToolName: "write", Path: workDir + "/main.go", WorkDir: workDir}, want: DecisionAllow,
		},
		{
			name: "read-only child denies write", parent: ModeYolo, forceRO: true,
			req: CheckRequest{ToolName: "write", Path: workDir + "/main.go", WorkDir: workDir}, want: DecisionDeny,
		},
		{
			name: "read-only child denies mutating bash", parent: ModeYolo, forceRO: true,
			req: CheckRequest{ToolName: "bash", Command: "touch x", WorkDir: workDir}, want: DecisionDeny,
		},
		{
			name: "read-only child allows read-only bash", parent: ModeYolo, forceRO: true,
			req: CheckRequest{ToolName: "bash", Command: "git status", WorkDir: workDir}, want: DecisionAllow,
		},
		{
			name: "child inherits task deny", parent: ModeYolo, denyTool: []string{"task"},
			req: CheckRequest{ToolName: "task", WorkDir: workDir}, want: DecisionDeny,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := New(Policy{Mode: tt.parent, WorkDir: workDir, DenyTools: tt.denyTool})
			child := DeriveChild(parent, tt.override, tt.forceRO)
			if d := child.Check(tt.req); d.Kind != tt.want {
				t.Fatalf("child Check(%+v) = %v, want %v", tt.req, d, tt.want)
			}
		})
	}
}

func TestDenyRulesBeatModes(t *testing.T) {
	workDir := t.TempDir()
	for _, mode := range []Mode{ModeAsk, ModeAutoEdit, ModeYolo} {
		t.Run(string(mode), func(t *testing.T) {
			e := New(Policy{
				Mode:       mode,
				WorkDir:    workDir,
				DenyTools:  []string{"rm_*"},
				DenyPaths:  []string{"secrets/**"},
				DenyBash:   []string{"curl *"},
				AllowTools: []string{"rm_*"},
				AllowPaths: []string{"secrets/**"},
				AllowBash:  []string{"curl *"},
			})
			if d := e.Check(CheckRequest{ToolName: "rm_file", WorkDir: workDir}); !d.IsDeny() {
				t.Fatalf("denied tool: got %v", d)
			}
			if d := e.Check(CheckRequest{ToolName: "write", Path: "secrets/api.key", WorkDir: workDir}); !d.IsDeny() {
				t.Fatalf("denied path: got %v", d)
			}
			if d := e.Check(CheckRequest{ToolName: "bash", Command: "curl https://x.test", WorkDir: workDir}); !d.IsDeny() {
				t.Fatalf("denied bash: got %v", d)
			}
		})
	}
}

func TestAllowRules(t *testing.T) {
	tools := New(Policy{AllowTools: []string{"mcp_*"}, Mode: ModeAsk})
	if d := tools.Check(CheckRequest{ToolName: "mcp_github"}); !d.IsAllow() {
		t.Fatalf("allow tool glob: got %v", d)
	}
	if d := tools.Check(CheckRequest{ToolName: "mcp"}); d.IsAllow() {
		t.Fatalf("allow tool glob matched too much: got %v", d)
	}

	bash := New(Policy{AllowBash: []string{"go test*"}, Mode: ModeAsk})
	if d := bash.Check(CheckRequest{ToolName: "bash", Command: "go test ./internal/..."}); !d.IsAllow() {
		t.Fatalf("allow bash glob: got %v", d)
	}
	if d := bash.Check(CheckRequest{ToolName: "bash", Command: "go build ./..."}); !d.IsAsk() {
		t.Fatalf("non-matching bash: got %v", d)
	}

	// A deny inside a segment still wins even if another segment matches an
	// allow rule.
	both := New(Policy{AllowBash: []string{"echo *"}, DenyBash: []string{"rm -rf *"}, Mode: ModeAsk})
	if d := both.Check(CheckRequest{ToolName: "bash", Command: "echo ok && rm -rf /tmp/x"}); !d.IsDeny() {
		t.Fatalf("segment deny should win: got %v", d)
	}
}

func TestDefaultBashDenyRules(t *testing.T) {
	denied := []string{
		"rm -rf /",
		"sudo rm -rf /",
		"rm -fr /",
		"rm -rf ~",
		"rm -rf ~/",
		"rm -rf ~/code",
		"rm -rf $HOME",
		"rm -rf ${HOME}/code",
		"rm -rf /etc",
		"rm -rf /etc/passwd",
		"rm -rf /Users",
		"mkfs.ext4 /dev/sda1",
		"mkfs -t ext4 /dev/sda1",
		"dd if=/dev/zero of=/dev/sda",
		"dd of=/dev/nvme0n1",
		"chmod -R 777 /",
		"chmod -R 777 /usr",
		"shutdown -h now",
		"reboot",
		":(){ :|:& };:",
		"bomb(){ bomb|bomb& }; bomb",
		"echo hi && rm -rf /",
		"echo hi; rm -rf /etc",
		"true || rm -rf /",
		"cat x | rm -rf /",
		"  rm   -rf   /  ",
	}
	for _, cmd := range denied {
		t.Run("deny/"+cmd, func(t *testing.T) {
			if reason, ok := DefaultBashDenyReason(cmd); !ok {
				t.Fatalf("expected %q to be denied", cmd)
			} else if reason == "" {
				t.Fatalf("expected a non-empty reason for %q", cmd)
			}
			d := New(Policy{Mode: ModeYolo}).Check(CheckRequest{ToolName: "bash", Command: cmd})
			if !d.IsDeny() {
				t.Fatalf("yolo Check(%q) = %v, want deny", cmd, d)
			}
		})
	}

	allowed := []string{
		"rm -rf /tmp/build",
		"rm -rf ./build",
		"rm -rf /home/user/project/dist",
		"rm -f /tmp/x",
		"rm file.txt",
		"echo hi",
		"ls -la",
		"git status",
		"chmod 777 file.txt",
		"chmod -R 777 ./dist",
		"go test ./...",
		"dd if=/dev/zero of=/tmp/disk.img",
		"sudo ls /root",
	}
	for _, cmd := range allowed {
		t.Run("allow/"+cmd, func(t *testing.T) {
			if reason, ok := DefaultBashDenyReason(cmd); ok {
				t.Fatalf("expected %q to be allowed, got deny: %s", cmd, reason)
			}
		})
	}
}

func TestDisableDefaultDeny(t *testing.T) {
	e := New(Policy{Mode: ModeYolo, DisableDefaultDeny: true, WorkDir: "/work"})
	if d := e.Check(CheckRequest{ToolName: "bash", Command: "rm -rf /"}); !d.IsAllow() {
		t.Fatalf("disabled default deny: got %v", d)
	}
	if d := e.Check(CheckRequest{ToolName: "write", Path: "/etc/passwd"}); !d.IsAllow() {
		t.Fatalf("disabled default deny for path: got %v", d)
	}
}

func TestSensitiveWritePaths(t *testing.T) {
	fixedHome(t)
	e := New(Policy{Mode: ModeYolo, WorkDir: "/work/project"})
	sensitive := []string{
		"~/.ssh/id_rsa",
		"/home/tester/.ssh/config",
		"~/.gnupg/private.key",
		"~/.aws/credentials",
		"/etc/passwd",
		"/etc/shadow",
		"~/.config/tern/credentials.toml",
		"/etc/sudoers",
	}
	for _, p := range sensitive {
		t.Run("deny/"+p, func(t *testing.T) {
			if d := e.Check(CheckRequest{ToolName: "write", Path: p}); !d.IsDeny() {
				t.Fatalf("write to %q: got %v, want deny", p, d)
			}
			// Reads are always fine.
			if d := e.Check(CheckRequest{ToolName: "read", IsReadOnly: true, Path: p}); !d.IsAllow() {
				t.Fatalf("read of %q: got %v, want allow", p, d)
			}
		})
	}

	allowed := []string{
		"~/.ssh-not-really/id_rsa",
		"/etc/hosts",
		"/tmp/passwd",
		"/work/project/internal/x.go",
	}
	for _, p := range allowed {
		t.Run("allow/"+p, func(t *testing.T) {
			if d := e.Check(CheckRequest{ToolName: "write", Path: p}); !d.IsAllow() {
				t.Fatalf("write to %q: got %v, want allow", p, d)
			}
		})
	}
}

func TestPathGlobs(t *testing.T) {
	fixedHome(t)
	const workDir = "/work/project"
	e := New(Policy{
		Mode:       ModeAsk,
		WorkDir:    workDir,
		AllowPaths: []string{"~/scratch/**", "docs/*.md"},
		DenyPaths:  []string{"vendor/**", "**/*.pem"},
	})

	tests := []struct {
		name string
		req  CheckRequest
		want DecisionKind
	}{
		{"allowed home scratch", CheckRequest{ToolName: "write", Path: "~/scratch/a/b.txt"}, DecisionAllow},
		{"allowed workspace docs", CheckRequest{ToolName: "write", Path: "docs/readme.md"}, DecisionAllow},
		{"docs md nested not matched", CheckRequest{ToolName: "write", Path: "docs/sub/readme.md"}, DecisionAsk},
		{"denied vendor relative", CheckRequest{ToolName: "write", Path: "vendor/lib/x.go"}, DecisionDeny},
		{"denied vendor absolute", CheckRequest{ToolName: "write", Path: workDir + "/vendor/lib/x.go"}, DecisionDeny},
		{"denied pem in subdir", CheckRequest{ToolName: "write", Path: "certs/server.pem"}, DecisionDeny},
		{"unmatched write asks", CheckRequest{ToolName: "write", Path: "src/main.go"}, DecisionAsk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.WorkDir = workDir
			if d := e.Check(tt.req); d.Kind != tt.want {
				t.Fatalf("Check(%+v) = %v, want %v", tt.req, d, tt.want)
			}
		})
	}
}

func TestPathGlobMatching(t *testing.T) {
	fixedHome(t)
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"~/.ssh/*", "/home/tester/.ssh/id_rsa", true},
		{"~/.ssh/*", "/home/tester/.ssh/keys/id_rsa", false},
		{"~/.ssh/**", "/home/tester/.ssh/keys/id_rsa", true},
		{"**/credentials.toml", "/a/b/credentials.toml", true},
		{"**/credentials.toml", "credentials.toml", true},
		{"/etc/passwd", "/etc/passwd", true},
		{"/etc/passwd", "/etc/passwd2", false},
		{"docs/*.md", "docs/a.md", true},
		{"docs/*.md", "docs/sub/a.md", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"|"+tt.path, func(t *testing.T) {
			if got := pathGlobMatch(expandHome(tt.pattern), tt.path); got != tt.want {
				t.Fatalf("pathGlobMatch(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}

func TestWebPolicy(t *testing.T) {
	tests := []struct {
		name string
		mode Mode
		web  WebPolicy
		want DecisionKind
	}{
		{"ask default", ModeAsk, WebPolicyAsk, DecisionAsk},
		{"ask allow", ModeAsk, WebPolicyAllow, DecisionAllow},
		{"ask deny", ModeAsk, WebPolicyDeny, DecisionDeny},
		{"auto-edit ask", ModeAutoEdit, WebPolicyAsk, DecisionAsk},
		{"auto-edit deny", ModeAutoEdit, WebPolicyDeny, DecisionDeny},
		{"auto-edit allow", ModeAutoEdit, WebPolicyAllow, DecisionAllow},
		{"yolo ask", ModeYolo, WebPolicyAsk, DecisionAllow},
		{"yolo deny", ModeYolo, WebPolicyDeny, DecisionDeny},
		{"plan allow", ModePlan, WebPolicyAllow, DecisionAllow},
		{"plan deny", ModePlan, WebPolicyDeny, DecisionDeny},
		{"plan ask", ModePlan, WebPolicyAsk, DecisionAsk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := New(Policy{Mode: tt.mode, Web: tt.web})
			d := e.Check(CheckRequest{ToolName: "web_fetch", IsWeb: true, IsReadOnly: true})
			if d.Kind != tt.want {
				t.Fatalf("Check web = %v, want %v", d, tt.want)
			}
		})
	}
}

func TestDeriveChildNoEscalation(t *testing.T) {
	tests := []struct {
		parent   Mode
		override string
		forceRO  bool
		want     Mode
	}{
		{ModeYolo, "", false, ModeYolo},
		{ModeYolo, "ask", false, ModeAsk},
		{ModeYolo, "auto-edit", false, ModeAutoEdit},
		{ModeYolo, "plan", false, ModePlan},
		{ModeYolo, "yolo", false, ModeYolo},
		{ModeAutoEdit, "yolo", false, ModeAutoEdit},
		{ModeAsk, "yolo", false, ModeAsk},
		{ModePlan, "yolo", false, ModePlan},
		{ModePlan, "", false, ModePlan},
		{ModeYolo, "bogus", false, ModeYolo},
		{ModeYolo, "", true, ModePlan},
		{ModeAsk, "auto-edit", true, ModePlan},
	}
	for _, tt := range tests {
		name := string(tt.parent) + "|" + tt.override
		if tt.forceRO {
			name += "|forceRO"
		}
		t.Run(name, func(t *testing.T) {
			parent := New(Policy{Mode: tt.parent, WorkDir: "/work"})
			child := DeriveChild(parent, tt.override, tt.forceRO)
			if child.Mode() != tt.want {
				t.Fatalf("DeriveChild mode = %q, want %q", child.Mode(), tt.want)
			}
		})
	}
}

func TestDeriveChildBehaviour(t *testing.T) {
	const workDir = "/work/project"
	parent := New(Policy{
		Mode:      ModeYolo,
		Web:       WebPolicyDeny,
		WorkDir:   workDir,
		DenyTools: []string{"danger_*"},
		DenyBash:  []string{"curl *"},
	})

	child := DeriveChild(parent, "auto-edit", false)
	if child.Mode() != ModeAutoEdit {
		t.Fatalf("child mode = %q", child.Mode())
	}
	// Deny rules are inherited.
	if d := child.Check(CheckRequest{ToolName: "danger_delete", WorkDir: workDir}); !d.IsDeny() {
		t.Fatalf("inherited deny tool: got %v", d)
	}
	if d := child.Check(CheckRequest{ToolName: "bash", Command: "curl x", WorkDir: workDir}); !d.IsDeny() {
		t.Fatalf("inherited deny bash: got %v", d)
	}
	// Web policy is inherited.
	if d := child.Check(CheckRequest{ToolName: "web_fetch", IsWeb: true, WorkDir: workDir}); !d.IsDeny() {
		t.Fatalf("inherited web deny: got %v", d)
	}
	// Auto-edit still asks for bash.
	if d := child.Check(CheckRequest{ToolName: "bash", Command: "npm install", WorkDir: workDir}); !d.IsAsk() {
		t.Fatalf("auto-edit child bash: got %v", d)
	}

	// A child of an ask parent cannot be promoted to yolo.
	askParent := New(Policy{Mode: ModeAsk, WorkDir: workDir})
	escalated := DeriveChild(askParent, "yolo", false)
	if d := escalated.Check(CheckRequest{ToolName: "write", Path: "/tmp/x", WorkDir: workDir}); !d.IsAsk() {
		t.Fatalf("child escalated: got %v", d)
	}

	// forceReadOnly children cannot write even when the parent is yolo.
	ro := DeriveChild(parent, "", true)
	if d := ro.Check(CheckRequest{ToolName: "write", Path: workDir + "/main.go", WorkDir: workDir}); !d.IsDeny() {
		t.Fatalf("read-only child write: got %v", d)
	}
	if d := ro.Check(CheckRequest{ToolName: "bash", Command: "touch x", WorkDir: workDir}); !d.IsDeny() {
		t.Fatalf("read-only child mutating bash: got %v", d)
	}
	if d := ro.Check(CheckRequest{ToolName: "bash", Command: "ls", WorkDir: workDir}); !d.IsAllow() {
		t.Fatalf("read-only child read-only bash: got %v", d)
	}

	// Nil parents are safe and default to ask.
	nilChild := DeriveChild(nil, "", false)
	if nilChild.Mode() != ModeAsk {
		t.Fatalf("nil parent mode = %q", nilChild.Mode())
	}
	nilRO := DeriveChild(nil, "", true)
	if nilRO.Mode() != ModePlan {
		t.Fatalf("nil parent read-only mode = %q", nilRO.Mode())
	}
}

func TestDeriveChildDoesNotMutateParent(t *testing.T) {
	parent := New(Policy{Mode: ModeYolo, WorkDir: "/work"})
	_ = DeriveChild(parent, "plan", false)
	if parent.Mode() != ModeYolo {
		t.Fatalf("parent mode mutated to %q", parent.Mode())
	}
}

func TestIsReadOnlyBashCommand(t *testing.T) {
	allowed := []string{
		"git status",
		"git status --short",
		"git diff HEAD",
		"git diff --stat",
		"git log --oneline -5",
		"git show HEAD:main.go",
		"git branch -a",
		"git branch --show-current",
		"git tag -l",
		"git remote -v",
		"git config --get user.name",
		"git -C /repo status",
		"git blame main.go",
		"git ls-files",
		"git stash list",
		"ls -la",
		"ls *.go",
		"cat go.mod",
		"head -n 10 main.go",
		"tail -5 log.txt",
		"wc -l main.go",
		"find . -name '*.go'",
		"rg foo",
		"grep -rn bar .",
		"go test ./...",
		"go test -race ./internal/permission/...",
		"go list ./...",
		"go env GOPATH",
		"go version",
		"go vet ./...",
		"pwd",
		"which go",
		"echo hi",
		"sort file.txt",
		"uniq file.txt",
		"diff a.txt b.txt",
		"sed -n '1,10p' file.txt",
		"jq . package.json",
		"stat main.go",
		"du -sh .",
		"file main.go",
	}
	for _, cmd := range allowed {
		t.Run("allow/"+cmd, func(t *testing.T) {
			if !IsReadOnlyBashCommand(cmd) {
				t.Fatalf("IsReadOnlyBashCommand(%q) = false, want true", cmd)
			}
		})
	}

	rejected := []string{
		"",
		"   ",
		"rm -rf /tmp/x",
		"ls | grep x",
		"ls && rm -rf /",
		"cat a > b",
		"cat a >> b",
		"cat < a",
		"echo $(rm -rf /tmp/x)",
		"echo `whoami`",
		"git commit -m x",
		"git push",
		"git push --force",
		"git checkout main",
		"git switch main",
		"git reset --hard",
		"git clean -fd",
		"git add .",
		"git branch -D foo",
		"git branch new-branch",
		"git tag v1.0.0",
		"git config user.name x",
		"git stash",
		"git log --output=x",
		"go build ./...",
		"go run main.go",
		"go generate ./...",
		"go get example.com/x",
		"go mod tidy",
		"find . -delete",
		"find . -exec rm {} ;",
		"sed -i 's/a/b/' file",
		"sed --in-place 's/a/b/' file",
		"sort -o out.txt in.txt",
		"npm install",
		"touch x",
		"mkdir d",
		"mv a b",
		"cp a b",
		"chmod 777 x",
		"curl https://example.com",
		"tee out.txt",
		"sudo ls",
		"FOO=bar ls",
		"./rm -rf /tmp/x",
	}
	for _, cmd := range rejected {
		t.Run("reject/"+cmd, func(t *testing.T) {
			if IsReadOnlyBashCommand(cmd) {
				t.Fatalf("IsReadOnlyBashCommand(%q) = true, want false", cmd)
			}
		})
	}
}

func TestUnifiedDiffIdentical(t *testing.T) {
	if got := UnifiedDiff("main.go", "a\nb\n", "a\nb\n"); got != "" {
		t.Fatalf("identical diff = %q, want empty", got)
	}
	if got := UnifiedDiff("main.go", "", ""); got != "" {
		t.Fatalf("empty diff = %q, want empty", got)
	}
}

func TestUnifiedDiffSingleLineChange(t *testing.T) {
	old := "line1\nline2\nline3\n"
	newContent := "line1\nLINE2\nline3\n"
	want := "--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1,3 +1,3 @@\n" +
		" line1\n" +
		"-line2\n" +
		"+LINE2\n" +
		" line3\n"
	if got := UnifiedDiff("main.go", old, newContent); got != want {
		t.Fatalf("UnifiedDiff mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestUnifiedDiffInsertion(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\ng\nh\n"
	newContent := "a\nb\nc\nINSERTED\nd\ne\nf\ng\nh\n"
	got := UnifiedDiff("pkg/x.go", old, newContent)
	if !strings.HasPrefix(got, "--- a/pkg/x.go\n+++ b/pkg/x.go\n") {
		t.Fatalf("missing headers in diff:\n%s", got)
	}
	if !strings.Contains(got, "+INSERTED\n") {
		t.Fatalf("missing insertion in diff:\n%s", got)
	}
	if strings.Contains(got, "-c\n") || strings.Contains(got, "-d\n") {
		t.Fatalf("unexpected deletions in diff:\n%s", got)
	}
}

func TestUnifiedDiffDeletion(t *testing.T) {
	old := "a\nb\ngone\nc\nd\n"
	newContent := "a\nb\nc\nd\n"
	got := UnifiedDiff("x.txt", old, newContent)
	if !strings.Contains(got, "-gone\n") {
		t.Fatalf("missing deletion in diff:\n%s", got)
	}
	if strings.Contains(got, "+") {
		// Only the "+++" header line may contain a plus sign.
		for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
			if strings.HasPrefix(line, "+") && line != "+++ b/x.txt" {
				t.Fatalf("unexpected insertion line %q in diff:\n%s", line, got)
			}
		}
	}
}

func TestUnifiedDiffEmptyFiles(t *testing.T) {
	wantNew := "--- a/new.txt\n" +
		"+++ b/new.txt\n" +
		"@@ -0,0 +1,1 @@\n" +
		"+hello\n"
	if got := UnifiedDiff("new.txt", "", "hello\n"); got != wantNew {
		t.Fatalf("new file diff:\n got %q\nwant %q", got, wantNew)
	}

	wantDeleted := "--- a/old.txt\n" +
		"+++ b/old.txt\n" +
		"@@ -1,1 +0,0 @@\n" +
		"-hello\n"
	if got := UnifiedDiff("old.txt", "hello\n", ""); got != wantDeleted {
		t.Fatalf("deleted file diff:\n got %q\nwant %q", got, wantDeleted)
	}
}

func TestUnifiedDiffSeparateHunks(t *testing.T) {
	var oldLines, newLines []string
	for i := 1; i <= 30; i++ {
		line := "line" + strconv.Itoa(i)
		oldLines = append(oldLines, line)
		if i == 2 {
			line = "changed2"
		}
		if i == 28 {
			line = "changed28"
		}
		newLines = append(newLines, line)
	}
	old := strings.Join(oldLines, "\n") + "\n"
	newContent := strings.Join(newLines, "\n") + "\n"
	got := UnifiedDiff("long.txt", old, newContent)
	if n := strings.Count(got, "@@ -"); n != 2 {
		t.Fatalf("hunk count = %d, want 2:\n%s", n, got)
	}
	if !strings.Contains(got, "-line2\n+changed2\n") || !strings.Contains(got, "-line28\n+changed28\n") {
		t.Fatalf("missing changes:\n%s", got)
	}
}

func TestUnifiedDiffNoTrailingNewline(t *testing.T) {
	got := UnifiedDiff("x.txt", "a\nb", "a\nc")
	want := "--- a/x.txt\n" +
		"+++ b/x.txt\n" +
		"@@ -1,2 +1,2 @@\n" +
		" a\n" +
		"-b\n" +
		"+c\n"
	if got != want {
		t.Fatalf("no-newline diff:\n got %q\nwant %q", got, want)
	}
}

func TestUnifiedDiffPathCleaning(t *testing.T) {
	got := UnifiedDiff("./internal/x.go", "a\n", "b\n")
	if !strings.HasPrefix(got, "--- a/internal/x.go\n+++ b/internal/x.go\n") {
		t.Fatalf("path not cleaned:\n%s", got)
	}
	got = UnifiedDiff("", "a\n", "b\n")
	if !strings.HasPrefix(got, "--- a/file\n+++ b/file\n") {
		t.Fatalf("empty path fallback missing:\n%s", got)
	}
}

func TestUnifiedDiffLargeContent(t *testing.T) {
	// Exercises the LCS size guard without asserting on exact output, only
	// that the diff is well-formed.
	var oldB, newB strings.Builder
	for i := 0; i < 3000; i++ {
		oldB.WriteString("old line\n")
		newB.WriteString("new line\n")
	}
	got := UnifiedDiff("big.txt", oldB.String(), newB.String())
	if !strings.HasPrefix(got, "--- a/big.txt\n+++ b/big.txt\n@@ ") {
		t.Fatalf("large diff not well-formed:\n%.200s", got)
	}
	if !strings.Contains(got, "-old line\n+new line\n") {
		t.Fatalf("large diff missing content")
	}
}

func TestCheckerConcurrent(t *testing.T) {
	e := New(Policy{Mode: ModeYolo, WorkDir: "/work"})
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				_ = e.Check(CheckRequest{ToolName: "write", Path: "a.go"})
				_ = IsReadOnlyBashCommand("git status")
				_ = UnifiedDiff("a", "x\n", "y\n")
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
