package context

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// InstallSkills installs one or more Claude / Agent Skills from a web URL,
// GitHub repository/tree/blob URL, GitHub shorthand (owner/repo[/subdir]), or
// local file/directory into targetRoot (typically ~/.config/tern/skills or
// ./.tern/skills).
func InstallSkills(ctx context.Context, targetRoot, source, nameOverride string, client *http.Client) ([]SkillMeta, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, errors.New("context: skill source is required")
	}
	if strings.TrimSpace(targetRoot) == "" {
		return nil, errors.New("context: skill target directory is required")
	}
	if err := os.MkdirAll(targetRoot, 0o755); err != nil {
		return nil, fmt.Errorf("context: create skills dir: %w", err)
	}

	// 1. Local file or directory?
	if !strings.HasPrefix(source, "http://") && !strings.HasPrefix(source, "https://") && !strings.HasPrefix(source, "git@") {
		if info, err := os.Stat(source); err == nil {
			if info.IsDir() {
				return installFromDir(targetRoot, source, nameOverride)
			}
			return installFromSingleFile(targetRoot, source, nameOverride)
		}
	}

	// 2. GitHub blob URL -> raw markdown download.
	if rawURL, inferredName, ok := githubBlobToRaw(source); ok {
		if nameOverride == "" {
			nameOverride = inferredName
		}
		return installFromURL(ctx, targetRoot, rawURL, nameOverride, client)
	}

	// 3. GitHub repo / tree URL or shorthand (owner/repo[/subdir]) -> shallow git clone.
	if repoURL, branch, subdir, ok := parseGitHubRepoSource(source); ok {
		return installFromGit(ctx, targetRoot, repoURL, branch, subdir, nameOverride)
	}

	// 4. Direct HTTP(S) URL to a SKILL.md / markdown file.
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		if strings.HasSuffix(strings.ToLower(source), ".git") {
			return installFromGit(ctx, targetRoot, source, "", "", nameOverride)
		}
		return installFromURL(ctx, targetRoot, source, nameOverride, client)
	}

	// 5. SSH git URL.
	if strings.HasPrefix(source, "git@") {
		return installFromGit(ctx, targetRoot, source, "", "", nameOverride)
	}

	return nil, fmt.Errorf("context: unrecognized skill source %q (expected a URL, GitHub owner/repo, or local path)", source)
}

// RemoveSkill deletes an installed skill from targetRoot (either <name>/ or <name>.md).
func RemoveSkill(targetRoot, name string) error {
	name = sanitizeSkillName(name)
	if name == "" {
		return errors.New("context: invalid skill name")
	}
	dirPath := filepath.Join(targetRoot, name)
	filePath := filepath.Join(targetRoot, name+".md")
	removed := false
	if _, err := os.Lstat(dirPath); err == nil {
		if err := os.RemoveAll(dirPath); err != nil {
			return fmt.Errorf("context: remove skill %q: %w", name, err)
		}
		removed = true
	}
	if _, err := os.Lstat(filePath); err == nil {
		if err := os.Remove(filePath); err != nil {
			return fmt.Errorf("context: remove skill %q: %w", name, err)
		}
		removed = true
	}
	if !removed {
		return fmt.Errorf("%w: %q", ErrSkillNotFound, name)
	}
	return nil
}

func githubBlobToRaw(raw string) (rawURL, inferredName string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Host, "github.com") {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	// /owner/repo/blob/<branch>/<path...>
	if len(parts) < 5 || parts[2] != "blob" {
		return "", "", false
	}
	owner, repo, branch := parts[0], parts[1], parts[3]
	fileParts := parts[4:]
	rawURL = fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", owner, repo, branch, strings.Join(fileParts, "/"))
	last := fileParts[len(fileParts)-1]
	if strings.EqualFold(last, "SKILL.md") && len(fileParts) >= 2 {
		inferredName = fileParts[len(fileParts)-2]
	} else {
		inferredName = strings.TrimSuffix(last, filepath.Ext(last))
	}
	return rawURL, inferredName, true
}

var githubShorthandRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(?:/.*)?$`)

func parseGitHubRepoSource(raw string) (repoURL, branch, subdir string, ok bool) {
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		u, err := url.Parse(raw)
		if err != nil || !strings.EqualFold(u.Host, "github.com") {
			return "", "", "", false
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) < 2 {
			return "", "", "", false
		}
		owner := parts[0]
		repo := strings.TrimSuffix(parts[1], ".git")
		repoURL = fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
		if len(parts) >= 4 && parts[2] == "tree" {
			branch = parts[3]
			if len(parts) > 4 {
				subdir = strings.Join(parts[4:], "/")
			}
		} else if len(parts) > 2 {
			subdir = strings.Join(parts[2:], "/")
		}
		return repoURL, branch, subdir, true
	}
	if githubShorthandRE.MatchString(raw) {
		parts := strings.Split(strings.Trim(raw, "/"), "/")
		owner, repo := parts[0], strings.TrimSuffix(parts[1], ".git")
		repoURL = fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
		if len(parts) > 2 {
			subdir = strings.Join(parts[2:], "/")
		}
		return repoURL, "", subdir, true
	}
	return "", "", "", false
}

func installFromURL(ctx context.Context, targetRoot, rawURL, nameOverride string, client *http.Client) ([]SkillMeta, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("context: build skill request: %w", err)
	}
	req.Header.Set("User-Agent", "tern-skill-installer")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("context: fetch skill %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("context: fetch skill %s: HTTP %d", rawURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("context: read skill body: %w", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, fmt.Errorf("context: skill at %s is empty", rawURL)
	}

	fm, _ := splitFrontMatter(string(body))
	name := strings.TrimSpace(nameOverride)
	if name == "" {
		name = strings.TrimSpace(fm.Name)
	}
	if name == "" {
		if u, uerr := url.Parse(rawURL); uerr == nil {
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(parts) > 0 {
				last := parts[len(parts)-1]
				if strings.EqualFold(last, "SKILL.md") && len(parts) >= 2 {
					name = parts[len(parts)-2]
				} else {
					name = strings.TrimSuffix(last, filepath.Ext(last))
				}
			}
		}
	}
	name = sanitizeSkillName(name)
	if name == "" {
		return nil, errors.New("context: could not determine skill name; pass --name <name>")
	}

	destDir := filepath.Join(targetRoot, name)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("context: create skill dir: %w", err)
	}
	destFile := filepath.Join(destDir, "SKILL.md")
	if err := os.WriteFile(destFile, body, 0o644); err != nil {
		return nil, fmt.Errorf("context: write skill: %w", err)
	}
	return []SkillMeta{{
		Name:        name,
		Description: fm.Description,
		Tags:        fm.Tags,
		Path:        destFile,
	}}, nil
}

func installFromGit(ctx context.Context, targetRoot, repoURL, branch, subdir, nameOverride string) ([]SkillMeta, error) {
	tmpDir, err := os.MkdirTemp("", "tern-skill-*")
	if err != nil {
		return nil, fmt.Errorf("context: create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	args := []string{"clone", "--depth", "1"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, repoURL, tmpDir)
	cmd := exec.CommandContext(ctx, "git", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("context: git clone %s: %w (%s)", repoURL, err, strings.TrimSpace(string(out)))
	}

	srcDir := tmpDir
	if subdir != "" {
		srcDir = filepath.Join(tmpDir, filepath.FromSlash(subdir))
		info, err := os.Stat(srcDir)
		if err != nil {
			return nil, fmt.Errorf("context: path %q not found in %s", subdir, repoURL)
		}
		if !info.IsDir() {
			return installFromSingleFile(targetRoot, srcDir, nameOverride)
		}
	}
	return installFromDir(targetRoot, srcDir, nameOverride)
}

func installFromSingleFile(targetRoot, srcFile, nameOverride string) ([]SkillMeta, error) {
	data, err := os.ReadFile(srcFile)
	if err != nil {
		return nil, fmt.Errorf("context: read %s: %w", srcFile, err)
	}
	fm, _ := splitFrontMatter(string(data))
	name := strings.TrimSpace(nameOverride)
	if name == "" {
		name = strings.TrimSpace(fm.Name)
	}
	if name == "" {
		base := filepath.Base(srcFile)
		if strings.EqualFold(base, "SKILL.md") {
			name = filepath.Base(filepath.Dir(srcFile))
		} else {
			name = strings.TrimSuffix(base, filepath.Ext(base))
		}
	}
	name = sanitizeSkillName(name)
	if name == "" {
		return nil, errors.New("context: could not determine skill name; pass --name <name>")
	}
	destDir := filepath.Join(targetRoot, name)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("context: create skill dir: %w", err)
	}
	destFile := filepath.Join(destDir, "SKILL.md")
	if err := os.WriteFile(destFile, data, 0o644); err != nil {
		return nil, fmt.Errorf("context: write skill: %w", err)
	}
	return []SkillMeta{{
		Name:        name,
		Description: fm.Description,
		Tags:        fm.Tags,
		Path:        destFile,
	}}, nil
}

func installFromDir(targetRoot, srcDir, nameOverride string) ([]SkillMeta, error) {
	// If srcDir itself is a skill directory (contains SKILL.md), install it directly.
	if info, err := os.Stat(filepath.Join(srcDir, "SKILL.md")); err == nil && !info.IsDir() {
		return installSkillFolder(targetRoot, srcDir, nameOverride)
	}

	// Otherwise scan standard skill subtrees inside the repo/directory.
	var skillDirs []string
	seen := make(map[string]struct{})
	err := filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "node_modules" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(d.Name(), "SKILL.md") {
			dir := filepath.Dir(p)
			if _, ok := seen[dir]; !ok {
				seen[dir] = struct{}{}
				skillDirs = append(skillDirs, dir)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(skillDirs) == 0 {
		return nil, fmt.Errorf("context: no SKILL.md found in %s", srcDir)
	}
	sort.Strings(skillDirs)
	if nameOverride != "" && len(skillDirs) > 1 {
		return nil, fmt.Errorf("context: --name can only be used when installing a single skill (found %d skills)", len(skillDirs))
	}
	var installed []SkillMeta
	for _, dir := range skillDirs {
		override := ""
		if len(skillDirs) == 1 {
			override = nameOverride
		}
		metas, err := installSkillFolder(targetRoot, dir, override)
		if err != nil {
			return installed, err
		}
		installed = append(installed, metas...)
	}
	return installed, nil
}

func installSkillFolder(targetRoot, skillDir, nameOverride string) ([]SkillMeta, error) {
	skillPath := filepath.Join(skillDir, "SKILL.md")
	data, err := os.ReadFile(skillPath)
	if err != nil {
		return nil, fmt.Errorf("context: read %s: %w", skillPath, err)
	}
	fm, _ := splitFrontMatter(string(data))
	name := strings.TrimSpace(nameOverride)
	if name == "" {
		name = strings.TrimSpace(fm.Name)
	}
	if name == "" {
		name = filepath.Base(skillDir)
	}
	name = sanitizeSkillName(name)
	if name == "" {
		return nil, errors.New("context: could not determine skill name")
	}

	destDir := filepath.Join(targetRoot, name)
	if err := os.RemoveAll(destDir); err != nil {
		return nil, fmt.Errorf("context: replace existing skill %q: %w", name, err)
	}
	if err := copyDirExcludingGit(skillDir, destDir); err != nil {
		return nil, fmt.Errorf("context: copy skill %q: %w", name, err)
	}
	return []SkillMeta{{
		Name:        name,
		Description: fm.Description,
		Tags:        fm.Tags,
		Path:        filepath.Join(destDir, "SKILL.md"),
	}}, nil
}

func copyDirExcludingGit(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func sanitizeSkillName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ToLower(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "." || out == ".." {
		return ""
	}
	return out
}

func isPathInside(path, parent string) bool {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(parent) == "" {
		return false
	}
	p := filepath.Clean(path)
	root := filepath.Clean(parent)
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+string(filepath.Separator))
}
