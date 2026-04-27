package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/DeviosLang/shirakami/internal/llm"
)

const defaultTTL = 24 * time.Hour

// State holds the persisted state of an in-progress analysis.
type State struct {
	Messages  []messageEnvelope `json:"messages"`
	StepCount int               `json:"step_count"`
	TaskID    string            `json:"task_id"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// messageEnvelope is used to serialize / deserialize the llm.Message interface.
type messageEnvelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// MarshalMessages converts a slice of llm.Message into JSON-serializable envelopes.
func MarshalMessages(msgs []llm.Message) ([]messageEnvelope, error) {
	out := make([]messageEnvelope, 0, len(msgs))
	for _, m := range msgs {
		var typeName string
		switch m.(type) {
		case llm.UserMessage:
			typeName = "user"
		case llm.AssistantMessage:
			typeName = "assistant"
		case llm.ToolResultMessage:
			typeName = "tool_result"
		default:
			return nil, fmt.Errorf("unknown message type: %T", m)
		}
		raw, err := json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("marshal message: %w", err)
		}
		out = append(out, messageEnvelope{Type: typeName, Payload: raw})
	}
	return out, nil
}

// UnmarshalMessages converts envelopes back to llm.Message values.
func UnmarshalMessages(envs []messageEnvelope) ([]llm.Message, error) {
	out := make([]llm.Message, 0, len(envs))
	for _, e := range envs {
		switch e.Type {
		case "user":
			var m llm.UserMessage
			if err := json.Unmarshal(e.Payload, &m); err != nil {
				return nil, err
			}
			out = append(out, m)
		case "assistant":
			var m llm.AssistantMessage
			if err := json.Unmarshal(e.Payload, &m); err != nil {
				return nil, err
			}
			out = append(out, m)
		case "tool_result":
			var m llm.ToolResultMessage
			if err := json.Unmarshal(e.Payload, &m); err != nil {
				return nil, err
			}
			out = append(out, m)
		default:
			return nil, fmt.Errorf("unknown envelope type: %s", e.Type)
		}
	}
	return out, nil
}

// Checkpointer persists and restores agent state in Redis.
type Checkpointer struct {
	rdb *redis.Client
	ttl time.Duration
}

// New creates a new Checkpointer backed by a Redis client.
func New(rdb *redis.Client) *Checkpointer {
	return &Checkpointer{rdb: rdb, ttl: defaultTTL}
}

// Save serializes and stores state under taskID with a 24-hour TTL.
func (c *Checkpointer) Save(ctx context.Context, taskID string, messages []llm.Message, stepCount int) error {
	envs, err := MarshalMessages(messages)
	if err != nil {
		return fmt.Errorf("marshal messages: %w", err)
	}
	s := State{
		Messages:  envs,
		StepCount: stepCount,
		TaskID:    taskID,
		UpdatedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	return c.rdb.Set(ctx, checkpointKey(taskID), data, c.ttl).Err()
}

// Load retrieves and deserializes the state for taskID.
// Returns nil, nil when the key does not exist.
func (c *Checkpointer) Load(ctx context.Context, taskID string) (*State, []llm.Message, error) {
	data, err := c.rdb.Get(ctx, checkpointKey(taskID)).Bytes()
	if err == redis.Nil {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("redis get: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, nil, fmt.Errorf("unmarshal state: %w", err)
	}
	msgs, err := UnmarshalMessages(s.Messages)
	if err != nil {
		return nil, nil, fmt.Errorf("unmarshal messages: %w", err)
	}
	return &s, msgs, nil
}

// Delete removes the checkpoint for taskID after analysis completes.
func (c *Checkpointer) Delete(ctx context.Context, taskID string) error {
	return c.rdb.Del(ctx, checkpointKey(taskID)).Err()
}

func checkpointKey(taskID string) string {
	return "shirakami:checkpoint:" + taskID
}

// ---------------------------------------------------------------------------
// FileCheckpointer: disk-based checkpoint for local / test use.
// ---------------------------------------------------------------------------

// RawMessage is a JSON-serializable wrapper for llm.Message used by FileCheckpointer.
type RawMessage struct {
	Role    string          `json:"role"`
	Payload json.RawMessage `json:"payload"`
}

// FileState holds the serializable state of an agent loop run (file-backed).
type FileState struct {
	TaskID    string       `json:"task_id"`
	StepCount int          `json:"step_count"`
	Messages  []RawMessage `json:"messages"`
}

// FileCheckpointer saves and loads agent loop state to/from disk.
// Intended for local development and tests where Redis is unavailable.
type FileCheckpointer struct {
	dir string
}

// NewFileCheckpointer creates a FileCheckpointer that stores files under dir.
func NewFileCheckpointer(dir string) (*FileCheckpointer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create checkpoint dir: %w", err)
	}
	return &FileCheckpointer{dir: dir}, nil
}

// Save persists state to disk, overwriting any previous checkpoint for the same taskID.
func (c *FileCheckpointer) Save(taskID string, stepCount int, messages []llm.Message) error {
	state := FileState{
		TaskID:    taskID,
		StepCount: stepCount,
		Messages:  make([]RawMessage, 0, len(messages)),
	}

	for _, m := range messages {
		raw, err := marshalRaw(m)
		if err != nil {
			return fmt.Errorf("marshal message: %w", err)
		}
		state.Messages = append(state.Messages, raw)
	}

	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	path := c.path(taskID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write checkpoint tmp: %w", err)
	}
	return os.Rename(tmp, path)
}

// Load reads a previously saved checkpoint. Returns nil state (no error) if none exists.
func (c *FileCheckpointer) Load(taskID string) (*FileState, error) {
	data, err := os.ReadFile(c.path(taskID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read checkpoint: %w", err)
	}

	var state FileState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("unmarshal checkpoint: %w", err)
	}
	return &state, nil
}

// Delete removes a checkpoint file. Safe to call if it doesn't exist.
func (c *FileCheckpointer) Delete(taskID string) error {
	err := os.Remove(c.path(taskID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// RestoreMessages converts raw checkpoint messages back into llm.Message values.
func RestoreMessages(raws []RawMessage) ([]llm.Message, error) {
	msgs := make([]llm.Message, 0, len(raws))
	for _, r := range raws {
		m, err := unmarshalRaw(r)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}

func (c *FileCheckpointer) path(taskID string) string {
	return filepath.Join(c.dir, taskID+".json")
}

func marshalRaw(m llm.Message) (RawMessage, error) {
	var role string
	switch m.(type) {
	case llm.UserMessage:
		role = "user"
	case llm.AssistantMessage:
		role = "assistant"
	case llm.ToolResultMessage:
		role = "tool"
	default:
		return RawMessage{}, fmt.Errorf("unknown message type %T", m)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return RawMessage{}, err
	}
	return RawMessage{Role: role, Payload: data}, nil
}

func unmarshalRaw(r RawMessage) (llm.Message, error) {
	switch r.Role {
	case "user":
		var m llm.UserMessage
		if err := json.Unmarshal(r.Payload, &m); err != nil {
			return nil, err
		}
		return m, nil
	case "assistant":
		var m llm.AssistantMessage
		if err := json.Unmarshal(r.Payload, &m); err != nil {
			return nil, err
		}
		return m, nil
	case "tool":
		var m llm.ToolResultMessage
		if err := json.Unmarshal(r.Payload, &m); err != nil {
			return nil, err
		}
		return m, nil
	default:
		return nil, fmt.Errorf("unknown message role %q", r.Role)
	}
}
