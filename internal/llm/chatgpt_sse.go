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

// Validate complete SSE frames before passing their unchanged bytes to the
// SDK. In particular, response.incomplete must never masquerade as a successful
// tool submission even if an SDK maps its missing finish reason to "stop".
type chatGPTStreamBody struct {
	body      io.ReadCloser
	reader    *bufio.Reader
	pending   []byte
	completed bool
	bytesRead int
	err       error
	fail      func(error) error
	call      *chatGPTCall
	owner     *responseBody
}

func (b *chatGPTStreamBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(b.pending) == 0 && b.err == nil {
		b.pending, b.err = b.readEvent()
		if b.err != nil && !errors.Is(b.err, io.EOF) {
			// Preserve the guard error if the SDK maps the read failure.
			b.fail(b.err)
		}
	}
	if len(b.pending) > 0 {
		n := copy(p, b.pending)
		b.pending = b.pending[n:]
		return n, nil
	}
	return 0, b.err
}

// readEvent keeps the original SSE bytes for capture. Only data fields are
// joined for validation. Comments and line endings pass through unchanged.
func (b *chatGPTStreamBody) readEvent() ([]byte, error) {
	var frame bytes.Buffer
	var data []string
	var eventName string
	for {
		line, err := b.readLine()
		if err != nil {
			// An event without a terminating blank line is incomplete.
			if errors.Is(err, io.EOF) {
				if !b.completed {
					return nil, errors.New("ChatGPT stream ended without response.completed; no tools were executed")
				}
				if line != "" || frame.Len() != 0 {
					return nil, errors.New("ChatGPT stream ended with an unterminated frame after response.completed")
				}
			}
			return nil, err
		}
		if frame.Len()+len(line) > maxCapturedResponseSize {
			return nil, errors.New("ChatGPT stream event exceeds the bounded size")
		}
		frame.WriteString(line)
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(value, " "))
		} else if value, ok := strings.CutPrefix(line, "event:"); ok {
			eventName = strings.TrimPrefix(value, " ")
		}
		if line != "" {
			continue
		}
		if len(data) > 0 {
			if invalid := b.validate(eventName, []byte(strings.Join(data, "\n"))); invalid != nil {
				return nil, invalid
			}
		}
		return frame.Bytes(), nil
	}
}

// ReadSlice bounds allocation even when a peer never sends a newline.
func (b *chatGPTStreamBody) readLine() (string, error) {
	var line bytes.Buffer
	for {
		fragment, err := b.reader.ReadSlice('\n')
		if len(fragment) > maxCapturedResponseSize-b.bytesRead {
			return "", errors.New("ChatGPT response exceeds the bounded stream size")
		}
		b.bytesRead += len(fragment)
		line.Write(fragment)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line.String(), err
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
		if !b.completed {
			return errors.New("ChatGPT stream ended before response.completed")
		}
		return nil
	}
	if b.completed {
		return errors.New("ChatGPT sent an unexpected event after response.completed")
	}
	var event struct {
		Type     string          `json:"type"`
		Item     json.RawMessage `json:"item"`
		Response struct {
			Status            string            `json:"status"`
			Error             json.RawMessage   `json:"error"`
			IncompleteDetails json.RawMessage   `json:"incomplete_details"`
			Output            []json.RawMessage `json:"output"`
			Usage             *struct {
				Input   int64 `json:"input_tokens"`
				Output  int64 `json:"output_tokens"`
				Details struct {
					Cached int64 `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return errors.New("invalid ChatGPT stream event")
	}
	if named && name != event.Type {
		return errors.New("ChatGPT stream event name does not match its payload")
	}
	// Failed and incomplete responses can still spend plan usage. Retain the
	// provider's terminal count when no successful SDK finish reaches Roast.
	switch event.Type {
	case "response.completed", "response.failed", "response.incomplete":
		if u := event.Response.Usage; u != nil {
			valid := u.Input >= 0 && u.Output >= 0 && u.Details.Cached >= 0 && u.Details.Cached <= u.Input
			if !valid && event.Type == "response.completed" {
				return errors.New("ChatGPT reported inconsistent token usage")
			}
			if valid && b.owner != nil {
				b.owner.terminalUsage = &fantasy.Usage{InputTokens: u.Input - u.Details.Cached, CacheReadTokens: u.Details.Cached, OutputTokens: u.Output}
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
	if len(event.Item) > 0 {
		if err := validateChatGPTOutputItem(event.Item, false); err != nil {
			return err
		}
	}
	for _, item := range event.Response.Output {
		if err := validateChatGPTOutputItem(item, event.Type == "response.completed"); err != nil {
			return err
		}
	}
	if event.Type == "response.completed" {
		if event.Response.Output == nil {
			return errors.New("ChatGPT finalized output is missing")
		}
		if event.Response.Status != "completed" {
			return errors.New("ChatGPT completion event has a non-completed status")
		}
		if jsonValuePresent(event.Response.Error) || jsonValuePresent(event.Response.IncompleteDetails) {
			return errors.New("ChatGPT completion event reported an error or incomplete response")
		}
		b.completed = true
		if b.call != nil {
			b.call.output.items = event.Response.Output
		}
	}
	return nil
}

func jsonValuePresent(raw json.RawMessage) bool {
	return len(raw) != 0 && !bytes.Equal(raw, []byte("null"))
}

func validateChatGPTOutputItem(raw json.RawMessage, final bool) error {
	var item struct {
		Type      string `json:"type"`
		Role      string `json:"role"`
		Namespace string `json:"namespace"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return errors.New("invalid ChatGPT stream output item")
	}
	if final && item.Status != "" && item.Status != "completed" {
		return errors.New("ChatGPT finalized output contains an unfinished item; no tools were executed")
	}
	switch item.Type {
	case "function_call":
		if item.Namespace != chatGPTToolNamespace {
			return errors.New("ChatGPT returned a function outside the Roast tool namespace; no tools were executed")
		}
	case "message":
		if item.Role != "assistant" {
			return errors.New("ChatGPT returned an output message with an invalid role; no tools were executed")
		}
	case "reasoning":
	default:
		return errors.New("ChatGPT returned an unsupported output type; no tools were executed")
	}
	return nil
}
