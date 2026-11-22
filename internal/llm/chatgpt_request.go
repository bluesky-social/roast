package llm

import (
	"encoding/json"
	"errors"
	"fmt"

	"charm.land/fantasy"
)

// Copy the fields Roast uses from Fantasy's request. Build the plan-specific
// envelope here so new SDK defaults cannot enable storage or change billing.
func encodeChatGPTRequest(body []byte, prompt fantasy.Prompt) ([]byte, error) {
	var sdk struct {
		Model      string                       `json:"model"`
		ToolChoice json.RawMessage              `json:"tool_choice"`
		Tools      []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &sdk); err != nil {
		return nil, fmt.Errorf("decode ChatGPT request: %w", err)
	}
	if sdk.Model == "" {
		return nil, errors.New("ChatGPT request is missing its model")
	}
	// Fantasy's input omits encrypted reasoning and tool namespaces.
	input, err := chatGPTInput(prompt)
	if err != nil {
		return nil, err
	}
	request := map[string]any{
		"model": sdk.Model, "input": input, "store": false, "stream": true,
		"include": []string{"reasoning.encrypted_content"},
	}
	if len(sdk.ToolChoice) > 0 {
		request["tool_choice"] = sdk.ToolChoice
	}
	for _, tool := range sdk.Tools {
		if string(tool["type"]) != `"function"` {
			return nil, errors.New("ChatGPT Roast supports only local function tools")
		}
	}
	if len(sdk.Tools) > 0 {
		request["tools"] = []map[string]any{{
			"type": "namespace", "name": chatGPTToolNamespace,
			"description": "Roast code review and final submission tools.", "tools": sdk.Tools,
		}}
	}
	return json.Marshal(request)
}

// chatGPTInput encodes the full conversation for a stateless Responses call.
func chatGPTInput(prompt fantasy.Prompt) ([]any, error) {
	input := make([]any, 0, len(prompt))
	for _, message := range prompt {
		parts := message.Content
		for i, part := range parts {
			if output, ok := part.Options()[chatGPTOutputKey].(*chatGPTOutput); ok {
				if message.Role != fantasy.MessageRoleAssistant || output == nil {
					return nil, errors.New("ChatGPT history is missing finalized assistant output")
				}
				for _, item := range *output {
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
	return input, nil
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
