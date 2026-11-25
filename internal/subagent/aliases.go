package subagent

// This file contains small, descriptive aliases for the core public types and
// helpers. They keep the API approachable without changing behaviour.

// AgentDefinition is an alias for Definition.
type AgentDefinition = Definition

// ManagerConfig is an alias for Config.
type ManagerConfig = Config

// SubagentEvent is an alias for Event.
type SubagentEvent = Event

// SubagentTask is an alias for Task.
type SubagentTask = Task

// Report is an alias for AgentReport.
type Report = AgentReport

// Event aliases.
const (
	EventStart    = EventSubagentStart
	EventStep     = EventSubagentStep
	EventDelta    = EventSubagentDelta
	EventEnd      = EventSubagentEnd
	EventApproval = EventSubagentApproval
)

// BuiltinAgents returns the built-in agent definitions sorted by name.
func BuiltinAgents() []Definition { return BuiltinDefinitions() }

// LoadAgents loads built-in plus global and project agent definitions.
func LoadAgents(globalConfigDir, workDir string) ([]Definition, error) {
	return LoadDefinitions(globalConfigDir, workDir)
}

// LoadAgent returns one loaded definition by name.
func LoadAgent(globalConfigDir, workDir, name string) (Definition, bool, error) {
	return NewLoader(globalConfigDir, workDir).Get(name)
}

// RenderBlock is an alias for RenderAgent.
func RenderBlock(v AgentView, expanded bool) string { return RenderAgent(v, expanded) }

// RenderAgentBlock is an alias for RenderAgent.
func RenderAgentBlock(v AgentView, expanded bool) string { return RenderAgent(v, expanded) }

// RenderAgentGroup is an alias for RenderGroup.
func RenderAgentGroup(views []AgentView, expanded bool) string { return RenderGroup(views, expanded) }

// IsolateWorkspace prepares a disposable copy of workDir.
func IsolateWorkspace(workDir string) (*Workcopy, error) { return PrepareWorkcopy(workDir) }

// New is an alias for NewManager.
func New(config Config) *Manager { return NewManager(config) }

// Cancel is an alias for CancelOne.
func (m *Manager) Cancel(id string) bool { return m.CancelOne(id) }

// Active is an alias for List.
func (m *Manager) Active() []AgentStatus { return m.List() }

// Close cancels all agents and waits for them to finish.
func (m *Manager) Close() {
	m.CancelAll()
	m.Wait()
}
