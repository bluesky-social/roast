package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"charm.land/fantasy"
)

// The preview does not accept ordinary Responses defaults. Keep this boundary
// explicit so SDK updates cannot accidentally enable storage or API billing.
func encodeChatGPTRequest(body []byte, prompt fantasy.Prompt) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("decode ChatGPT request: %w", err)
	}
	if request == nil {
		return nil, errors.New("ChatGPT request must be an object")
	}
	request["include"] = json.RawMessage(`["reasoning.encrypted_content"]`)
	for _, key := range []string{
		"background", "conversation", "max_output_tokens", "max_tool_calls", "metadata", "moderation", "multi_agent", "prompt",
		"prompt_cache_retention", "safety_identifier", "temperature", "top_logprobs", "top_p", "truncation", "user", "previous_response_id",
	} {
		delete(request, key)
	}
	request["store"], request["stream"] = json.RawMessage("false"), json.RawMessage("true")
	if input := request["input"]; len(input) == 0 || input[0] != '[' {
		return nil, errors.New("ChatGPT input must be an array containing the conversation history")
	}
	// Fantasy drops finalized reasoning and tool namespaces while building its
	// input. Use the prompt captured by chatGPTModel for every turn.
	input, err := chatGPTInput(prompt)
	if err != nil {
		return nil, err
	}
	request["input"] = input
	var tools []map[string]json.RawMessage
	if raw := request["tools"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, fmt.Errorf("decode ChatGPT tools: %w", err)
		}
	}
	for _, tool := range tools {
		if string(tool["type"]) != `"function"` {
			return nil, errors.New("ChatGPT Roast supports only local function tools")
		}
	}
	if len(tools) > 0 {
		request["tools"], _ = json.Marshal([]any{map[string]any{
			"type": "namespace", "name": chatGPTToolNamespace,
			"description": "Roast code review and final submission tools.", "tools": tools,
		}})
	}
	return json.Marshal(request)
}

const chatGPTOutputKey = "roast_chatgpt_output"

type chatGPTCallKey struct{}
type chatGPTCall struct {
	prompt fantasy.Prompt
	output chatGPTOutput
}

// Finalized raw output survives Fantasy's conversion to prompt messages. Its
// Responses adapter otherwise discards reasoning and namespace metadata. One
// shared marker on the turn's content restores the complete output exactly
// once, in server order, without promoting it to a higher-priority message.
type chatGPTOutput struct {
	items []json.RawMessage
}

func (*chatGPTOutput) Options()                          {}
func (o *chatGPTOutput) MarshalJSON() ([]byte, error)    { return json.Marshal(o.items) }
func (o *chatGPTOutput) UnmarshalJSON(data []byte) error { return json.Unmarshal(data, &o.items) }

type chatGPTModel struct{ fantasy.LanguageModel }

func (m *chatGPTModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	state := &chatGPTCall{prompt: call.Prompt}
	ctx = context.WithValue(ctx, chatGPTCallKey{}, state)
	stream, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		return nil, providerCallError{err}
	}
	return func(yield func(fantasy.StreamPart) bool) {
		yieldError := func(err error) {
			if err != nil {
				err = providerCallError{err}
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
		}
		calls := make(map[string]fantasy.StreamPart)
		markedOutput := false
		for part := range stream {
			switch part.Type {
			case fantasy.StreamPartTypeError:
				yieldError(part.Error)
				return
			case fantasy.StreamPartTypeToolCall, fantasy.StreamPartTypeTextEnd, fantasy.StreamPartTypeReasoningEnd:
				markedOutput = true
				metadata := make(fantasy.ProviderMetadata, len(part.ProviderMetadata)+1)
				maps.Copy(metadata, part.ProviderMetadata)
				metadata[chatGPTOutputKey] = &state.output
				part.ProviderMetadata = metadata
				if part.Type == fantasy.StreamPartTypeToolCall {
					if _, exists := calls[part.ID]; exists || part.ID == "" || part.ProviderExecuted {
						yieldError(errors.New("invalid ChatGPT streamed tool call"))
						return
					}
					calls[part.ID] = part
					continue
				}
			case fantasy.StreamPartTypeFinish:
				// Parallel calls can finish in a different order. Dispatch them in
				// the order that the server finalized and Roast will replay.
				ordered, err := state.output.finalizedToolCalls(calls)
				if err != nil {
					// The managed model records finish usage and discards its
					// buffered output when the error follows.
					if yield(part) {
						yieldError(err)
					}
					return
				}
				if !markedOutput && len(state.output.items) != 0 {
					// A terminal response may contain only opaque reasoning that
					// the SDK never streamed. Carry its exact output into history.
					const id = "roast-finalized-output"
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningStart, ID: id}) ||
						!yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningEnd, ID: id, ProviderMetadata: fantasy.ProviderMetadata{chatGPTOutputKey: &state.output}}) {
						return
					}
				}
				for _, call := range ordered {
					if !yield(call) {
						return
					}
				}
			}
			if !yield(part) {
				return
			}
		}
	}, nil
}

func (o *chatGPTOutput) finalizedToolCalls(calls map[string]fantasy.StreamPart) ([]fantasy.StreamPart, error) {
	ordered := make([]fantasy.StreamPart, 0, len(calls))
	for _, raw := range o.items {
		var item struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Status    string `json:"status"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("invalid finalized ChatGPT output: %w", err)
		}
		if item.Type != "function_call" {
			continue
		}
		call, ok := calls[item.CallID]
		if !ok || call.ToolCallName != item.Name || call.ToolCallInput != item.Arguments ||
			(item.Status != "" && item.Status != "completed") {
			return nil, errors.New("ChatGPT finalized output does not match streamed tool calls")
		}
		ordered = append(ordered, call)
		delete(calls, item.CallID)
	}
	if len(calls) != 0 {
		return nil, errors.New("ChatGPT finalized output omitted streamed tool calls")
	}
	return ordered, nil
}

// chatGPTInput encodes the full conversation for a stateless Responses call.
func chatGPTInput(prompt fantasy.Prompt) (json.RawMessage, error) {
	input := make([]any, 0, len(prompt))
	for _, message := range prompt {
		seen := make(map[*chatGPTOutput]bool)
		for _, part := range message.Content {
			if original, ok := part.Options()[chatGPTOutputKey].(*chatGPTOutput); ok {
				if message.Role != fantasy.MessageRoleAssistant || original == nil || len(original.items) == 0 {
					return nil, errors.New("ChatGPT history is missing finalized assistant output")
				}
				if !seen[original] {
					for _, item := range original.items {
						input = append(input, item)
					}
					seen[original] = true
				}
				continue
			}
			item, err := chatGPTInputPart(message.Role, part)
			if err != nil {
				return nil, err
			}
			input = append(input, item)
		}
	}
	return json.Marshal(input)
}

func chatGPTConversationBytes(prompt fantasy.Prompt) int {
	// Replay shared finalized output once, as it appears on the wire.
	if input, err := chatGPTInput(prompt); err == nil {
		return len(input)
	}
	return fantasyConversationBytes(prompt)
}

func chatGPTInputPart(role fantasy.MessageRole, part fantasy.MessagePart) (map[string]string, error) {
	switch part.GetType() {
	case fantasy.ContentTypeText:
		text, ok := fantasy.AsMessagePart[fantasy.TextPart](part)
		if !ok {
			return nil, errors.New("invalid ChatGPT text history")
		}
		if role == fantasy.MessageRoleSystem {
			role = "developer"
		}
		return map[string]string{"role": string(role), "content": text.Text}, nil
	case fantasy.ContentTypeToolCall:
		tool, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part)
		if !ok || tool.ProviderExecuted {
			return nil, errors.New("invalid ChatGPT local tool history")
		}
		return map[string]string{
			"type": "function_call", "call_id": tool.ToolCallID,
			"namespace": chatGPTToolNamespace, "name": tool.ToolName, "arguments": tool.Input,
		}, nil
	case fantasy.ContentTypeToolResult:
		tool, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
		if !ok || tool.ProviderExecuted {
			return nil, errors.New("invalid ChatGPT local tool result")
		}
		var output string
		if text, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tool.Output); ok {
			output = text.Text
		} else if failure, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tool.Output); ok && failure.Error != nil {
			output = failure.Error.Error()
		} else {
			return nil, errors.New("ChatGPT Roast supports text tool results only")
		}
		return map[string]string{"type": "function_call_output", "call_id": tool.ToolCallID, "output": output}, nil
	default:
		return nil, fmt.Errorf("ChatGPT Roast cannot replay unrecognized history type %q", part.GetType())
	}
}
