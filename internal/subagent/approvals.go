package subagent

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrNoApprover is returned when an approval is required but no approver is
// configured. The call is denied.
var ErrNoApprover = errors.New("subagent: no approval handler configured")

// ApprovalRequest is one permission question raised by a sub-agent. The UI
// renders a single prompt at a time; these labels make it unambiguous which
// agent asked what.
type ApprovalRequest struct {
	AgentID         string
	AgentType       string
	TaskDescription string
	ToolName        string
	Detail          string
	DiffPreview     string
	Reason          string
}

// Approver decides one approval request. Implementations may block while the
// user responds but must honour ctx.
type Approver func(ctx context.Context, req ApprovalRequest) (bool, error)

// ApprovalQueue serializes approval prompts from concurrent sub-agents so the
// UI never shows two overlapping questions.
type ApprovalQueue struct {
	token   chan struct{}
	mu      sync.RWMutex
	approve Approver
	onAsk   func(ApprovalRequest)
	pending int
}

// NewApprovalQueue returns a queue. The optional approver can also be supplied
// later with SetApprover.
func NewApprovalQueue(approvers ...Approver) *ApprovalQueue {
	q := &ApprovalQueue{token: make(chan struct{}, 1)}
	q.token <- struct{}{}
	if len(approvers) > 0 {
		q.approve = approvers[0]
	}
	return q
}

// SetApprover installs or replaces the approval handler.
func (q *ApprovalQueue) SetApprover(a Approver) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.approve = a
}

// SetOnAsk installs an observer called immediately before the approver runs.
// It is used to emit subagent_approval events.
func (q *ApprovalQueue) SetOnAsk(fn func(ApprovalRequest)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.onAsk = fn
}

// Pending returns the number of approvals waiting for their turn.
func (q *ApprovalQueue) Pending() int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.pending
}

// Ask enqueues req and blocks until it is answered or ctx is done. It returns
// ErrNoApprover (denying the call) when no handler is installed.
func (q *ApprovalQueue) Ask(ctx context.Context, req ApprovalRequest) (bool, error) {
	if q == nil {
		return false, ErrNoApprover
	}
	if ctx == nil {
		ctx = context.Background()
	}

	q.mu.Lock()
	q.pending++
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		q.pending--
		q.mu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-q.token:
	}
	defer func() { q.token <- struct{}{} }()

	if err := ctx.Err(); err != nil {
		return false, err
	}

	q.mu.RLock()
	approve := q.approve
	onAsk := q.onAsk
	q.mu.RUnlock()
	if onAsk != nil {
		onAsk(req)
	}
	if approve == nil {
		return false, ErrNoApprover
	}

	ok, err := approve(ctx, req)
	if err != nil {
		return false, fmt.Errorf("subagent: approval %q for %s: %w", req.ToolName, req.AgentID, err)
	}
	return ok, nil
}

// Request is an alias for Ask.
func (q *ApprovalQueue) Request(ctx context.Context, req ApprovalRequest) (bool, error) {
	return q.Ask(ctx, req)
}
