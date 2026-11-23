package llm

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"charm.land/fantasy"
)

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
		// Nested reviews may inherit the context. Use the state only in the
		// agent session that first bound it.
		session, _ := ctx.Value(sessionContextKey{}).(string)
		if deadline.session == "" {
			deadline.session = session
		}
		switch {
		case deadline.session != session:
			deadline = nil
		case deadline.reached && deadline.failure != nil:
			// The shared agent may start wrap-up after the cutoff. Reuse
			// the reported failure before opening another request.
			deadline.usedFailure = true
			return nil, chatGPTNonRetryableError{deadline.failure}
		default:
			deadline.failure = nil
		}
	}
	state := &chatGPTCall{prompt: call.Prompt, deadline: deadline}
	ctx = context.WithValue(ctx, chatGPTCallKey{}, state)
	stream, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		return nil, state.reportError(ctx, cmp.Or(state.failure, err))
	}
	return func(yield func(fantasy.StreamPart) bool) {
		state.stream(ctx, stream, yield)
	}, nil
}

func (c *chatGPTCall) stream(ctx context.Context, stream fantasy.StreamResponse, yield func(fantasy.StreamPart) bool) {
	calls := make(map[string]fantasy.StreamPart)
	var finish *fantasy.StreamPart
	for part := range stream {
		// The finalized output carries replay metadata, including any encrypted
		// reasoning the SDK also put on individual parts.
		part.ProviderMetadata = nil
		switch part.Type {
		case fantasy.StreamPartTypeError:
			c.yieldError(ctx, yield, cmp.Or(c.failure, part.Error))
			return
		case fantasy.StreamPartTypeToolCall:
			if _, exists := calls[part.ID]; exists || part.ID == "" || part.ProviderExecuted {
				c.yieldError(ctx, yield, errors.New("invalid ChatGPT streamed tool call"))
				return
			}
			calls[part.ID] = part
		case fantasy.StreamPartTypeFinish:
			if c.usage != nil {
				part.Usage = *c.usage
			}
			finish = &part
			c.usage = &part.Usage
		default:
			if !yield(part) {
				return
			}
		}
	}

	// The SDK may stop after a terminal event without reporting a guard error.
	if c.failure != nil {
		c.yieldError(ctx, yield, c.failure)
		return
	}
	if finish == nil || c.output == nil {
		c.yieldError(ctx, yield, fantasy.NewIncompleteStreamError())
		return
	}
	ordered, err := c.finalizedToolCalls(calls)
	if err != nil {
		c.yieldError(ctx, yield, err)
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

func (c *chatGPTCall) yieldError(ctx context.Context, yield func(fantasy.StreamPart) bool, err error) {
	// Report usage from the failed attempt before propagating the error.
	if c.usage != nil && !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonError, Usage: *c.usage}) {
		return
	}
	yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: c.reportError(ctx, err)})
}

func (c *chatGPTCall) reportError(ctx context.Context, err error) error {
	if _, reported := errors.AsType[providerCallError](err); !reported {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		err = providerCallError{err}
	}
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		err = fmt.Errorf("%w: %w", err, ctxErr)
	}
	if c.deadline != nil && !c.deadline.reached {
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
