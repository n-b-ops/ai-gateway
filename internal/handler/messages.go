package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/internal/anthropicwire"
	"github.com/ferro-labs/ai-gateway/internal/apierror"
	"github.com/ferro-labs/ai-gateway/internal/sse"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// Messages handles POST /v1/messages — the Anthropic Messages API endpoint.
//
// Two routing paths:
//  1. Anthropic-native provider: forwards the raw request body directly,
//     preserving tool definitions, thinking blocks, and content block fidelity.
//     The upstream response is returned verbatim (or streamed raw for streaming).
//  2. OpenAI-native provider: converts the Anthropic request to core.Request,
//     routes through the full gateway pipeline (strategy, plugins, OTEL),
//     then converts the core.Response back to Anthropic JSON.
func Messages(gw *aigateway.Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Read and buffer the body so we can use it both for model extraction
		// and potential raw forwarding.
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
			return
		}
		_ = r.Body.Close()

		// Extract model name for routing from the buffered body.
		model := extractModelFromJSON(bodyBytes)
		if model == "" {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
				`no "model" field found in request body`)
			return
		}

		// Resolve model alias and find provider.
		model = gw.ResolveAlias(model)
		p, ok := gw.FindByModel(model)
		if !ok {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
				"no provider supports model: "+model)
			return
		}

		// Check if streaming.
		stream := isStreamRequest(bodyBytes)

		// --- Path 1: Anthropic-native provider ---
		if ap, ok := p.(core.AnthropicProvider); ok {
			if stream {
				handleAnthropicStream(w, r, ap, bytes.NewReader(bodyBytes))
			} else {
				handleAnthropicNonStream(w, r, ap, bytes.NewReader(bodyBytes))
			}
			return
		}

		// --- Path 2: OpenAI-native provider ---
		req, err := anthropicToRequest(bodyBytes, model)
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		if stream {
			ch, err := gw.RouteStream(r.Context(), req)
			if err != nil {
				status, errType, code := apierror.RouteErrorDetails(err)
				writeAnthropicError(w, status, errType, code)
				return
			}
			sse.WriteAnthropic(r.Context(), w, ch)
			return
		}

		resp, err := gw.Route(r.Context(), req)
		if err != nil {
			status, errType, code := apierror.RouteErrorDetails(err)
			writeAnthropicError(w, status, errType, code)
			return
		}
		writeAnthropicResponse(w, resp)
	}
}

// handleAnthropicNonStream forwards a raw Anthropic request to a native provider
// and writes the response back as JSON.
func handleAnthropicNonStream(w http.ResponseWriter, r *http.Request, ap core.AnthropicProvider, body io.Reader) {
	resp, err := ap.HandleAnthropicRequest(r.Context(), body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "failed to read upstream response")
		return
	}

	if resp.StatusCode != http.StatusOK {
		var errResp anthropicwire.ErrorResponse
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error.Message != "" {
			writeAnthropicError(w, resp.StatusCode, errResp.Error.Type, errResp.Error.Message)
			return
		}
		writeAnthropicError(w, resp.StatusCode, "api_error", string(respBody))
		return
	}

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

// handleAnthropicStream forwards a raw Anthropic streaming request and proxies
// the SSE response.
func handleAnthropicStream(w http.ResponseWriter, r *http.Request, ap core.AnthropicProvider, body io.Reader) {
	resp, err := ap.HandleAnthropicRequest(r.Context(), body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		writeAnthropicError(w, resp.StatusCode, "api_error", string(respBody))
		return
	}

	sse.ProxyAnthropicStream(r.Context(), w, resp)
}

// extractModelFromJSON extracts the top-level "model" string from a JSON body.
func extractModelFromJSON(body []byte) string {
	var raw struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &raw) == nil {
		return raw.Model
	}
	return ""
}

// isStreamRequest checks whether the request body has "stream": true.
func isStreamRequest(body []byte) bool {
	var raw struct {
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(body, &raw) == nil {
		return raw.Stream
	}
	return false
}

// anthropicToRequest converts an Anthropic Messages API request body to the
// internal OpenAI-shaped core.Request. This is used for cross-format routing
// when the target provider does not speak Anthropic natively.
//
// Conversion mappings:
//   Anthropic system (string/array) → core.RoleSystem message
//   Anthropic messages          → core messages (role/content mapping)
//   Anthropic tools              → core.Tool (with Function wrapper)
//   Anthropic tool_choice        → core.ToolChoice
//   Anthropic max_tokens          → core.MaxTokens
//   Anthropic temperature         → core.Temperature
//   Anthropic top_p               → core.TopP
//   Anthropic stop_sequences      → core.Stop
func anthropicToRequest(body []byte, model string) (core.Request, error) {
	var areq anthropicwire.MessageRequest
	if err := json.Unmarshal(body, &areq); err != nil {
		return core.Request{}, fmt.Errorf("invalid Anthropic request: %w", err)
	}

	req := core.Request{
		Model:  model,
		Stream: areq.Stream,
	}

	// Max tokens.
	if areq.MaxTokens > 0 {
		mt := areq.MaxTokens
		req.MaxTokens = &mt
	}

	// Temperature.
	if areq.Temperature != nil {
		req.Temperature = areq.Temperature
	}

	// Top P.
	if areq.TopP != nil {
		req.TopP = areq.TopP
	}

	// Stop sequences.
	req.Stop = areq.StopSequences

	// System prompt.
	systemText := extractSystemText(areq.System)
	if systemText != "" {
		req.Messages = append(req.Messages, core.Message{
			Role:    core.RoleSystem,
			Content: systemText,
		})
	}

	// Messages.
	for _, msg := range areq.Messages {
		req.Messages = append(req.Messages, convertAnthropicMessage(msg))
	}

	// Tools.
	req.Tools = convertAnthropicTools(areq.Tools)

	// Tool choice.
	req.ToolChoice = convertAnthropicToolChoice(areq.ToolChoice)

	// Metadata user_id → user field.
	if areq.Metadata != nil && areq.Metadata.UserID != "" {
		req.User = areq.Metadata.UserID
	}

	return req, nil
}

// extractSystemText converts the Anthropic system field to a plain string.
// In Anthropic API, system can be a string or an array of content blocks.
func extractSystemText(system any) string {
	switch s := system.(type) {
	case string:
		return s
	case []interface{}:
		var parts []string
		for _, block := range s {
			if m, ok := block.(map[string]interface{}); ok {
				if t, ok := m["type"].(string); ok && t == "text" {
					if text, ok := m["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
		return joinStrings(parts, "\n")
	default:
		return ""
	}
}

func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	result := parts[0]
	for i := 1; i < len(parts); i++ {
		result += sep + parts[i]
	}
	return result
}

// convertAnthropicMessage converts a single Anthropic message to a core.Message.
func convertAnthropicMessage(msg anthropicwire.Message) core.Message {
	cm := core.Message{
		Role: msg.Role,
	}
	switch content := msg.Content.(type) {
	case string:
		cm.Content = content
	case []interface{}:
		cm.ContentParts = convertContentBlocks(content)
		// Build a plain text concatenation for Content too.
		var texts []string
		for _, block := range content {
			if m, ok := block.(map[string]interface{}); ok {
				if t, _ := m["type"].(string); t == "text" {
					if text, ok := m["text"].(string); ok {
						texts = append(texts, text)
					}
				}
			}
		}
		cm.Content = joinStrings(texts, "")
	}
	return cm
}

// convertContentBlocks converts Anthropic content blocks to core.ContentPart.
func convertContentBlocks(blocks []interface{}) []core.ContentPart {
	var parts []core.ContentPart
	for _, block := range blocks {
		m, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		blockType, _ := m["type"].(string)
		switch blockType {
		case "text":
			if text, ok := m["text"].(string); ok {
				parts = append(parts, core.ContentPart{
					Type: core.ContentTypeText,
					Text: text,
				})
			}
		case "image":
			if src, ok := m["source"].(map[string]interface{}); ok {
				imgType, _ := src["type"].(string)
				if imgType == "base64" {
					mediaType, _ := src["media_type"].(string)
					data, _ := src["data"].(string)
					if mediaType != "" && data != "" {
						parts = append(parts, core.ContentPart{
							Type: "image_url",
							ImageURL: &core.ImageURLPart{
								URL: "data:" + mediaType + ";base64," + data,
							},
						})
					}
				}
			}
		case "tool_use":
			id, _ := m["id"].(string)
			name, _ := m["name"].(string)
			input, _ := json.Marshal(m["input"])
			if name != "" {
				parts = append(parts, core.ContentPart{
					Type: core.ContentTypeText,
					Text: fmt.Sprintf("[tool_use id=%s name=%s input=%s]", id, name, string(input)),
				})
			}
		case "tool_result":
			toolUseID, _ := m["tool_use_id"].(string)
			contentStr := ""
			if c, ok := m["content"].(string); ok {
				contentStr = c
			}
			parts = append(parts, core.ContentPart{
				Type: core.ContentTypeText,
				Text: fmt.Sprintf("[tool_result id=%s] %s", toolUseID, contentStr),
			})
		}
	}
	return parts
}

// convertAnthropicTools converts Anthropic tool definitions to core.Tool.
func convertAnthropicTools(atools []anthropicwire.Tool) []core.Tool {
	if len(atools) == 0 {
		return nil
	}
	tools := make([]core.Tool, 0, len(atools))
	for _, t := range atools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools = append(tools, core.Tool{
			Type: "function",
			Function: core.Function{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  schema,
			},
		})
	}
	return tools
}

// convertAnthropicToolChoice converts Anthropic tool_choice to core.ToolChoice.
func convertAnthropicToolChoice(tc any) any {
	if tc == nil {
		return nil
	}
	if s, ok := tc.(string); ok {
		switch s {
		case "auto":
			return "auto"
		case "any":
			return "required"
		case "none":
			return "none"
		}
	}
	if m, ok := tc.(map[string]interface{}); ok {
		if t, _ := m["type"].(string); t == "tool" {
			if name, ok := m["name"].(string); ok {
				return map[string]interface{}{
					"type": "function",
					"function": map[string]string{
						"name": name,
					},
				}
			}
		}
	}
	return nil
}

// writeAnthropicResponse converts a core.Response to an Anthropic Messages
// response and writes it as JSON.
func writeAnthropicResponse(w http.ResponseWriter, resp *core.Response) {
	var content []anthropicwire.ContentBlock

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]

		if choice.Message.Content != "" {
			content = append(content, anthropicwire.ContentBlock{
				Type: "text",
				Text: choice.Message.Content,
			})
		}

		for _, tc := range choice.Message.ToolCalls {
			content = append(content, anthropicwire.ContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: json.RawMessage(tc.Function.Arguments),
			})
		}
	}

	stopReason := "end_turn"
	if len(resp.Choices) > 0 && resp.Choices[0].FinishReason != "" {
		stopReason = finishReasonFromOpenAI(resp.Choices[0].FinishReason)
	}

	aresp := anthropicwire.MessageResponse{
		ID:      resp.ID,
		Type:    "message",
		Role:    "assistant",
		Content: content,
		Model:   resp.Model,
		Usage: anthropicwire.Usage{
			InputTokens:              resp.Usage.PromptTokens,
			OutputTokens:             resp.Usage.CompletionTokens,
			CacheReadInputTokens:     resp.Usage.CacheReadTokens,
			CacheCreationInputTokens: resp.Usage.CacheWriteTokens,
		},
		StopReason: stopReason,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(aresp)
}

func finishReasonFromOpenAI(reason string) string {
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

// writeAnthropicError writes an error in Anthropic error format.
func writeAnthropicError(w http.ResponseWriter, statusCode int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(anthropicwire.ErrorResponse{
		Type: "error",
		Error: anthropicwire.ErrorDetail{
			Type:    errType,
			Message: message,
		},
	})
}
