package tui

import (
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tern-agent/tern/internal/agent"
	"github.com/tern-agent/tern/internal/subagent"
)

// Message types used internally by the model.
type (
	tickMsg          time.Time
	agentEventMsg    struct{ event agent.Event }
	subagentEventMsg struct{ event subagent.Event }
	turnDoneMsg      struct {
		result *agent.TurnResult
		err    error
	}
	cmdResultMsg struct {
		text  string
		err   error
		quiet bool
	}
	approvalRequestMsg struct{ request *approvalState }
	askUserMsg         struct{ request *askState }
	editorExternalMsg  struct {
		content string
		err     error
	}
)

// eventBus decouples agent goroutines from Bubble Tea's update loop. Producers
// append to an unbounded queue under a mutex; a single pump goroutine forwards
// messages to the program. A slow UI therefore never blocks an agent.
type eventBus struct {
	mu     sync.Mutex
	cond   *sync.Cond
	queue  []tea.Msg
	closed bool
}

func newEventBus() *eventBus {
	b := &eventBus{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// Push enqueues a message. It never blocks.
func (b *eventBus) Push(msg tea.Msg) {
	if b == nil {
		return
	}
	b.mu.Lock()
	if !b.closed {
		b.queue = append(b.queue, msg)
		b.cond.Signal()
	}
	b.mu.Unlock()
}

// Close stops the pump. Queued messages are discarded.
func (b *eventBus) Close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.closed = true
	b.queue = nil
	b.cond.Broadcast()
	b.mu.Unlock()
}

func (b *eventBus) pop() (tea.Msg, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.queue) == 0 && !b.closed {
		b.cond.Wait()
	}
	if b.closed {
		return nil, false
	}
	msg := b.queue[0]
	b.queue[0] = nil
	b.queue = b.queue[1:]
	return msg, true
}

// pump forwards queued messages to p until the bus is closed.
func (b *eventBus) pump(p *tea.Program) {
	for {
		msg, ok := b.pop()
		if !ok {
			return
		}
		p.Send(msg)
	}
}
