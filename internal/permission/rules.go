package permission

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// DefaultDenyWritePaths are path globs that may never be written to, even in
// yolo mode, unless Policy.DisableDefaultDeny is set. Reading them is still
// permitted.
var DefaultDenyWritePaths = []string{
	"~/.ssh/*",
	"~/.ssh/**",
	"~/.gnupg/*",
	"~/.gnupg/**",
	"~/.aws/credentials",
	"~/.aws/config",
	"~/.netrc",
	"~/.docker/config.json",
	"~/.kube/config",
	"~/.config/gh/hosts.yml",
	"~/.config/tern/credentials.toml",
	"/etc/passwd",
	"/etc/shadow",
	"/etc/sudoers",
	"/etc/sudoers.d/**",
	"/etc/ssh/**",
}

// DefaultDenyBashPatterns are command globs that are always denied unless
// Policy.DisableDefaultDeny is set. Each pattern is matched against individual
// command segments (split on ;, &&, ||, | and newlines) with leading "sudo"
// removed and whitespace collapsed.
//
// "*" matches any sequence of characters, including spaces and slashes, so
// these patterns describe whole commands rather than paths.
var DefaultDenyBashPatterns = []string{
	"rm -rf /",
	"rm -fr /",
	"rm -rf ~*",
	"rm -fr ~*",
	"rm -rf $HOME*",
	"rm -rf ${HOME}*",
	"mkfs*",
	"mkfs.*",
	"wipefs*",
	"dd if=* of=/dev/*",
	"dd of=/dev/*",
	"chmod -R 777 /",
	"chmod -R 777 /*",
	"chmod 777 -R /*",
	"chown -R * /*",
	"shutdown*",
	"reboot*",
	"halt*",
	"poweroff*",
	"> /dev/sd*",
	"> /dev/disk*",
	"> /dev/nvme*",
}

// DefaultBashDenyReason checks a shell command against the built-in
// destructive-command rules. It returns a human-readable reason when the
// command must be denied.
func DefaultBashDenyReason(command string) (string, bool) {
	// Fork bombs contain their own separators (";", "|", "&"), so inspect the
	// whole command before splitting it into segments.
	if isForkBomb(collapseSpaces(stripCommandPrefixes(command))) {
		return fmt.Sprintf("fork bomb pattern detected in %q", collapseSpaces(command)), true
	}
	for _, seg := range shellSegments(command) {
		if reason, ok := segmentDenyReason(seg); ok {
			return reason, true
		}
	}
	return "", false
}

func segmentDenyReason(seg string) (string, bool) {
	norm := collapseSpaces(seg)
	if norm == "" {
		return "", false
	}
	base := stripCommandPrefixes(norm)
	if isForkBomb(base) {
		return fmt.Sprintf("fork bomb pattern detected in %q", norm), true
	}
	for _, pat := range DefaultDenyBashPatterns {
		if wildcardMatch(pat, base) {
			return fmt.Sprintf("destructive command %q matches built-in rule %q", norm, pat), true
		}
	}
	if target, ok := dangerousRMTarget(base); ok {
		return fmt.Sprintf("refusing recursive forced delete of protected path %q", target), true
	}
	return "", false
}

// protectedRoots are paths whose recursive forced deletion is always denied.
var protectedRoots = map[string]bool{
	"":              true, // "/" after trimming
	"/":             true,
	"*":             true,
	"~":             true,
	"$HOME":         true,
	"${HOME}":       true,
	"~/*":           true,
	"$HOME/*":       true,
	"${HOME}/*":     true,
	"/bin":          true,
	"/boot":         true,
	"/dev":          true,
	"/etc":          true,
	"/home":         true,
	"/lib":          true,
	"/lib64":        true,
	"/opt":          true,
	"/proc":         true,
	"/root":         true,
	"/sbin":         true,
	"/sys":          true,
	"/System":       true,
	"/Library":      true,
	"/Users":        true,
	"/usr":          true,
	"/var":          true,
	"/Applications": true,
}

// protectedPrefixes are top-level directories where any recursive forced
// delete is denied, including nested paths such as /etc/passwd.
var protectedPrefixes = []string{
	"/bin", "/boot", "/dev", "/etc", "/lib", "/lib64",
	"/proc", "/root", "/sbin", "/sys", "/usr", "/var",
	"/System", "/Library",
}

// dangerousRMTarget detects `rm -rf <protected>` style commands. The check is
// structural rather than a loose glob so that `rm -rf /tmp/build` stays legal
// while `rm -rf /` and `rm -rf ~` are denied.
func dangerousRMTarget(norm string) (string, bool) {
	fields := strings.Fields(norm)
	if len(fields) < 2 || filepath.Base(fields[0]) != "rm" {
		return "", false
	}
	recursive, force := false, false
	var targets []string
	for _, f := range fields[1:] {
		switch {
		case strings.HasPrefix(f, "--recursive"):
			recursive = true
		case strings.HasPrefix(f, "--force"):
			force = true
		case strings.HasPrefix(f, "--"):
			// Other long options do not affect recursion/force.
		case strings.HasPrefix(f, "-") && f != "-":
			if strings.ContainsAny(f, "rR") {
				recursive = true
			}
			if strings.Contains(f, "f") {
				force = true
			}
		default:
			targets = append(targets, f)
		}
	}
	if !recursive || !force {
		return "", false
	}
	for _, t := range targets {
		if isProtectedTarget(t) {
			return t, true
		}
	}
	return "", false
}

func isProtectedTarget(raw string) bool {
	t := strings.Trim(strings.TrimSpace(raw), `"'`)
	if t == "" {
		return false
	}
	trimmed := strings.TrimRight(t, "/")
	if trimmed == "" {
		// "", "/", "//" and friends all mean the filesystem root.
		return true
	}
	if protectedRoots[trimmed] {
		return true
	}
	// A top-level directory glob, e.g. /etc/* or /usr/*.
	if strings.HasSuffix(trimmed, "/*") {
		return protectedRoots[strings.TrimSuffix(trimmed, "/*")]
	}
	// Anything inside a critical system directory.
	if strings.HasPrefix(trimmed, "/") {
		for _, prefix := range protectedPrefixes {
			if trimmed == prefix || strings.HasPrefix(trimmed, prefix+"/") {
				return true
			}
		}
	}
	return false
}

// isForkBomb recognises the classic shell fork bomb and its close variants,
// e.g. ":(){ :|:& };:".
func isForkBomb(norm string) bool {
	compact := strings.ReplaceAll(norm, " ", "")
	if !strings.Contains(compact, "(){") {
		return false
	}
	if !strings.Contains(compact, "|") || !strings.Contains(compact, "&") {
		return false
	}
	return strings.Contains(compact, ";:") || strings.Contains(compact, "};") || strings.Contains(compact, "&}")
}

// shellSegments splits a shell command into independently executed segments.
func shellSegments(command string) []string {
	replacer := strings.NewReplacer(
		"&&", "\x00",
		"||", "\x00",
		";", "\x00",
		"|", "\x00",
		"\n", "\x00",
		"\r", "\x00",
	)
	parts := strings.Split(replacer.Replace(command), "\x00")
	segs := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(p), "({"))
		p = strings.TrimRight(p, ")")
		if p != "" {
			segs = append(segs, p)
		}
	}
	return segs
}

// stripCommandPrefixes removes wrappers that do not change the effective
// command: leading sudo/env/command/nohup/time and VAR=value assignments.
func stripCommandPrefixes(norm string) string {
	fields := strings.Fields(norm)
	for len(fields) > 0 {
		switch filepath.Base(fields[0]) {
		case "sudo", "command", "env", "nohup", "time", "exec", "builtin":
			fields = fields[1:]
			// Skip sudo/env flags.
			for len(fields) > 0 && strings.HasPrefix(fields[0], "-") {
				fields = fields[1:]
			}
			continue
		}
		if strings.Contains(fields[0], "=") && !strings.HasPrefix(fields[0], "=") {
			fields = fields[1:]
			continue
		}
		break
	}
	return strings.Join(fields, " ")
}

func collapseSpaces(s string) string { return strings.Join(strings.Fields(s), " ") }

// --- glob matching -------------------------------------------------------

// matchAnyGlob matches s against shell-style patterns where "*" and "?" may
// match any character. It returns the matching pattern.
func matchAnyGlob(patterns []string, s string) (string, bool) {
	for _, pat := range patterns {
		if wildcardMatch(pat, s) {
			return pat, true
		}
	}
	return "", false
}

// wildcardMatch matches s against pattern where "*" matches any sequence and
// "?" matches exactly one character. Matching is case-sensitive.
func wildcardMatch(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	if !strings.ContainsAny(pattern, "*?") {
		return pattern == s
	}
	return regexpFor("bash", pattern, false).MatchString(s)
}

// matchAnyBash matches a whole command against bash glob patterns, considering
// each shell segment as well as the wrapped forms with sudo/env prefixes
// removed.
func matchAnyBash(patterns []string, command string) (string, bool) {
	if len(patterns) == 0 {
		return "", false
	}
	candidates := append([]string{collapseSpaces(command)}, shellSegments(command)...)
	for i := range candidates {
		candidates[i] = stripCommandPrefixes(candidates[i])
	}
	for _, pat := range patterns {
		for _, cand := range candidates {
			if cand == "" {
				continue
			}
			if wildcardMatch(pat, cand) {
				return pat, true
			}
		}
	}
	return "", false
}

// matchAnyPath matches a path against path globs, resolving "~" and relative
// paths against workDir. It returns the matching pattern.
func matchAnyPath(patterns []string, path, workDir string) (string, bool) {
	if len(patterns) == 0 {
		return "", false
	}
	candidates := pathCandidates(path, workDir)
	for _, pat := range patterns {
		expanded := expandHome(pat)
		for _, cand := range candidates {
			if pathGlobMatch(expanded, cand) {
				return pat, true
			}
		}
	}
	return "", false
}

func isPathAbs(p string) bool {
	return filepath.IsAbs(p) || strings.HasPrefix(p, "/")
}

// pathCandidates returns the cleaned slash-normalized forms of path that path
// globs are matched against: as given (with ~ expanded), absolute, and
// workspace-relative.
func pathCandidates(path, workDir string) []string {
	p := filepath.ToSlash(filepath.Clean(expandHome(strings.TrimSpace(path))))
	wd := ""
	if strings.TrimSpace(workDir) != "" {
		wd = filepath.ToSlash(filepath.Clean(workDir))
	}
	var out []string
	add := func(v string) {
		v = filepath.ToSlash(filepath.Clean(v))
		for _, existing := range out {
			if existing == v {
				return
			}
		}
		out = append(out, v)
	}
	add(p)
	if isPathAbs(p) {
		if wd != "" {
			if rel, err := filepath.Rel(filepath.FromSlash(wd), filepath.FromSlash(p)); err == nil {
				add(rel)
			}
		}
		return out
	}
	if wd != "" {
		add(wd + "/" + p)
	}
	return out
}

// pathGlobMatch matches a path against a glob where "*" does not cross path
// separators and "**" does.
func pathGlobMatch(pattern, path string) bool {
	pattern = filepath.ToSlash(pattern)
	path = filepath.ToSlash(path)
	if pattern == path {
		return true
	}
	if !strings.ContainsAny(pattern, "*?") {
		return false
	}
	return regexpFor("path", pattern, true).MatchString(path)
}

var (
	regexpCacheMu sync.RWMutex
	regexpCache   = map[string]*regexp.Regexp{}
)

func regexpFor(kind, pattern string, pathMode bool) *regexp.Regexp {
	key := kind + "\x00" + pattern
	regexpCacheMu.RLock()
	re, ok := regexpCache[key]
	regexpCacheMu.RUnlock()
	if ok {
		return re
	}
	var src string
	if pathMode {
		src = pathGlobSource(pattern)
	} else {
		src = wildcardSource(pattern)
	}
	re = regexp.MustCompile(src)
	regexpCacheMu.Lock()
	regexpCache[key] = re
	regexpCacheMu.Unlock()
	return re
}

func wildcardSource(pattern string) string {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return b.String()
}

func pathGlobSource(pattern string) string {
	rs := []rune(pattern)
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '*':
			if i+1 < len(rs) && rs[i+1] == '*' {
				for i+1 < len(rs) && rs[i+1] == '*' {
					i++
				}
				if i+1 < len(rs) && rs[i+1] == '/' {
					// "**/" optionally matches zero directories.
					b.WriteString("(?:.*/)?")
					i++
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(rs[i])))
		}
	}
	b.WriteString("$")
	return b.String()
}

// expandHome expands a leading "~" using the current user's home directory.
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}
