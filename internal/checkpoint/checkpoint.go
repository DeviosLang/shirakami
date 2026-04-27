package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
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
