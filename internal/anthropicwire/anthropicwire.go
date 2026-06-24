// Package anthropicwire holds request and response types shared by the
// standalone Anthropic provider, the Anthropic-on-Bedrock path, and any
// handler that needs to parse or serialize the Anthropic Messages API JSON.
package anthropicwire

import (
	"encoding/json"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// Tool is an Anthropic tool definition ({name, description, input_schema}).
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// MapTools converts canonical tool definitions to Anthropic's native shape.
// An empty parameter schema defaults to an empty object, which Anthropic
// requires (input_schema is mandatory).
func MapTools(tools []core.Tool) []Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}
	return out
}

// --- Anthropic Messages API request types ---

// MessageRequest is the top-level request body for POST /v1/messages.
type MessageRequest struct {
	Model         string           `json:"model"`
	MaxTokens     int              `json:"max_tokens"`
	System        any              `json:"system,omitempty"` // string or []ContentBlock
	Messages      []Message        `json:"messages"`
	Tools         []Tool           `json:"tools,omitempty"`
	ToolChoice    any              `json:"tool_choice,omitempty"`
	Temperature   *float64         `json:"temperature,omitempty"`
	TopP          *float64         `json:"top_p,omitempty"`
	StopSequences []string         `json:"stop_sequences,omitempty"`
	Stream        bool             `json:"stream,omitempty"`
	Metadata      *RequestMetadata `json:"metadata,omitempty"`
}

// RequestMetadata holds user-identifying metadata for the request.
type RequestMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

// Message is a single turn in the conversation.
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []ContentBlock
}

// ContentBlock is a single block within a message's content array.
type ContentBlock struct {
	Type string `json:"type"`

	// type=text
	Text string `json:"text,omitempty"`

	// type=image
	Source *ImageSource `json:"source,omitempty"`

	// type=tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// type=tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"` // string or []ContentBlock
}

// ImageSource carries an image for an image content block.
type ImageSource struct {
	Type      string `json:"type"`                 // "base64" | "url"
	MediaType string `json:"media_type,omitempty"` // base64 only
	Data      string `json:"data,omitempty"`       // base64 only
	URL       string `json:"url,omitempty"`        // url only
}

// --- Anthropic Messages API response types ---

// MessageResponse is the top-level response for a non-streaming request.
type MessageResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Content    []ContentBlock `json:"content"`
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"`
	StopSequence string       `json:"stop_sequence,omitempty"`
	Usage      Usage          `json:"usage"`
}

// Usage holds token usage information.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// --- Streaming event types ---

// StreamEvent is the base type for all Anthropic SSE events.
type StreamEvent struct {
	Type string `json:"type"`
}

// StreamMessageStart is the first event in a streaming response.
type StreamMessageStart struct {
	Type    string          `json:"type"`
	Message StreamMessageInfo `json:"message"`
}

// StreamMessageInfo holds the message metadata from message_start.
type StreamMessageInfo struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Role  string `json:"role"`
	Model string `json:"model"`
	Usage Usage  `json:"usage"`
}

// StreamContentBlockStart signals the start of a content block.
type StreamContentBlockStart struct {
	Type         string               `json:"type"`
	Index        int                  `json:"index"`
	ContentBlock StreamContentBlockInfo `json:"content_block"`
}

// StreamContentBlockInfo holds the content block metadata.
type StreamContentBlockInfo struct {
	Type  string          `json:"type"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// StreamContentBlockDelta holds incremental content.
type StreamContentBlockDelta struct {
	Type  string        `json:"type"`
	Index int           `json:"index"`
	Delta StreamDeltaPayload `json:"delta"`
}

// StreamDeltaPayload is the delta within a content_block_delta event.
type StreamDeltaPayload struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

// StreamMessageDelta is the final event before message_stop.
type StreamMessageDelta struct {
	Type  string             `json:"type"`
	Delta StreamStopDelta    `json:"delta"`
	Usage Usage              `json:"usage"`
}

// StreamStopDelta holds the stop reason and sequence.
type StreamStopDelta struct {
	StopReason   string `json:"stop_reason"`
	StopSequence string `json:"stop_sequence,omitempty"`
}

// --- Error types ---

// ErrorResponse is the Anthropic error envelope.
type ErrorResponse struct {
	Type  string      `json:"type"`
	Error ErrorDetail `json:"error"`
}

// ErrorDetail holds the error information.
type ErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
