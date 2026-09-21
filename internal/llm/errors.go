package llm

import "errors"

// ErrNoFinalCall means a Converse loop hit MaxTurns without the model
// calling the final tool. Callers decide the policy (the verify stage
// fails open; the finder reports a note).
var ErrNoFinalCall = errors.New("llm: tool loop ended without a final tool call")

// ErrWorkDeadline marks a Converse loop cut off by ConverseOpts.WorkDeadline.
// Always wrapped together with ErrNoFinalCall (the deadline fired AND the
// wrap-up turn produced no submission) so errors.Is matches both the generic
// no-submission policy and the specific stop reason.
var ErrWorkDeadline = errors.New("llm: work deadline reached")

// ErrAuth wraps 401/403 from an inference endpoint.
var ErrAuth = errors.New("llm: authentication failed")

// ErrEndpoint marks a non-auth inference HTTP or transport failure.
var ErrEndpoint = errors.New("llm: inference endpoint failure")
