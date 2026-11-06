package review

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bluesky-social/roast/internal/llm"
)

type cutoffClient struct{}

func (cutoffClient) Name() string     { return "cutoff-test" }
func (cutoffClient) Usage() llm.Usage { return llm.Usage{} }
func (cutoffClient) CompleteStructured(context.Context, string, string, string, any) error {
	panic("not called")
}
func (cutoffClient) Converse(_ context.Context, _, _ string, opts llm.ConverseOpts, out any) error {
	opts.OnWorkDeadline()
	response := out.(*FindingsResponse)
	*response = FindingsResponse{Assessment: AssessmentNoFindings, Findings: []WireFinding{}}
	return nil
}

func TestSuccessfulDeadlineWrapUpIsIncomplete(t *testing.T) {
	result, err := Find(t.Context(), cutoffClient{}, FindInput{
		Interactive: true,
		MaxTurns:    2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Incomplete || result.StopReason != "deadline" {
		t.Fatalf("incomplete=%v stop_reason=%q", result.Incomplete, result.StopReason)
	}
}

type noSubmissionClient struct{ cutoffClient }

func (noSubmissionClient) Converse(context.Context, string, string, llm.ConverseOpts, any) error {
	return fmt.Errorf("%w: finder-model: the response ended with finish reason %q", llm.ErrNoFinalCall, "length")
}

func TestNoSubmissionNoteKeepsTheCause(t *testing.T) {
	result, err := Find(t.Context(), noSubmissionClient{}, FindInput{Interactive: true, MaxTurns: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Incomplete || len(result.Notes) != 1 || !strings.Contains(result.Notes[0], `finish reason "length"`) {
		t.Fatalf("incomplete=%v notes=%q", result.Incomplete, result.Notes)
	}
}
