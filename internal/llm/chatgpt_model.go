package llm

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"

	"charm.land/fantasy"
)

type chatGPTClient struct{ *client }

type chatGPTDeadlineKey struct{}

type chatGPTDeadline struct {
	session string
	reached bool
	failure error
}

func (c *chatGPTClient) Converse(ctx context.Context, system, user string, opts ConverseOpts, out any) error {
	if !opts.WorkDeadline.IsZero() {
		deadline := &chatGPTDeadline{}
		ctx = context.WithValue(ctx, chatGPTDeadlineKey{}, deadline)
		onDeadline := opts.OnWorkDeadline
		opts.OnWorkDeadline = func() {
			deadline.reached = true
			if onDeadline != nil {
				onDeadline()
			}
		}
	}
	return c.client.Converse(ctx, system, user, opts, out)
}

const chatGPTOutputKey = "roast_chatgpt_output"

type chatGPTCallKey struct{}
type chatGPTCall struct {
	prompt         fantasy.Prompt
	output         *chatGPTOutput
	completedCalls []chatGPTOutputItem
	usage          *fantasy.Usage
	failure        error
	deadline       *chatGPTDeadline
}

func (c *chatGPTCall) recordFailure(err error) error {
	if c != nil {
		c.failure = err
	}
	return err
}

// Fantasy carries this metadata into the next turn's prompt. Keep the raw
// items in server order, including fields the SDK does not model.
type chatGPTOutput []json.RawMessage

func (*chatGPTOutput) Options()                      {}
func (o chatGPTOutput) MarshalJSON() ([]byte, error) { return json.Marshal([]json.RawMessage(o)) }
func (o *chatGPTOutput) UnmarshalJSON(b []byte) error {
	return json.Unmarshal(b, (*[]json.RawMessage)(o))
}

type chatGPTModel struct{ fantasy.LanguageModel }

func (m *chatGPTModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	deadline, _ := ctx.Value(chatGPTDeadlineKey{}).(*chatGPTDeadline)
	if deadline != nil {
		// A tool can start another agent run with this context. The deadline
		// state belongs to the outer run's session.
		session, _ := ctx.Value(sessionContextKey{}).(string)
		switch {
		case deadline.session == "":
			deadline.session = session
		case deadline.session != session:
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
		return nil, state.rememberFailure(cmp.Or(state.failure, err))
	}
	return func(yield func(fantasy.StreamPart) bool) {
		state.stream(stream, yield)
	}, nil
}

func (c *chatGPTCall) stream(stream fantasy.StreamResponse, yield func(fantasy.StreamPart) bool) {
	calls := make(map[string]fantasy.StreamPart)
	var finish *fantasy.StreamPart
	for part := range stream {
		// The finalized output carries replay metadata, including any encrypted
		// reasoning the SDK also put on individual parts.
		part.ProviderMetadata = nil
		switch part.Type {
		case fantasy.StreamPartTypeError:
			c.yieldError(yield, cmp.Or(c.failure, part.Error))
			return
		case fantasy.StreamPartTypeToolCall:
			if _, exists := calls[part.ID]; exists || part.ID == "" || part.ProviderExecuted {
				c.yieldError(yield, errors.New("invalid ChatGPT streamed tool call"))
				return
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
	if c.failure != nil {
		c.yieldError(yield, c.failure)
		return
	}
	if finish == nil || c.output == nil {
		c.yieldError(yield, fantasy.NewIncompleteStreamError())
		return
	}
	ordered, err := c.finalizedToolCalls(calls)
	if err != nil {
		c.yieldError(yield, err)
		return
	}
	for _, call := range ordered {
		if !yield(call) {
			return
		}
	}
	// Place the replay metadata after the provider content and before the
	// agent can add a local tool call to the turn.
	const id = "roast-finalized-output"
	if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningStart, ID: id}) ||
		!yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningEnd, ID: id, ProviderMetadata: fantasy.ProviderMetadata{chatGPTOutputKey: c.output}}) {
		return
	}
	yield(*finish)
}

func (c *chatGPTCall) yieldError(yield func(fantasy.StreamPart) bool, err error) {
	c.rememberFailure(err)
	// Report usage from the failed attempt before propagating the error.
	if c.usage != nil && !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonError, Usage: *c.usage}) {
		return
	}
	yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
}

func (c *chatGPTCall) rememberFailure(err error) error {
	if c.deadline != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		c.deadline.failure = err
	}
	return err
}

func (c *chatGPTCall) finalizedToolCalls(calls map[string]fantasy.StreamPart) ([]fantasy.StreamPart, error) {
	// Parallel calls can finish in a different order. Dispatch them in the
	// order in which the server finalized them and Roast will replay them.
	ordered := make([]fantasy.StreamPart, 0, len(calls))
	for _, item := range c.completedCalls {
		call, ok := calls[item.CallID]
		if !ok || call.ToolCallName != item.Name || call.ToolCallInput != item.Arguments {
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
