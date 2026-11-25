package permission

import (
	"path/filepath"
	"strings"
)

// readOnlyCommands is the allowlist of binaries that only inspect state. Each
// command may still have mutating flags, which are rejected below.
var readOnlyCommands = map[string]bool{
	"basename": true,
	"cal":      true,
	"cat":      true,
	"cmp":      true,
	"comm":     true,
	"cut":      true,
	"date":     true,
	"df":       true,
	"diff":     true,
	"dirname":  true,
	"du":       true,
	"echo":     true,
	"egrep":    true,
	"env":      true,
	"fgrep":    true,
	"file":     true,
	"find":     true,
	"git":      true,
	"go":       true,
	"grep":     true,
	"head":     true,
	"hexdump":  true,
	"id":       true,
	"jq":       true,
	"ls":       true,
	"nl":       true,
	"od":       true,
	"pwd":      true,
	"printf":   true,
	"printenv": true,
	"ps":       true,
	"realpath": true,
	"readlink": true,
	"rg":       true,
	"sed":      true,
	"sort":     true,
	"stat":     true,
	"strings":  true,
	"tail":     true,
	"tr":       true,
	"tree":     true,
	"type":     true,
	"uname":    true,
	"uniq":     true,
	"wc":       true,
	"whereis":  true,
	"which":    true,
	"whoami":   true,
	"yq":       true,
}

// shellMetachars are rejected anywhere in a command because they compose or
// redirect commands into state-changing pipelines.
var shellMetachars = []string{
	"|", "&", ";", ">", "<", "`", "$(", "\n", "\r", "${IFS}",
}

// IsReadOnlyBashCommand reports whether cmd is a single, non-composing shell
// command from a conservative read-only allowlist. It is used by explore/plan
// agents and by the evaluator to auto-allow safe inspection commands.
//
// Pipes, redirects, command substitution, background jobs and command chaining
// are all rejected, as are known mutating subcommands (git commit, go build,
// find -delete, sed -i, sort -o, ...). Anything not recognised is rejected.
func IsReadOnlyBashCommand(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	for _, bad := range shellMetachars {
		if strings.Contains(cmd, bad) {
			return false
		}
	}
	// Built-in destructive rules always win.
	if _, denied := DefaultBashDenyReason(cmd); denied {
		return false
	}

	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	// Reject environment assignments and path prefixes like "./rm".
	first := fields[0]
	if strings.Contains(first, "=") {
		return false
	}
	base := filepath.Base(first)
	if !readOnlyCommands[base] {
		return false
	}
	args := fields[1:]

	// Flags that write files for otherwise read-only commands.
	for _, a := range args {
		switch {
		case a == "--output" || strings.HasPrefix(a, "--output="):
			return false
		case a == "-o":
			switch base {
			case "sort", "go", "git", "grep", "rg":
				return false
			}
		case strings.HasPrefix(a, "-o") && len(a) > 2 && base == "sort":
			return false
		}
	}

	switch base {
	case "find":
		return readOnlyFind(args)
	case "sed":
		return readOnlySed(args)
	case "git":
		return readOnlyGit(args)
	case "go":
		return readOnlyGo(args)
	}
	return true
}

func readOnlyFind(args []string) bool {
	for _, a := range args {
		switch a {
		case "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fls", "-fprint", "-fprint0", "-fprintf":
			return false
		}
	}
	return true
}

func readOnlySed(args []string) bool {
	for _, a := range args {
		if a == "-i" || strings.HasPrefix(a, "-i.") || strings.HasPrefix(a, "--in-place") {
			return false
		}
		if strings.HasPrefix(a, "-i") && len(a) > 2 {
			return false
		}
	}
	return true
}

// readOnlyGit allows only clearly non-mutating subcommands and flag sets.
func readOnlyGit(args []string) bool {
	i := 0
	// Skip global flags, including "-C <dir>" and "-c key=value".
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree":
			i += 2
			continue
		case strings.HasPrefix(a, "-C") || strings.HasPrefix(a, "-c"):
			i++
			continue
		case a == "--no-pager" || a == "--paginate" || a == "--bare" || a == "--no-optional-locks":
			i++
			continue
		case strings.HasPrefix(a, "-"):
			i++
			continue
		}
		break
	}
	if i >= len(args) {
		return false
	}
	sub := args[i]
	rest := args[i+1:]

	switch sub {
	case "status", "diff", "log", "show", "rev-parse", "describe", "blame",
		"ls-files", "ls-tree", "ls-remote", "grep", "shortlog", "show-ref",
		"cat-file", "merge-base", "name-rev", "whatchanged", "reflog":
		return true
	case "branch":
		for _, a := range rest {
			switch a {
			case "-a", "-r", "-v", "-vv", "--list", "--show-current", "--all", "--remotes", "--verbose", "--no-color", "-l":
				continue
			}
			if strings.HasPrefix(a, "-") {
				// Unknown branch flag: assume it may mutate (e.g. -d, -D, -m).
				return false
			}
			return false // positional args mean creation/renaming
		}
		return true
	case "tag":
		for _, a := range rest {
			switch a {
			case "-l", "--list", "-n", "--contains", "--no-contains", "--merged", "--points-at":
				continue
			}
			if strings.HasPrefix(a, "--") || strings.HasPrefix(a, "-") {
				return false
			}
			return false // a positional arg creates a tag
		}
		return true
	case "remote":
		for _, a := range rest {
			switch a {
			case "-v", "--verbose", "show", "get-url":
				continue
			}
			return false
		}
		return true
	case "config":
		// Only explicit reads are allowed: "git config user.name value"
		// writes, while "git config --get user.name" is safe.
		read := false
		for _, a := range rest {
			switch {
			case a == "--get", a == "--get-all", a == "--list", a == "-l",
				a == "--show-origin", a == "--show-scope", a == "--name-only":
				read = true
			case strings.HasPrefix(a, "--get"):
				read = true
			case strings.HasPrefix(a, "-"):
				return false
			default:
				if !read {
					return false
				}
			}
		}
		return read
	case "stash":
		if len(rest) == 0 {
			return false
		}
		return rest[0] == "list" || rest[0] == "show"
	}
	return false
}

// readOnlyGo allows inspection and test subcommands only.
func readOnlyGo(args []string) bool {
	if len(args) == 0 {
		return false
	}
	// Skip leading build flags such as "-C dir".
	sub := args[0]
	for strings.HasPrefix(sub, "-") && len(args) > 1 {
		args = args[1:]
		sub = args[0]
	}
	switch sub {
	case "test", "list", "env", "version", "vet", "doc":
		for _, a := range args[1:] {
			if a == "-o" || strings.HasPrefix(a, "-o=") || a == "-output" {
				return false
			}
			if a == "-c" {
				return false
			}
		}
		return true
	}
	return false
}
