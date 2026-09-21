// Package label stores human adjudication labels for review findings — the
// correctness verdict a person assigns to one of roast's own proposals,
// append-only so no relabeling ever destroys the trail a κ statistic or an
// audit needs.
//
// Resolve implements the scoring rules for append-only corrections and
// repeated blind adjudication.
package label

import (
	"fmt"
	"time"

	"github.com/bluesky-social/roast/internal/review"
)

// Correctness is the primary adjudication axis — deliberately NOT folded
// into one flat enum together with FalseReason: a verdict and its cause are
// orthogonal facts.
type Correctness string

const (
	CorrectnessGenuine   Correctness = "genuine"
	CorrectnessFalse     Correctness = "false"
	CorrectnessUncertain Correctness = "uncertain"
)

// FalseReason further classifies a Correctness: false label. Only
// meaningful, and only valid, alongside CorrectnessFalse.
type FalseReason string

const (
	FalseReasonQuoteConfabulation FalseReason = "quote_confabulation"
	FalseReasonStandardMisapplied FalseReason = "standard_misapplied"
	FalseReasonStaleAssumption    FalseReason = "stale_assumption"
	FalseReasonOther              FalseReason = "other"
)

// Label is one append-only adjudication row for a single finding. A
// re-adjudication is a NEW row (Supersedes naming the row it corrects);
// nothing is ever erased or rewritten in place.
type Label struct {
	// RowID is this row's own stable identity, minted at Append time.
	RowID string `json:"row_id"`
	// RunID is the run this finding belongs to, denormalized onto the row
	// so a single line is self-contained even though the file itself is
	// already keyed by run id.
	RunID string `json:"run_id"`
	// FindingID is review.Finding.ID — an immutable UUID. NEVER the display
	// alias and NEVER the fingerprint: identity ≠ similarity, and the
	// fingerprint is a similarity heuristic that tolerates collisions on
	// purpose, so it must never key an adjudication label.
	FindingID string `json:"finding_id"`

	Correctness      Correctness     `json:"correctness"`
	FalseReason      FalseReason     `json:"false_reason,omitempty"`
	SeverityOverride review.Severity `json:"severity_override,omitempty"`

	Adjudicator   string `json:"adjudicator"`
	AdjudicatedAt string `json:"adjudicated_at"` // RFC3339Nano, UTC

	// Pass is 1 for a first adjudication, 2 for an intra-rater
	// re-adjudication (the seeded blind re-queue). Pass 2 rows NEVER score —
	// they exist solely to compute repeatability.
	Pass int `json:"pass"`
	// Supersedes names the row id this one deliberately corrects. Only a
	// pass:1 row with Supersedes set can override an earlier scoring row;
	// write order alone never does — a correction is a deliberate act.
	Supersedes string `json:"supersedes,omitempty"`
}

// validate enforces the schema's invariants before a row is ever written.
func validate(l Label) error {
	switch l.Correctness {
	case CorrectnessGenuine, CorrectnessFalse, CorrectnessUncertain:
	default:
		return fmt.Errorf("label: correctness must be genuine|false|uncertain, got %q", l.Correctness)
	}
	if l.FalseReason != "" {
		switch l.FalseReason {
		case FalseReasonQuoteConfabulation, FalseReasonStandardMisapplied, FalseReasonStaleAssumption, FalseReasonOther:
		default:
			return fmt.Errorf("label: false_reason must be quote_confabulation|standard_misapplied|stale_assumption|other, got %q", l.FalseReason)
		}
		if l.Correctness != CorrectnessFalse {
			return fmt.Errorf("label: false_reason is only valid when correctness=false, got correctness=%q", l.Correctness)
		}
	}
	if l.SeverityOverride != "" {
		if _, ok := review.ParseSeverity(string(l.SeverityOverride)); !ok {
			return fmt.Errorf("label: severity_override %q is not a valid severity", l.SeverityOverride)
		}
	}
	if l.Pass != 1 && l.Pass != 2 {
		return fmt.Errorf("label: pass must be 1 or 2, got %d", l.Pass)
	}
	// FindingID must be the immutable UUID (never the alias or fingerprint):
	// an alias- or fingerprint-keyed row would silently never resolve,
	// since Resolve matches against the UUIDs read from result.json.
	// uuidRE is defined in findings.go, same package.
	if !uuidRE.MatchString(l.FindingID) {
		return fmt.Errorf("label: finding id must be a UUID, got %q", l.FindingID)
	}
	if l.Adjudicator == "" {
		return fmt.Errorf("label: adjudicator is required")
	}
	// AdjudicatedAt is documented as RFC3339Nano and is what Resolve orders
	// superseding rows by. Append mints it when unset but preserves any
	// non-empty caller value, so without this an unparseable timestamp
	// persists and silently perturbs supersession order.
	if l.AdjudicatedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, l.AdjudicatedAt); err != nil {
			return fmt.Errorf("label: adjudicated_at must be RFC3339Nano, got %q", l.AdjudicatedAt)
		}
	}
	return nil
}
