package llm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"charm.land/fantasy"
)

// Validate complete SSE frames before passing their original bytes to the SDK
// and response capture.
type chatGPTStreamBody struct {
	body      io.ReadCloser
	reader    *bufio.Reader
	pending   []byte
	bytesRead int
	err       error
	call      *chatGPTCall
}

func newChatGPTStreamBody(body io.ReadCloser, call *chatGPTCall) *chatGPTStreamBody {
	// Limit the source before ReadString, including a stream with no newline.
	return &chatGPTStreamBody{body: body, reader: bufio.NewReader(io.LimitReader(body, maxCapturedResponseSize+1)), call: call}
}

func (b *chatGPTStreamBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(b.pending) == 0 && b.err == nil {
		b.pending, b.err = b.readEvent()
		if b.err != nil && !errors.Is(b.err, io.EOF) {
			// Preserve the guard error if the SDK maps the read failure.
			b.call.recordFailure(b.err)
		}
	}
	if b.err != nil {
		return 0, b.err
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

// readEvent keeps the original SSE bytes for capture. Only data fields are
// joined for validation. Comments and line endings pass through unchanged.
func (b *chatGPTStreamBody) readEvent() ([]byte, error) {
	var frame bytes.Buffer
	var data []string
	var eventName string
	for {
		line, err := b.reader.ReadString('\n')
		b.bytesRead += len(line)
		if b.bytesRead > maxCapturedResponseSize {
			return nil, errors.New("ChatGPT response exceeds the bounded stream size")
		}
		if err != nil {
			// An event without a terminating blank line is incomplete.
			if errors.Is(err, io.EOF) {
				if b.call.output == nil {
					return nil, errors.New("ChatGPT stream ended without response.completed; no tools were executed")
				}
				if line != "" || frame.Len() != 0 {
					return nil, errors.New("ChatGPT stream ended with an unterminated frame after response.completed")
				}
			}
			return nil, err
		}
		frame.WriteString(line)
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			if len(data) > 0 {
				if err := b.validate(eventName, []byte(strings.Join(data, "\n"))); err != nil {
					return nil, err
				}
			}
			return frame.Bytes(), nil
		}
		field, value, _ := strings.Cut(line, ":")
		switch field {
		case "data":
			data = append(data, strings.TrimPrefix(value, " "))
		case "event":
			eventName = strings.TrimPrefix(value, " ")
		}
	}
}

func (b *chatGPTStreamBody) Close() error { return b.body.Close() }

func (b *chatGPTStreamBody) validate(name string, data []byte) error {
	named := name != "" && name != "message"
	if bytes.Equal(data, []byte("[DONE]")) {
		if named && name != "done" {
			return errors.New("ChatGPT stream event name does not match its end marker")
		}
		if b.call.output == nil {
			return errors.New("ChatGPT stream ended before response.completed")
		}
		return nil
	}
	if b.call.output != nil {
		return errors.New("ChatGPT sent an unexpected event after response.completed")
	}
	var event struct {
		Type     string          `json:"type"`
		Item     json.RawMessage `json:"item"`
		Error    json.RawMessage `json:"error"`
		Response struct {
			Status            string          `json:"status"`
			Error             json.RawMessage `json:"error"`
			IncompleteDetails json.RawMessage `json:"incomplete_details"`
			Output            chatGPTOutput   `json:"output"`
			Usage             *struct {
				Input   int64 `json:"input_tokens"`
				Output  int64 `json:"output_tokens"`
				Details struct {
					Cached int64 `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &event); err != nil || event.Type == "" {
		return errors.New("invalid ChatGPT stream event")
	}
	if named && name != event.Type {
		return errors.New("ChatGPT stream event name does not match its payload")
	}
	completed := event.Type == "response.completed"
	// Failed and incomplete responses can still spend plan usage. Retain the
	// provider's terminal count when no successful SDK finish reaches Roast.
	switch event.Type {
	case "response.completed", "response.failed", "response.incomplete":
		if u := event.Response.Usage; u != nil {
			valid := u.Input >= 0 && u.Output >= 0 && u.Details.Cached >= 0 && u.Details.Cached <= u.Input
			if !valid && completed {
				return errors.New("ChatGPT reported inconsistent token usage")
			}
			if valid {
				b.call.usage = &fantasy.Usage{InputTokens: u.Input - u.Details.Cached, CacheReadTokens: u.Details.Cached, OutputTokens: u.Output}
			}
		}
	}
	switch event.Type {
	case "response.incomplete":
		return errors.New("ChatGPT response was incomplete; no tools were executed (max_output_tokens is unsupported on this route)")
	case "response.failed":
		return newChatGPTError(http.StatusOK, event.Response.Error)
	case "error":
		return newChatGPTError(http.StatusOK, data)
	}
	// The SDK includes the raw payload in errors from this envelope.
	if len(event.Error) != 0 {
		return newChatGPTError(http.StatusOK, data)
	}
	if len(event.Item) > 0 {
		if _, err := decodeChatGPTOutputItem(event.Item, false); err != nil {
			return err
		}
	}
	for _, item := range event.Response.Output {
		output, err := decodeChatGPTOutputItem(item, completed)
		if err != nil {
			return err
		}
		if completed && output.Type == "function_call" {
			b.call.completedCalls = append(b.call.completedCalls, output)
		}
	}
	if completed {
		if event.Response.Output == nil {
			return errors.New("ChatGPT finalized output is missing")
		}
		if event.Response.Status != "completed" {
			return errors.New("ChatGPT completion event has a non-completed status")
		}
		if jsonValuePresent(event.Response.Error) || jsonValuePresent(event.Response.IncompleteDetails) {
			return errors.New("ChatGPT completion event reported an error or incomplete response")
		}
		output := event.Response.Output
		b.call.output = &output
	}
	return nil
}

func jsonValuePresent(raw json.RawMessage) bool {
	return len(raw) != 0 && !bytes.Equal(raw, []byte("null"))
}

type chatGPTOutputItem struct {
	Type      string `json:"type"`
	Role      string `json:"role"`
	Namespace string `json:"namespace"`
	Status    string `json:"status"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func decodeChatGPTOutputItem(raw json.RawMessage, final bool) (chatGPTOutputItem, error) {
	var item chatGPTOutputItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return item, errors.New("invalid ChatGPT stream output item")
	}
	if final && item.Status != "" && item.Status != "completed" {
		return item, errors.New("ChatGPT finalized output contains an unfinished item")
	}
	switch item.Type {
	case "function_call":
		if item.Namespace != chatGPTToolNamespace {
			return item, errors.New("ChatGPT returned a function outside the Roast tool namespace")
		}
	case "message":
		if item.Role != "assistant" {
			return item, errors.New("ChatGPT returned an output message with an invalid role")
		}
	case "reasoning":
	default:
		return item, errors.New("ChatGPT returned an unsupported output type")
	}
	return item, nil
}
