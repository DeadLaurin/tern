package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Shell execution defaults.
const (
	DefaultBashTimeout = 60 * time.Second
	MaxBashTimeout     = 600 * time.Second
)

// JobStatus describes the lifecycle state of a background job.
type JobStatus string

const (
	JobRunning   JobStatus = "running"
	JobCompleted JobStatus = "completed"
	JobFailed    JobStatus = "failed"
	JobKilled    JobStatus = "killed"
)

// JobSnapshot is a point-in-time view of a background job.
type JobSnapshot struct {
	ID        string
	Command   string
	PID       int
	Status    JobStatus
	ExitCode  int
	Output    string
	StartedAt time.Time
	Duration  time.Duration
}

// Job is a managed background process.
type Job struct {
	id      string
	command string
	started time.Time

	cmd    *exec.Cmd
	pid    int
	done   chan struct{}
	output *boundedBuffer

	mu            sync.Mutex
	status        JobStatus
	exitCode      int
	duration      time.Duration
	killRequested bool
}

func (j *Job) finish(err error) {
	j.mu.Lock()
	j.duration = time.Since(j.started)
	if j.killRequested {
		j.status = JobKilled
		j.exitCode = exitCodeOf(err)
	} else if err != nil {
		j.status = JobFailed
		j.exitCode = exitCodeOf(err)
	} else {
		j.status = JobCompleted
	}
	j.mu.Unlock()
	close(j.done)
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func (j *Job) snapshot() JobSnapshot {
	j.mu.Lock()
	status := j.status
	exitCode := j.exitCode
	duration := j.duration
	if status == JobRunning {
		duration = time.Since(j.started)
	}
	j.mu.Unlock()
	return JobSnapshot{
		ID:        j.id,
		Command:   j.command,
		PID:       j.pid,
		Status:    status,
		ExitCode:  exitCode,
		Output:    j.output.String(),
		StartedAt: j.started,
		Duration:  duration,
	}
}

// boundedBuffer keeps at most max bytes of process output, retaining the most
// recent bytes and reporting how much was dropped. It is safe for concurrent
// writes.
type boundedBuffer struct {
	mu      sync.Mutex
	buf     []byte
	max     int
	dropped int
}

func newBoundedBuffer(max int) *boundedBuffer {
	if max <= 0 {
		max = DefaultOutputCap
	}
	return &boundedBuffer{max: max}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	if n >= b.max {
		b.dropped += len(b.buf) + n - b.max
		b.buf = append(b.buf[:0], p[n-b.max:]...)
		return n, nil
	}
	if len(b.buf)+n > b.max {
		overflow := len(b.buf) + n - b.max
		b.buf = append(b.buf[:0], b.buf[overflow:]...)
		b.dropped += overflow
	}
	b.buf = append(b.buf, p...)
	return n, nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := string(b.buf)
	if b.dropped > 0 {
		s = fmt.Sprintf("[... %d earlier chars dropped ...]\n%s", b.dropped, s)
	}
	return s
}

// JobManager owns the background processes started by the bash tool.
type JobManager struct {
	mu     sync.Mutex
	jobs   map[string]*Job
	seq    int
	closed bool
}

// NewJobManager returns an empty job manager.
func NewJobManager() *JobManager {
	return &JobManager{jobs: make(map[string]*Job)}
}

// Start launches command in a background job and returns its initial snapshot.
func (m *JobManager) Start(shell []string, command, workDir string, maxOutput int) (JobSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return JobSnapshot{}, errors.New("job manager is closed")
	}
	if strings.TrimSpace(command) == "" {
		return JobSnapshot{}, errors.New("command must not be empty")
	}
	m.seq++
	id := fmt.Sprintf("job-%d", m.seq)

	cmd := buildShellCommand(shell, command, workDir)
	configureProcessGroup(cmd)
	job := &Job{
		id:      id,
		command: command,
		started: time.Now(),
		done:    make(chan struct{}),
		status:  JobRunning,
		output:  newBoundedBuffer(maxOutput),
	}
	cmd.Stdout = job.output
	cmd.Stderr = job.output
	if err := cmd.Start(); err != nil {
		return JobSnapshot{}, err
	}
	job.cmd = cmd
	job.pid = cmd.Process.Pid
	m.jobs[id] = job

	go func() {
		job.finish(cmd.Wait())
	}()
	return job.snapshot(), nil
}

// Poll returns the latest snapshot for a job.
func (m *JobManager) Poll(id string) (JobSnapshot, bool) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	m.mu.Unlock()
	if !ok {
		return JobSnapshot{}, false
	}
	return job.snapshot(), true
}

// List returns snapshots for all known jobs in creation order.
func (m *JobManager) List() []JobSnapshot {
	m.mu.Lock()
	jobs := make([]*Job, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, job)
	}
	m.mu.Unlock()

	// Job IDs are monotonically increasing ("job-N"); sort numerically.
	sortJobsByID(jobs)
	out := make([]JobSnapshot, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, job.snapshot())
	}
	return out
}

// Kill terminates a job, first politely and then forcefully, and returns its
// final snapshot.
func (m *JobManager) Kill(id string) (JobSnapshot, error) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	m.mu.Unlock()
	if !ok {
		return JobSnapshot{}, fmt.Errorf("unknown job %q", id)
	}
	terminateJob(job)
	return job.snapshot(), nil
}

func terminateJob(job *Job) {
	select {
	case <-job.done:
		return
	default:
	}
	job.mu.Lock()
	if job.status != JobRunning {
		job.mu.Unlock()
		return
	}
	job.killRequested = true
	job.mu.Unlock()

	_ = signalProcessGroup(job.cmd, false)
	select {
	case <-job.done:
		return
	case <-time.After(2 * time.Second):
	}
	_ = signalProcessGroup(job.cmd, true)
	select {
	case <-job.done:
	case <-time.After(2 * time.Second):
	}
}

// Close kills every running job and waits for its goroutine to finish. It is
// safe to call more than once.
func (m *JobManager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	jobs := make([]*Job, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, job)
	}
	m.mu.Unlock()

	var errs []error
	for _, job := range jobs {
		terminateJob(job)
		select {
		case <-job.done:
		case <-time.After(5 * time.Second):
			errs = append(errs, fmt.Errorf("job %s did not exit", job.id))
		}
	}
	return errors.Join(errs...)
}

// sortJobsByID orders jobs by their numeric suffix.
func sortJobsByID(jobs []*Job) {
	// Insertion sort keeps this dependency-free and is fine for the small
	// number of jobs a session starts.
	for i := 1; i < len(jobs); i++ {
		for j := i; j > 0 && jobOrder(jobs[j], jobs[j-1]); j-- {
			jobs[j], jobs[j-1] = jobs[j-1], jobs[j]
		}
	}
}

func jobOrder(a, b *Job) bool {
	na, oka := jobNumber(a.id)
	nb, okb := jobNumber(b.id)
	if oka && okb {
		return na < nb
	}
	if oka != okb {
		return oka
	}
	return a.id < b.id
}

func jobNumber(id string) (int, bool) {
	const prefix = "job-"
	if !strings.HasPrefix(id, prefix) {
		return 0, false
	}
	n := 0
	if len(id) == len(prefix) {
		return 0, false
	}
	for _, r := range id[len(prefix):] {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

// ---------------------------------------------------------------------------
// bash tool
// ---------------------------------------------------------------------------

// BashTool executes shell commands, foreground or background.
type BashTool struct {
	jobs *JobManager
}

var bashSchema = schema(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "Shell command to execute. Chain dependent steps with && in one call (e.g. \"go build ./... && go test ./...\") instead of separate calls. Prefer the read, grep, glob, edit, and multi_edit tools for inspecting or editing files."},
    "action": {"type": "string", "enum": ["run", "poll", "kill"], "description": "run executes a command (default); poll reads a background job's output and status; kill terminates a background job."},
    "job_id": {"type": "string", "description": "Background job id returned by a run with background=true; required for poll and kill."},
    "background": {"type": "boolean", "description": "Run as a managed background job and return its job_id. Use for dev servers, file watchers, REPLs, or any long-running process; then poll the job instead of waiting for the foreground timeout."},
    "timeout_seconds": {"type": "integer", "description": "Foreground timeout in seconds (default 60, max 600). Only for commands that finish on their own; do not use it to wait on servers or watchers.", "minimum": 1, "maximum": 600}
  },
  "additionalProperties": false
}`)

// NewBashTool returns a bash tool with its own job manager.
func NewBashTool() *BashTool {
	return &BashTool{jobs: NewJobManager()}
}

// NewBashToolWithManager returns a bash tool sharing manager.
func NewBashToolWithManager(manager *JobManager) *BashTool {
	if manager == nil {
		manager = NewJobManager()
	}
	return &BashTool{jobs: manager}
}

// Jobs returns the tool's job manager.
func (t *BashTool) Jobs() *JobManager { return t.jobs }

// Close terminates all background jobs started by this tool.
func (t *BashTool) Close() error { return t.jobs.Close() }

func (t *BashTool) Name() string                { return ToolBash }
func (t *BashTool) IsReadOnly() bool            { return false }
func (t *BashTool) IsWeb() bool                 { return false }
func (t *BashTool) IsParallelSafe() bool        { return false }
func (t *BashTool) Parameters() json.RawMessage { return bashSchema }
func (t *BashTool) Description() string {
	return "Execute a shell command in the workspace. Prefer the native read, grep, glob, edit, and multi_edit tools for inspecting and editing files; use bash for builds, tests, and commands that have no native tool. Chain dependent steps with && in a single call rather than making several round-trips. Pass background=true for dev servers, watchers, and other long-running processes so the foreground call does not block until the timeout, then read output with action=poll and stop it with action=kill using its job_id."
}

type bashArgs struct {
	Command        string `json:"command"`
	Action         string `json:"action"`
	JobID          string `json:"job_id"`
	Background     bool   `json:"background"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// Execute implements Tool.
func (t *BashTool) Execute(ctx context.Context, env ExecEnv, raw json.RawMessage) (Result, error) {
	var args bashArgs
	if err := decodeArgs(raw, &args); err != nil {
		return Result{}, fmt.Errorf("bash: invalid arguments: %w", err)
	}
	action := strings.TrimSpace(args.Action)
	if action == "" {
		action = "run"
	}

	switch action {
	case "run":
		if strings.TrimSpace(args.Command) == "" {
			return Result{}, errors.New(`bash: missing required argument "command"`)
		}
		if args.Background {
			return t.startBackground(env, args)
		}
		return t.runForeground(ctx, env, args)
	case "poll":
		return t.poll(env, args.JobID)
	case "kill":
		return t.kill(env, args.JobID)
	default:
		return Result{}, fmt.Errorf("bash: unknown action %q (want run, poll, or kill)", args.Action)
	}
}

func (t *BashTool) startBackground(env ExecEnv, args bashArgs) (Result, error) {
	snapshot, err := t.jobs.Start(shellSpec(env), args.Command, env.WorkingDir(), env.OutputLimit())
	if err != nil {
		return Result{}, fmt.Errorf("bash: start background job: %w", err)
	}
	out := fmt.Sprintf("started background job %s (pid %d)\ncommand: %s", snapshot.ID, snapshot.PID, snapshot.Command)
	return Result{
		Output: out,
		Metadata: map[string]any{
			"job_id": snapshot.ID,
			"pid":    snapshot.PID,
			"status": string(snapshot.Status),
		},
	}, nil
}

func (t *BashTool) poll(env ExecEnv, jobID string) (Result, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return Result{}, errors.New(`bash: action "poll" requires "job_id"`)
	}
	snapshot, ok := t.jobs.Poll(jobID)
	if !ok {
		return Result{}, fmt.Errorf("bash: unknown job %q", jobID)
	}
	return Result{
		Output: env.Truncate(formatJobSnapshot(snapshot)),
		Metadata: map[string]any{
			"job_id":    snapshot.ID,
			"status":    string(snapshot.Status),
			"exit_code": snapshot.ExitCode,
		},
	}, nil
}

func (t *BashTool) kill(env ExecEnv, jobID string) (Result, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return Result{}, errors.New(`bash: action "kill" requires "job_id"`)
	}
	snapshot, err := t.jobs.Kill(jobID)
	if err != nil {
		return Result{}, fmt.Errorf("bash: %w", err)
	}
	out := formatJobSnapshot(snapshot)
	return Result{
		Output: env.Truncate(out),
		Metadata: map[string]any{
			"job_id":    snapshot.ID,
			"status":    string(snapshot.Status),
			"exit_code": snapshot.ExitCode,
		},
	}, nil
}

// env is unused by poll/kill but kept on the signature for symmetry.
var _ = func(env ExecEnv) {}

func (t *BashTool) runForeground(ctx context.Context, env ExecEnv, args bashArgs) (Result, error) {
	timeout := args.TimeoutSeconds
	if timeout <= 0 {
		timeout = int(DefaultBashTimeout / time.Second)
	}
	maxSeconds := int(MaxBashTimeout / time.Second)
	if timeout > maxSeconds {
		timeout = maxSeconds
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	cmd := buildShellCommandContext(runCtx, shellSpec(env), args.Command, env.WorkingDir())
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second

	// Capture into a bounded buffer so noisy commands never allocate their full
	// output in memory before it is truncated. The child also gets a nil stdin
	// (an empty device) so a command that reads input cannot block forever.
	output := newBoundedBuffer(env.OutputLimit())
	cmd.Stdin = nil
	cmd.Stdout = output
	cmd.Stderr = output

	started := time.Now()
	err := cmd.Run()
	duration := time.Since(started)

	exitCode := 0
	var statusMsg string
	switch {
	case runCtx.Err() != nil && ctx.Err() == nil:
		// The timeout fired, not an outer cancellation.
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			statusMsg = fmt.Sprintf("command timed out after %ds", timeout)
		} else {
			statusMsg = "command cancelled"
		}
		exitCode = -1
	case ctx.Err() != nil:
		statusMsg = "command cancelled"
		exitCode = -1
	case err != nil:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
			statusMsg = fmt.Sprintf("command exited with status %d", exitCode)
		} else {
			return Result{}, fmt.Errorf("bash: %w", err)
		}
	}

	out := output.String()
	if statusMsg != "" {
		out = strings.TrimRight(out, "\n")
		if out != "" {
			out += "\n"
		}
		out += "(" + statusMsg + ")"
	}
	return Result{
		Output:  env.Truncate(out),
		IsError: statusMsg != "",
		Metadata: map[string]any{
			"exit_code":   exitCode,
			"timed_out":   strings.Contains(statusMsg, "timed out"),
			"duration_ms": duration.Milliseconds(),
		},
	}, nil
}

func formatJobSnapshot(s JobSnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "job_id: %s\nstatus: %s\ncommand: %s", s.ID, s.Status, s.Command)
	if s.PID != 0 {
		fmt.Fprintf(&b, "\npid: %d", s.PID)
	}
	if s.Status != JobRunning {
		fmt.Fprintf(&b, "\nexit_code: %d", s.ExitCode)
	}
	b.WriteString("\noutput:")
	if s.Output == "" {
		fmt.Fprintf(&b, " (none yet)")
	} else {
		b.WriteByte('\n')
		b.WriteString(s.Output)
	}
	return b.String()
}

// shellSpec returns the shell executable and its command-prefix arguments.
func shellSpec(env ExecEnv) []string {
	shell := strings.TrimSpace(env.Shell)
	if shell == "" {
		if runtime.GOOS == "windows" {
			return []string{"powershell", "-NoProfile", "-NonInteractive", "-Command"}
		}
		return []string{"sh", "-c"}
	}
	fields := strings.Fields(shell)
	if len(fields) == 0 {
		return []string{"sh", "-c"}
	}
	name := strings.ToLower(filepathBase(fields[0]))
	switch runtime.GOOS {
	case "windows":
		switch name {
		case "cmd", "cmd.exe":
			return append(fields, "/C")
		case "powershell", "powershell.exe", "pwsh", "pwsh.exe":
			return append(fields, "-NoProfile", "-NonInteractive", "-Command")
		default:
			return append(fields, "-c")
		}
	default:
		return append(fields, "-c")
	}
}

func filepathBase(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[i+1:]
	}
	return path
}

func buildShellCommand(shell []string, command, workDir string) *exec.Cmd {
	return buildShellCommandContext(context.Background(), shell, command, workDir)
}

func buildShellCommandContext(ctx context.Context, shell []string, command, workDir string) *exec.Cmd {
	if len(shell) == 0 {
		shell = []string{"sh", "-c"}
	}
	args := make([]string, 0, len(shell)-1+1)
	args = append(args, shell[1:]...)
	args = append(args, command)
	cmd := exec.CommandContext(ctx, shell[0], args...)
	cmd.Dir = workDir
	cmd.Env = nonInteractiveEnv(os.Environ())
	return cmd
}

// nonInteractiveDefaults are environment variables that keep shell commands
// from blocking on an interactive prompt the agent can never answer.
var nonInteractiveDefaults = [][2]string{
	{"GIT_TERMINAL_PROMPT", "0"},
	{"PAGER", "cat"},
	{"GIT_PAGER", "cat"},
	{"CI", "true"},
	{"DEBIAN_FRONTEND", "noninteractive"},
}

// nonInteractiveEnv returns base augmented with non-interactive defaults. An
// entry is only appended when base does not already define its key, so a
// caller-provided value always wins. A nil base means the current process
// environment, matching exec.Cmd's default.
func nonInteractiveEnv(base []string) []string {
	if base == nil {
		base = os.Environ()
	}
	out := make([]string, 0, len(base)+len(nonInteractiveDefaults))
	out = append(out, base...)
	for _, kv := range nonInteractiveDefaults {
		if envHasKey(base, kv[0]) {
			continue
		}
		out = append(out, kv[0]+"="+kv[1])
	}
	return out
}

// envHasKey reports whether env contains an entry for key. Windows environment
// keys are case-insensitive, so the comparison follows suit there.
func envHasKey(env []string, key string) bool {
	for _, entry := range env {
		i := strings.IndexByte(entry, '=')
		if i < 0 {
			continue
		}
		name := entry[:i]
		if name == key {
			return true
		}
		if runtime.GOOS == "windows" && strings.EqualFold(name, key) {
			return true
		}
	}
	return false
}
