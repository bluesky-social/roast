package llm

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
	prompt   fantasy.Prompt
	output   chatGPTOutput
	usage    *fantasy.Usage
	failure  error
	deadline *chatGPTDeadline
}

func (c *chatGPTCall) recordFailure(err error) error {
	if c != nil {
		c.failure = err
	}
	return err
}

// Store finalized output once, after the provider's content and before any
// tools added locally. It carries the server's order and encrypted reasoning
// through Fantasy's conversion to prompt messages.
type chatGPTOutput struct {
	items []json.RawMessage
}

func (*chatGPTOutput) Options()                          {}
func (o *chatGPTOutput) MarshalJSON() ([]byte, error)    { return json.Marshal(o.items) }
func (o *chatGPTOutput) UnmarshalJSON(data []byte) error { return json.Unmarshal(data, &o.items) }

type chatGPTModel struct{ fantasy.LanguageModel }

func (m *chatGPTModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	deadline, _ := ctx.Value(chatGPTDeadlineKey{}).(*chatGPTDeadline)
	if deadline != nil {
		// A tool can start another agent run with this context. The deadline
		// state belongs to the outer run's session.
		session, _ := ctx.Value(sessionContextKey{}).(string)
		if deadline.session == "" {
			deadline.session = session
		}
		if deadline.session != session {
			deadline = nil
		}
	}
	if deadline != nil {
		// The reserved wrap-up can follow a deadline interruption. Reuse a
		// provider failure already received at the cutoff without a new request.
		if deadline.reached && deadline.failure != nil {
			return nil, deadline.failure
		}
		deadline.failure = nil
	}
	state := &chatGPTCall{prompt: call.Prompt, deadline: deadline}
	ctx = context.WithValue(ctx, chatGPTCallKey{}, state)
	stream, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		return func(yield func(fantasy.StreamPart) bool) {
			state.yieldError(yield, nil, cmp.Or(state.failure, err))
		}, nil
	}
	return func(yield func(fantasy.StreamPart) bool) {
		state.stream(stream, yield)
	}, nil
}

func (c *chatGPTCall) stream(stream fantasy.StreamResponse, yield func(fantasy.StreamPart) bool) {
	calls := make(map[string]fantasy.StreamPart)
	var finish *fantasy.StreamPart
	var streamErr error
consume:
	for part := range stream {
		// The finalized output carries replay metadata, including any encrypted
		// reasoning the SDK also put on individual parts.
		part.ProviderMetadata = nil
		switch part.Type {
		case fantasy.StreamPartTypeError:
			streamErr = part.Error
			break consume
		case fantasy.StreamPartTypeToolCall:
			if _, exists := calls[part.ID]; exists || part.ID == "" || part.ProviderExecuted {
				streamErr = errors.New("invalid ChatGPT streamed tool call")
				break consume
			}
			calls[part.ID] = part
		case fantasy.StreamPartTypeFinish:
			finish = &part
		default:
			if !yield(part) {
				return
			}
		}
	}

	// The SDK may stop after a terminal event without reporting a guard error.
	if err := cmp.Or(c.failure, streamErr); err != nil {
		c.yieldError(yield, finish, err)
		return
	}
	if finish == nil {
		c.yieldError(yield, nil, fantasy.NewIncompleteStreamError())
		return
	}
	ordered, err := c.output.finalizedToolCalls(calls)
	if err != nil {
		c.yieldError(yield, finish, err)
		return
	}
	for _, call := range ordered {
		if !yield(call) {
			return
		}
	}
	const id = "roast-finalized-output"
	if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningStart, ID: id}) ||
		!yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningEnd, ID: id, ProviderMetadata: fantasy.ProviderMetadata{chatGPTOutputKey: &c.output}}) {
		return
	}
	yield(*finish)
}

func (c *chatGPTCall) yieldError(yield func(fantasy.StreamPart) bool, finish *fantasy.StreamPart, err error) {
	if c.deadline != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		c.deadline.failure = err
	}
	usage := c.usage
	if finish != nil {
		usage = &finish.Usage
	}
	// Report usage from the failed attempt before propagating the error.
	if usage != nil && !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonError, Usage: *usage}) {
		return
	}
	yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
}

func (o *chatGPTOutput) finalizedToolCalls(calls map[string]fantasy.StreamPart) ([]fantasy.StreamPart, error) {
	if o.items == nil {
		return nil, errors.New("ChatGPT stream ended without response.completed; no tools were executed")
	}
	// Parallel calls can finish in a different order. Dispatch them in the
	// order in which the server finalized them and Roast will replay them.
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
		parts := message.Content
		for i, part := range parts {
			if original, ok := part.Options()[chatGPTOutputKey].(*chatGPTOutput); ok {
				if message.Role != fantasy.MessageRoleAssistant || original == nil || original.items == nil {
					return nil, errors.New("ChatGPT history is missing finalized assistant output")
				}
				for _, item := range original.items {
					input = append(input, item)
				}
				// The marker replaces the provider's prefix. Any local nudge
				// appended by the agent still belongs to this assistant turn.
				parts = parts[i+1:]
				break
			}
		}
		for _, part := range parts {
			item, err := chatGPTInputPart(message.Role, part)
			if err != nil {
				return nil, err
			}
			input = append(input, item)
		}
	}
	return json.Marshal(input)
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
