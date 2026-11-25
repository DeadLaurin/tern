package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Todo statuses.
const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoCompleted  = "completed"
	TodoCancelled  = "cancelled"
)

// TodoItem is one entry in the agent's structured task list.
type TodoItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"`
}

// ValidTodoStatus reports whether status is a supported todo state.
func ValidTodoStatus(status string) bool {
	switch status {
	case TodoPending, TodoInProgress, TodoCompleted, TodoCancelled:
		return true
	default:
		return false
	}
}

// TodoStore holds the current task list. It is safe for concurrent use.
type TodoStore struct {
	mu    sync.Mutex
	items []TodoItem
}

// NewTodoStore returns an empty todo store.
func NewTodoStore() *TodoStore { return &TodoStore{} }

// Items returns a copy of the current list.
func (s *TodoStore) Items() []TodoItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TodoItem(nil), s.items...)
}

// Replace validates and replaces the whole list. It reports whether the list
// actually changed.
func (s *TodoStore) Replace(items []TodoItem) (bool, error) {
	next := make([]TodoItem, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for i, item := range items {
		item = TodoItem{
			ID:      strings.TrimSpace(item.ID),
			Content: strings.TrimSpace(item.Content),
			Status:  strings.TrimSpace(item.Status),
		}
		if item.ID == "" {
			return false, fmt.Errorf("todo: item %d is missing an id", i+1)
		}
		if item.Content == "" {
			return false, fmt.Errorf("todo: item %q is missing content", item.ID)
		}
		if !ValidTodoStatus(item.Status) {
			return false, fmt.Errorf("todo: item %q has invalid status %q (want pending, in_progress, completed, or cancelled)", item.ID, item.Status)
		}
		if _, dup := seen[item.ID]; dup {
			return false, fmt.Errorf("todo: duplicate item id %q", item.ID)
		}
		seen[item.ID] = struct{}{}
		next = append(next, item)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if equalTodos(s.items, next) {
		return false, nil
	}
	s.items = next
	return true, nil
}

func equalTodos(a, b []TodoItem) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TodoTool maintains the agent's structured task list.
type TodoTool struct {
	store *TodoStore
}

var todoSchema = schema(`{
  "type": "object",
  "properties": {
    "todos": {
      "type": "array",
      "description": "The complete task list, replacing the previous list.",
      "items": {
        "type": "object",
        "properties": {
          "id": {"type": "string", "description": "Stable item identifier."},
          "content": {"type": "string", "description": "What needs to be done."},
          "status": {"type": "string", "enum": ["pending", "in_progress", "completed", "cancelled"]}
        },
        "required": ["id", "content", "status"],
        "additionalProperties": false
      }
    }
  },
  "required": ["todos"],
  "additionalProperties": false
}`)

// NewTodoTool returns a todo tool with its own store.
func NewTodoTool() *TodoTool {
	return &TodoTool{store: NewTodoStore()}
}

// NewTodoToolWithStore returns a todo tool backed by store.
func NewTodoToolWithStore(store *TodoStore) *TodoTool {
	if store == nil {
		store = NewTodoStore()
	}
	return &TodoTool{store: store}
}

// Store returns the tool's todo store.
func (t *TodoTool) Store() *TodoStore { return t.store }

func (t *TodoTool) Name() string                { return ToolTodo }
func (t *TodoTool) IsReadOnly() bool            { return false }
func (t *TodoTool) IsWeb() bool                 { return false }
func (t *TodoTool) IsParallelSafe() bool        { return false }
func (t *TodoTool) Parameters() json.RawMessage { return todoSchema }
func (t *TodoTool) Description() string {
	return "Maintain a structured task list. Send the complete list of todos; each item has an id, content, and status (pending, in_progress, completed, cancelled)."
}

type todoArgs struct {
	Todos []TodoItem `json:"todos"`
}

// Execute implements Tool.
func (t *TodoTool) Execute(_ context.Context, env ExecEnv, raw json.RawMessage) (Result, error) {
	var args todoArgs
	if err := decodeArgs(raw, &args); err != nil {
		return Result{}, fmt.Errorf("todo: invalid arguments: %w", err)
	}
	todos := args.Todos
	if todos == nil {
		todos = []TodoItem{}
	}
	changed, err := t.store.Replace(todos)
	if err != nil {
		return Result{}, err
	}
	items := t.store.Items()
	if changed && env.OnTodoUpdate != nil {
		env.OnTodoUpdate(append([]TodoItem(nil), items...))
	}
	meta := map[string]any{
		"todo_count": len(items),
		"changed":    changed,
	}
	if len(items) == 0 {
		return Result{Output: "todo list cleared", Metadata: meta}, nil
	}
	var b strings.Builder
	for i, item := range items {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d. [%s] %s (id: %s)", i+1, item.Status, item.Content, item.ID)
	}
	return Result{
		Output:   env.Truncate(b.String()),
		Metadata: meta,
	}, nil
}

// AskUserTool asks the user a question.
type AskUserTool struct{}

var askUserSchema = schema(`{
  "type": "object",
  "properties": {
    "question": {"type": "string", "description": "The question to ask."},
    "options": {
      "type": "array",
      "description": "Optional list of suggested answers.",
      "items": {"type": "string"}
    }
  },
  "required": ["question"],
  "additionalProperties": false
}`)

// NewAskUserTool returns the ask_user tool.
func NewAskUserTool() *AskUserTool { return &AskUserTool{} }

func (t *AskUserTool) Name() string                { return ToolAskUser }
func (t *AskUserTool) IsReadOnly() bool            { return true }
func (t *AskUserTool) IsWeb() bool                 { return false }
func (t *AskUserTool) IsParallelSafe() bool        { return false }
func (t *AskUserTool) Parameters() json.RawMessage { return askUserSchema }
func (t *AskUserTool) Description() string {
	return "Ask the user a question, optionally with a list of suggested options, and return their answer."
}

type askUserArgs struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
}

// Execute implements Tool.
func (t *AskUserTool) Execute(ctx context.Context, env ExecEnv, raw json.RawMessage) (Result, error) {
	var args askUserArgs
	if err := decodeArgs(raw, &args); err != nil {
		return Result{}, fmt.Errorf("ask_user: invalid arguments: %w", err)
	}
	question := strings.TrimSpace(args.Question)
	if question == "" {
		return Result{}, errors.New(`ask_user: missing required argument "question"`)
	}
	if env.AskUser == nil {
		return Result{}, errors.New("ask_user: unavailable in this non-interactive session")
	}
	answer, err := env.AskUser(ctx, question, args.Options)
	if err != nil {
		return Result{}, fmt.Errorf("ask_user: %w", err)
	}
	return Result{
		Output: answer,
		Metadata: map[string]any{
			"question": question,
			"options":  args.Options,
		},
	}, nil
}
