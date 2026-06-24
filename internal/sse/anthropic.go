package sse

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/ferro-labs/ai-gateway/internal/anthropicwire"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// WriteAnthropic converts an OpenAI-shaped stream-chunk channel to Anthropic SSE
// events and writes them to the response writer. This is used for cross-format
// routing: when an Anthropic-format client request is routed to an OpenAI-native
// provider, the response must be converted back to Anthropic SSE events.
//
// The conversion approximates Anthropic streaming:
//   - message_start (once, with prompt tokens)
//   - content_block_start (once, at first text or tool_use)
//   - content_block_delta (text_delta or input_json_delta per chunk)
//   - content_block_stop (once, before message_delta)
//   - message_delta (stop_reason + output tokens)
//   - message_stop (terminal event)
func WriteAnthropic(ctx context.Context, w http.ResponseWriter, ch <-chan core.StreamChunk) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	var (
		msgID        string
		model        string
		started      bool
		toolStarted  bool
		textStarted  bool
		toolBlockIdx int
		promptTokens int
		finished     bool
	)

	for {
		select {
		case <-ctx.Done():
			return
		case chunk, ok := <-ch:
			if !ok {
				return
			}
			if chunk.Error != nil {
				return
			}

			if msgID == "" {
				msgID = chunk.ID
			}
			if model == "" && chunk.Model != "" {
				model = chunk.Model
			}

			// Send message_start on first chunk.
			if !started {
				started = true
				if chunk.Usage != nil {
					promptTokens = chunk.Usage.PromptTokens
				}
				emitSSE(w, flusher, "message_start", anthropicwire.StreamMessageStart{
					Type: "message_start",
					Message: anthropicwire.StreamMessageInfo{
						ID:    msgID,
						Type:  "message",
						Role:  "assistant",
						Model: model,
						Usage: anthropicwire.Usage{InputTokens: promptTokens},
					},
				})
			}

			for _, choice := range chunk.Choices {
				// Tool call deltas.
				for _, tc := range choice.Delta.ToolCalls {
					if !toolStarted {
						if textStarted {
							// Close the text block before starting tool_use.
							emitSSE(w, flusher, "content_block_stop",
								map[string]interface{}{"type": "content_block_stop", "index": 0})
						}
						toolStarted = true
						emitSSE(w, flusher, "content_block_start", anthropicwire.StreamContentBlockStart{
							Type:  "content_block_start",
							Index: 0,
							ContentBlock: anthropicwire.StreamContentBlockInfo{
								Type: "tool_use",
								ID:   tc.ID,
								Name: tc.Function.Name,
							},
						})
					}
					if tc.Function.Arguments != "" {
						emitSSE(w, flusher, "content_block_delta", anthropicwire.StreamContentBlockDelta{
							Type:  "content_block_delta",
							Index: toolBlockIdx,
							Delta: anthropicwire.StreamDeltaPayload{
								Type:        "input_json_delta",
								PartialJSON: tc.Function.Arguments,
							},
						})
						flusher.Flush()
					}
				}

				// Text delta.
				if choice.Delta.Content != "" {
					if !textStarted && !toolStarted {
						textStarted = true
						emitSSE(w, flusher, "content_block_start", anthropicwire.StreamContentBlockStart{
							Type:  "content_block_start",
							Index: 0,
							ContentBlock: anthropicwire.StreamContentBlockInfo{
								Type: "text",
							},
						})
					}
					emitSSE(w, flusher, "content_block_delta", anthropicwire.StreamContentBlockDelta{
						Type:  "content_block_delta",
						Index: 0,
						Delta: anthropicwire.StreamDeltaPayload{
							Type: "text_delta",
							Text: choice.Delta.Content,
						},
					})
					flusher.Flush()
				}

				// Finish reason.
				if choice.FinishReason != "" && !finished {
					finished = true

					lastIdx := 0
					emitSSE(w, flusher, "content_block_stop",
						map[string]interface{}{"type": "content_block_stop", "index": lastIdx})

					outputTokens := 0
					if chunk.Usage != nil {
						outputTokens = chunk.Usage.CompletionTokens
						if promptTokens == 0 {
							promptTokens = chunk.Usage.PromptTokens
						}
					}

					emitSSE(w, flusher, "message_delta", anthropicwire.StreamMessageDelta{
						Type: "message_delta",
						Delta: anthropicwire.StreamStopDelta{
							StopReason: finishReasonToAnthropic(choice.FinishReason),
						},
						Usage: anthropicwire.Usage{
							InputTokens:  promptTokens,
							OutputTokens: outputTokens,
						},
					})
					emitSSE(w, flusher, "message_stop",
						map[string]string{"type": "message_stop"})
					flusher.Flush()
					return
				}
			}
		}
	}
}

func emitSSE(w io.Writer, flusher http.Flusher, event string, payload interface{}) {
	data, _ := json.Marshal(payload)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	flusher.Flush()
}

// finishReasonToAnthropic maps OpenAI finish reasons to Anthropic stop_reason values.
func finishReasonToAnthropic(reason string) string {
	switch reason {
	case core.FinishReasonStop:
		return "end_turn"
	case core.FinishReasonLength:
		return "max_tokens"
	case core.FinishReasonToolCalls:
		return "tool_use"
	case core.FinishReasonContentFilter:
		return "content_filter"
	default:
		return reason
	}
}

// ProxyAnthropicStream forwards a raw SSE response body from an Anthropic-native
// provider directly to the client, preserving the native event format.
// The caller must close resp.Body.
func ProxyAnthropicStream(ctx context.Context, w http.ResponseWriter, resp *http.Response) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				flusher.Flush()
			}
			if err != nil {
				return
			}
		}
	}
}
