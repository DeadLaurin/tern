package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/tern-agent/tern/internal/config"
)

const defaultUpdateRepo = "DeadLaurin/tern"

var (
	githubAPIBase      = "https://api.github.com"
	githubDownloadBase = "https://github.com"
	// executablePath is a variable so commands can be tested without touching
	// the real tern binary.
	executablePath = os.Executable
)

type updateOptions struct {
	Repo           string
	CurrentVersion string
	GOOS           string
	GOARCH         string
	Client         *http.Client
	APIBase        string
	DownloadBase   string
	TargetPath     string
	Force          bool
}

func printUpdateUsage(w io.Writer) {
	fmt.Fprintln(w, `tern update - update tern to the latest release

Usage:
  tern update [--check] [--force]

Flags:
      --check    report whether an update is available without installing it
      --force    reinstall the latest release even if already up to date
  -h, --help     show this help`)
}

func runUpdateCommand(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("tern update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var checkOnly, force, help bool
	fs.BoolVar(&checkOnly, "check", false, "report whether an update is available without installing it")
	fs.BoolVar(&force, "force", false, "reinstall the latest release even if already up to date")
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUpdateUsage(out)
			return 0
		}
		fmt.Fprintf(errOut, "tern update: %v\n", err)
		return 2
	}
	if help {
		printUpdateUsage(out)
		return 0
	}
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintf(errOut, "tern update: unexpected argument %q\n", rest[0])
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	repo := strings.TrimSpace(os.Getenv("TERN_UPDATE_REPO"))
	if repo == "" {
		repo = defaultUpdateRepo
	}
	opts := updateOptions{
		Repo:           repo,
		CurrentVersion: version,
		GOOS:           runtime.GOOS,
		GOARCH:         runtime.GOARCH,
		Force:          force,
	}

	if checkOnly {
		release, available, err := checkForUpdateDetails(ctx, opts)
		if err != nil {
			fmt.Fprintf(errOut, "tern update: %v\n", err)
			return 1
		}
		if available {
			fmt.Fprintf(out, "Update available: %s (current %s)\n", release.Tag, version)
			printReleaseNotes(out, release.Tag, release.Body)
		} else {
			fmt.Fprintf(out, "tern is already up to date (%s)\n", release.Tag)
		}
		return 0
	}

	fmt.Fprintf(out, "Checking for tern updates from %s...\n", repo)
	applyUpdateDefaults(&opts)
	release, err := latestRelease(ctx, opts.Client, opts.APIBase, opts.Repo)
	if err != nil {
		fmt.Fprintf(errOut, "tern update: %v\n", err)
		return 1
	}
	tag, updated, err := performUpdateFromRelease(ctx, opts, release)
	if err != nil {
		fmt.Fprintf(errOut, "tern update: %v\n", err)
		return 1
	}
	if !updated {
		fmt.Fprintf(out, "tern is already up to date (%s)\n", tag)
		return 0
	}
	fmt.Fprintf(out, "Updated tern to %s. Run tern to see what's new.\n", tag)
	markPendingChangelog(tag)
	return 0
}

// markPendingChangelog records that an update just completed so the TUI shows
// the What's New modal once on its next launch instead of dumping release notes
// into the terminal during `tern update`.
func markPendingChangelog(tag string) {
	configDir, err := config.ConfigDir()
	if err != nil || strings.TrimSpace(configDir) == "" {
		return
	}
	_ = os.MkdirAll(configDir, 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "pending_changelog"), []byte(strings.TrimSpace(tag)+"\n"), 0o600)
}

// releaseNotesMaxLines caps how much of a release body is printed.
const releaseNotesMaxLines = 16

// printReleaseNotes renders a release body under a "What's new" header,
// skipping the block entirely when the body is empty.
func printReleaseNotes(out io.Writer, tag, body string) {
	notes := formatReleaseNotes(body, releaseNotesMaxLines)
	if notes == "" {
		return
	}
	fmt.Fprintf(out, "What's new in %s:\n%s\n", tag, notes)
}

// formatReleaseNotes trims empty edges, collapses repeated blank lines,
// indents the body, and caps it at maxLines (a value <= 0 means no cap).
func formatReleaseNotes(body string, maxLines int) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")

	cleaned := make([]string, 0)
	blank := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			if len(cleaned) == 0 || blank {
				continue
			}
			blank = true
			cleaned = append(cleaned, "")
			continue
		}
		blank = false
		cleaned = append(cleaned, line)
	}
	for len(cleaned) > 0 && cleaned[len(cleaned)-1] == "" {
		cleaned = cleaned[:len(cleaned)-1]
	}

	truncated := false
	if maxLines > 0 && len(cleaned) > maxLines {
		cleaned = cleaned[:maxLines]
		for len(cleaned) > 0 && cleaned[len(cleaned)-1] == "" {
			cleaned = cleaned[:len(cleaned)-1]
		}
		truncated = true
	}
	if len(cleaned) == 0 {
		return ""
	}

	var b strings.Builder
	for _, line := range cleaned {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	if truncated {
		b.WriteString("  ...\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func applyUpdateDefaults(opts *updateOptions) {
	if opts.Repo == "" {
		opts.Repo = defaultUpdateRepo
	}
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
	}
	if opts.GOARCH == "" {
		opts.GOARCH = runtime.GOARCH
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 60 * time.Second}
	}
	if opts.APIBase == "" {
		opts.APIBase = githubAPIBase
	}
	if opts.DownloadBase == "" {
		opts.DownloadBase = githubDownloadBase
	}
}

// checkForUpdate reports the latest release tag and whether it differs from
// the current version without downloading or replacing anything.
func checkForUpdate(ctx context.Context, opts updateOptions) (string, bool, error) {
	release, available, err := checkForUpdateDetails(ctx, opts)
	if err != nil {
		return "", false, err
	}
	return release.Tag, available, nil
}

// checkForUpdateDetails is checkForUpdate plus the release body, so callers can
// show release notes alongside the availability notice.
func checkForUpdateDetails(ctx context.Context, opts updateOptions) (releaseInfo, bool, error) {
	applyUpdateDefaults(&opts)
	release, err := latestRelease(ctx, opts.Client, opts.APIBase, opts.Repo)
	if err != nil {
		return releaseInfo{}, false, err
	}
	available := normalizeVersion(release.Tag) != normalizeVersion(opts.CurrentVersion)
	return release, available, nil
}

func performUpdate(ctx context.Context, opts updateOptions) (string, bool, error) {
	applyUpdateDefaults(&opts)
	release, err := latestRelease(ctx, opts.Client, opts.APIBase, opts.Repo)
	if err != nil {
		return "", false, err
	}
	return performUpdateFromRelease(ctx, opts, release)
}

// performUpdateFromRelease installs an already-fetched release, avoiding a
// second API call when the caller also needs its release notes.
func performUpdateFromRelease(ctx context.Context, opts updateOptions, release releaseInfo) (string, bool, error) {
	tag := release.Tag
	if normalizeVersion(tag) == normalizeVersion(opts.CurrentVersion) && !opts.Force {
		return tag, false, nil
	}

	releaseVersion := strings.TrimPrefix(tag, "v")
	asset := updateAssetName(releaseVersion, opts.GOOS, opts.GOARCH)
	base := fmt.Sprintf("%s/%s/releases/download/%s",
		strings.TrimRight(opts.DownloadBase, "/"), opts.Repo, tag)

	archive, err := download(ctx, opts.Client, base+"/"+asset)
	if err != nil {
		return tag, false, err
	}
	checksums, err := download(ctx, opts.Client, base+"/checksums.txt")
	if err != nil {
		return tag, false, err
	}
	if err := verifyChecksum(asset, archive, checksums); err != nil {
		return tag, false, err
	}
	binary, mode, err := extractBinary(asset, archive, opts.GOOS)
	if err != nil {
		return tag, false, err
	}
	if err := replaceExecutable(opts.TargetPath, binary, mode); err != nil {
		return tag, false, err
	}
	return tag, true, nil
}

func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	return v
}

// isNewerVersion reports whether latest is strictly newer than current using
// major.minor.patch comparison. Non-semver current versions (such as "dev" or
// "test") return false so passive update checks never nag local builds.
func isNewerVersion(latest, current string) bool {
	lParts, lOK := parseSemver(latest)
	cParts, cOK := parseSemver(current)
	if !lOK || !cOK {
		return false
	}
	for i := 0; i < 3; i++ {
		if lParts[i] != cParts[i] {
			return lParts[i] > cParts[i]
		}
	}
	return false
}

func parseSemver(v string) ([3]int, bool) {
	var parts [3]int
	v = normalizeVersion(v)
	if idx := strings.IndexAny(v, "-+"); idx >= 0 {
		v = v[:idx]
	}
	segs := strings.Split(v, ".")
	if len(segs) != 3 {
		return parts, false
	}
	for i, s := range segs {
		if s == "" {
			return parts, false
		}
		n := 0
		for _, ch := range s {
			if ch < '0' || ch > '9' {
				return parts, false
			}
			n = n*10 + int(ch-'0')
		}
		parts[i] = n
	}
	return parts, true
}

func checkUpdateQuick(ctx context.Context, currentVersion string) (string, bool, error) {
	if _, ok := parseSemver(currentVersion); !ok {
		return "", false, nil
	}
	repo := strings.TrimSpace(os.Getenv("TERN_UPDATE_REPO"))
	if repo == "" {
		repo = defaultUpdateRepo
	}
	client := &http.Client{Timeout: 2500 * time.Millisecond}
	tag, err := latestTag(ctx, client, githubAPIBase, repo)
	if err != nil {
		return "", false, err
	}
	return tag, isNewerVersion(tag, currentVersion), nil
}

func runVersionCommand(args []string, o runOptions) int {
	fs := flag.NewFlagSet("tern version", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var noCheck, help bool
	fs.BoolVar(&noCheck, "no-check", false, "skip checking GitHub for newer releases")
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(o.Out, "Usage: tern version [--no-check]")
			return 0
		}
		fmt.Fprintf(o.Err, "tern version: %v\n", err)
		return 2
	}
	if help {
		fmt.Fprintln(o.Out, "Usage: tern version [--no-check]")
		return 0
	}

	fmt.Fprintf(o.Out, "tern %s\n", o.Version)
	if noCheck {
		return 0
	}
	if _, ok := parseSemver(o.Version); !ok {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	tag, available, err := checkUpdateQuick(ctx, o.Version)
	if err != nil || tag == "" {
		return 0
	}
	if available {
		fmt.Fprintf(o.Out, "Update available: %s (run \"tern update\")\n", tag)
	} else {
		fmt.Fprintf(o.Out, "Up to date (%s)\n", tag)
	}
	return 0
}

func updateAssetName(version, goos, goarch string) string {
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("tern_%s_%s_%s.%s", version, goos, goarch, ext)
}

// releaseInfo is the subset of a GitHub release tern needs.
type releaseInfo struct {
	Tag  string
	Body string
}

func latestTag(ctx context.Context, client *http.Client, apiBase, repo string) (string, error) {
	release, err := latestRelease(ctx, client, apiBase, repo)
	if err != nil {
		return "", err
	}
	return release.Tag, nil
}

// latestRelease fetches the latest published release, including its notes.
func latestRelease(ctx context.Context, client *http.Client, apiBase, repo string) (releaseInfo, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", strings.TrimRight(apiBase, "/"), repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return releaseInfo{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "tern-updater")
	resp, err := client.Do(req)
	if err != nil {
		return releaseInfo{}, fmt.Errorf("query latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return releaseInfo{}, fmt.Errorf("query latest release: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var payload struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return releaseInfo{}, fmt.Errorf("decode latest release: %w", err)
	}
	payload.TagName = strings.TrimSpace(payload.TagName)
	if payload.TagName == "" {
		return releaseInfo{}, errors.New("latest release has no tag_name")
	}
	return releaseInfo{Tag: payload.TagName, Body: payload.Body}, nil
}

func download(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tern-updater")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("download %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	return data, nil
}

func verifyChecksum(asset string, data, checksums []byte) error {
	expected, ok := checksumFor(asset, checksums)
	if !ok {
		return fmt.Errorf("checksums.txt has no entry for %s", asset)
	}
	sum := sha256.Sum256(data)
	actual := hex.EncodeToString(sum[:])
	if !strings.EqualFold(expected, actual) {
		return fmt.Errorf("checksum verification failed for %s: expected %s, got %s", asset, expected, actual)
	}
	return nil
}

func checksumFor(asset string, checksums []byte) (string, bool) {
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		if name == asset {
			return strings.ToLower(fields[0]), true
		}
	}
	return "", false
}

func extractBinary(asset string, data []byte, goos string) ([]byte, os.FileMode, error) {
	want := "tern"
	if goos == "windows" {
		want = "tern.exe"
	}
	if strings.HasSuffix(asset, ".zip") {
		return extractZipBinary(data, want)
	}
	return extractTarBinary(data, want)
}

func extractZipBinary(data []byte, want string) ([]byte, os.FileMode, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, 0, fmt.Errorf("open zip: %w", err)
	}
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		if filepath.Base(file.Name) != want || unsafeArchivePath(file.Name) {
			continue
		}
		f, err := file.Open()
		if err != nil {
			return nil, 0, fmt.Errorf("open %s in zip: %w", file.Name, err)
		}
		body, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("read %s in zip: %w", file.Name, err)
		}
		return body, archiveMode(file.Mode()), nil
	}
	return nil, 0, fmt.Errorf("%s not found in archive", want)
}

func extractTarBinary(data []byte, want string) ([]byte, os.FileMode, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, 0, fmt.Errorf("open gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("read tar archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		if filepath.Base(header.Name) != want || unsafeArchivePath(header.Name) {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, 0, fmt.Errorf("read %s from tar: %w", header.Name, err)
		}
		return body, archiveMode(header.FileInfo().Mode()), nil
	}
	return nil, 0, fmt.Errorf("%s not found in archive", want)
}

func unsafeArchivePath(name string) bool {
	clean := filepath.Clean(name)
	return filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func archiveMode(mode os.FileMode) os.FileMode {
	mode = mode.Perm()
	if mode == 0 {
		return 0o755
	}
	return mode
}

// replaceExecutable atomically replaces target (or the running executable)
// with data. Symlinks are resolved first so updating a symlinked install keeps
// the symlink intact and replaces the real binary it points to.
func replaceExecutable(target string, data []byte, mode os.FileMode) error {
	if strings.TrimSpace(target) == "" {
		exe, err := executablePath()
		if err != nil {
			return fmt.Errorf("locate current executable: %w", err)
		}
		target = exe
	}
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	if mode == 0 {
		mode = 0o755
	}
	if info, err := os.Stat(target); err == nil {
		if info.Mode().Perm() != 0 {
			mode = info.Mode().Perm()
		}
	}

	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".tern-update-*")
	if err != nil {
		return fmt.Errorf("create temporary executable next to %s: %w", target, err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, target); err != nil {
		// Windows may refuse to replace a running executable via rename.
		// Move the old binary aside and retry once.
		backup := target + ".old"
		if removeErr := os.Remove(backup); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("replace executable: %w", err)
		}
		if moveErr := os.Rename(target, backup); moveErr != nil {
			return fmt.Errorf("replace executable: %w", err)
		}
		if retryErr := os.Rename(tmpPath, target); retryErr != nil {
			_ = os.Rename(backup, target)
			return fmt.Errorf("replace executable: %w", retryErr)
		}
		_ = os.Remove(backup)
	}
	cleanup = false
	return nil
}
