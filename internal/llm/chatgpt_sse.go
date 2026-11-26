package llm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/fantasy"
	fantasyopenai "charm.land/fantasy/providers/openai"
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
			if !errors.Is(err, io.EOF) {
				if interruption := chatGPTInterruption(err); interruption != nil {
					return nil, interruption
				}
				return nil, errors.New("ChatGPT connection failed while reading the response stream")
			}
			// An event without a terminating blank line is incomplete.
			if b.call.output == nil {
				return nil, errors.New("ChatGPT stream ended without response.completed; no tools were executed")
			}
			if line != "" || frame.Len() != 0 {
				return nil, errors.New("ChatGPT stream ended with an unterminated frame after response.completed")
			}
			return nil, io.EOF
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
				OutputDetails struct {
					Reasoning int64 `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if !uniqueJSONObject(data, foldJSONName) || json.Unmarshal(data, &event) != nil || event.Type == "" {
		return errors.New("invalid ChatGPT stream event")
	}
	kind := event.Type
	if named && name != kind {
		return errors.New("ChatGPT stream event name does not match its payload")
	}
	response := event.Response
	completed := kind == "response.completed"
	// Failed and incomplete responses can still spend plan usage. Retain the
	// provider's terminal count when no successful SDK finish reaches Roast.
	switch kind {
	case "response.completed", "response.failed", "response.incomplete":
		if u := response.Usage; u != nil {
			input, output, cached, reasoning := u.Input, u.Output, u.Details.Cached, u.OutputDetails.Reasoning
			valid := input >= 0 && output >= 0 && cached >= 0 && cached <= input && reasoning >= 0
			if !valid && completed {
				return errors.New("ChatGPT reported inconsistent token usage")
			}
			if valid {
				output, total := fantasyopenai.FoldDisjointReasoning(output, reasoning, input+output)
				b.call.usage = &fantasy.Usage{
					InputTokens: input - cached, CacheReadTokens: cached, OutputTokens: output,
					ReasoningTokens: reasoning, TotalTokens: total,
				}
			}
		}
	}
	// The SDK treats any top-level error field as an error, including null.
	// Keep diagnostics out of SDK errors and raw response capture.
	if kind == "response.failed" || kind == "error" ||
		len(event.Error) != 0 || jsonValuePresent(response.Error) {
		return newChatGPTError(http.StatusOK, data)
	}
	if kind == "response.incomplete" {
		return errors.New("ChatGPT response was incomplete; no tools were executed (max_output_tokens is unsupported on this route)")
	}
	if len(event.Item) != 0 {
		if _, err := decodeChatGPTOutputItem(event.Item, false); err != nil {
			return err
		}
	}
	seenItemIDs := make(map[string]bool)
	for _, item := range response.Output {
		output, err := decodeChatGPTOutputItem(item, completed)
		if err != nil {
			return err
		}
		if !completed {
			continue
		}
		if output.ID != "" {
			if seenItemIDs[output.ID] {
				return errors.New("ChatGPT finalized output contains duplicate item IDs")
			}
			seenItemIDs[output.ID] = true
		}
		if output.Type == "function_call" {
			b.call.completedCalls = append(b.call.completedCalls, output)
		}
	}
	if completed {
		if response.Output == nil {
			return errors.New("ChatGPT finalized output is missing")
		}
		if response.Status != "completed" {
			return errors.New("ChatGPT completion event has a non-completed status")
		}
		if jsonValuePresent(response.IncompleteDetails) {
			return errors.New("ChatGPT completion event reported an incomplete response")
		}
		output := response.Output
		b.call.output = &output
	}
	return nil
}

// Protocol envelopes use folded names to match Go's struct decoder. Metadata
// and tool arguments preserve case-sensitive names.
func uniqueJSONObject(data []byte, foldKey func(string) string) bool {
	if !utf8.Valid(data) || !json.Valid(data) || !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return uniqueJSONValue(decoder, foldKey)
}

func uniqueJSONValue(decoder *json.Decoder, foldKey func(string) string) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	if token != json.Delim('{') && token != json.Delim('[') {
		return true
	}
	var fields map[string]bool
	if token == json.Delim('{') {
		fields = make(map[string]bool)
	}
	for decoder.More() {
		foldChild := foldKey
		if fields != nil {
			rawKey, err := decoder.Token()
			name, ok := rawKey.(string)
			if err != nil || !ok {
				return false
			}
			if foldKey != nil {
				name = foldKey(name)
				if name == "metadata" {
					foldChild = nil
				}
			}
			if fields[name] {
				return false
			}
			fields[name] = true
		}
		if !uniqueJSONValue(decoder, foldChild) {
			return false
		}
	}
	_, err = decoder.Token()
	return err == nil
}

func foldJSONName(name string) string {
	return strings.Map(func(r rune) rune {
		next := unicode.SimpleFold(r)
		for next > r {
			r, next = next, unicode.SimpleFold(next)
		}
		if 'A' <= next && next <= 'Z' {
			return next + ('a' - 'A')
		}
		return next
	}, name)
}

func jsonValuePresent(raw json.RawMessage) bool {
	return len(raw) != 0 && !bytes.Equal(raw, []byte("null"))
}

type chatGPTOutputItem struct {
	ID        string
	Type      string
	Role      string
	Namespace string
	Status    string
	CallID    string
	Name      string
	Arguments string
}

func (item *chatGPTOutputItem) decode(raw json.RawMessage) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return false
	}
	fields := map[string]*string{
		"id": &item.ID, "type": &item.Type, "role": &item.Role, "namespace": &item.Namespace,
		"status": &item.Status, "call_id": &item.CallID, "name": &item.Name, "arguments": &item.Arguments,
	}
	var ignored json.RawMessage
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return false
		}
		var destination any = &ignored
		key := foldJSONName(name)
		if field, known := fields[key]; known {
			if name != key {
				return false
			}
			destination = field
		}
		if err := decoder.Decode(destination); err != nil {
			return false
		}
	}
	token, err := decoder.Token()
	return err == nil && token == json.Delim('}')
}

func decodeChatGPTOutputItem(raw json.RawMessage, final bool) (chatGPTOutputItem, error) {
	var item chatGPTOutputItem
	if !item.decode(raw) {
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
		if final && !uniqueJSONObject([]byte(item.Arguments), nil) {
			return item, errors.New("ChatGPT finalized output contains invalid tool arguments")
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
