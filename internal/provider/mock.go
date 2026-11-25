package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// MockProvider is a deterministic Provider for tests. It can replay either
// programmatic StreamEvent scripts or raw recorded SSE fixtures. Every
// received request is recorded together with its canonical serialization.
//
// A turn is consumed from Scripts first, then from SSEScripts. Assign the
// exported slices directly or use the Queue/Add helpers.
type MockProvider struct {
	mu sync.Mutex

	// Scripts holds programmatic event scripts, one slice per turn.
	Scripts [][]StreamEvent
	// SSEScripts holds raw SSE fixtures, one per turn.
	SSEScripts []string

	// Requests records every ChatRequest passed to Stream.
	Requests []ChatRequest
	// RequestBytes records MarshalCanonicalRequest output for each request.
	RequestBytes [][]byte
	// Calls counts Stream invocations.
	Calls int
}

// NewMockProvider returns an empty mock provider.
func NewMockProvider() *MockProvider {
	return &MockProvider{}
}

// ModelInfo implements Provider.
func (m *MockProvider) ModelInfo(model string) ModelCapabilities {
	return DefaultModelCapabilities(model)
}

// QueueScript appends one programmatic turn. Passing no events queues an
// intentionally empty turn.
func (m *MockProvider) QueueScript(events ...StreamEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Scripts = append(m.Scripts, append([]StreamEvent(nil), events...))
}

// AddScript is an alias for QueueScript.
func (m *MockProvider) AddScript(events []StreamEvent) {
	m.QueueScript(events...)
}

// QueueSSE appends one raw SSE fixture turn.
func (m *MockProvider) QueueSSE(fixture string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.SSEScripts = append(m.SSEScripts, fixture)
}

// AddSSE is an alias for QueueSSE.
func (m *MockProvider) AddSSE(fixture string) {
	m.QueueSSE(fixture)
}

// Stream implements Provider.
func (m *MockProvider) Stream(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	canonical, canonicalErr := MarshalCanonicalRequest(req)

	m.mu.Lock()
	m.Calls++
	call := m.Calls
	m.Requests = append(m.Requests, req)
	if canonicalErr == nil {
		m.RequestBytes = append(m.RequestBytes, append([]byte(nil), canonical...))
	} else {
		m.RequestBytes = append(m.RequestBytes, nil)
	}

	var script []StreamEvent
	hasScript := false
	var fixture string
	hasFixture := false
	switch {
	case len(m.Scripts) > 0:
		script = m.Scripts[0]
		m.Scripts = m.Scripts[1:]
		hasScript = true
	case len(m.SSEScripts) > 0:
		fixture = m.SSEScripts[0]
		m.SSEScripts = m.SSEScripts[1:]
		hasFixture = true
	}
	m.mu.Unlock()

	if !hasScript && !hasFixture {
		return nil, fmt.Errorf("provider: mock has no script queued for turn %d", call)
	}

	out := make(chan StreamEvent)
	go func() {
		defer close(out)
		if hasFixture {
			_ = ParseSSEStream(ctx, strings.NewReader(fixture), out)
			return
		}
		for _, ev := range script {
			if !emitEvent(ctx, out, ev) {
				return
			}
		}
	}()
	return out, nil
}

// RequestSnapshot returns a copy of all recorded requests.
func (m *MockProvider) RequestSnapshot() []ChatRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ChatRequest(nil), m.Requests...)
}

// RequestBytesSnapshot returns a copy of all recorded canonical request
// bytes.
func (m *MockProvider) RequestBytesSnapshot() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]byte, len(m.RequestBytes))
	for i := range m.RequestBytes {
		out[i] = append([]byte(nil), m.RequestBytes[i]...)
	}
	return out
}

// LastRequest returns the most recently received request.
func (m *MockProvider) LastRequest() (ChatRequest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.Requests) == 0 {
		return ChatRequest{}, false
	}
	return m.Requests[len(m.Requests)-1], true
}

// LastRequestBytes returns the canonical bytes of the most recent request.
func (m *MockProvider) LastRequestBytes() ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.RequestBytes) == 0 {
		return nil, false
	}
	return append([]byte(nil), m.RequestBytes[len(m.RequestBytes)-1]...), true
}
