package label

import (
	"testing"

	"github.com/bluesky-social/roast/internal/review"
)

func validLabel() Label {
	return Label{
		FindingID:   "11111111-1111-4111-8111-111111111111",
		Correctness: CorrectnessGenuine,
		Adjudicator: "alice",
		Pass:        1,
	}
}

func TestValidate_OK(t *testing.T) {
	if err := validate(validLabel()); err != nil {
		t.Fatalf("expected valid label to pass, got: %v", err)
	}
}

func TestValidate_CorrectnessRequired(t *testing.T) {
	l := validLabel()
	l.Correctness = ""
	if err := validate(l); err == nil {
		t.Fatal("expected error for empty correctness")
	}
	l.Correctness = "wrong"
	if err := validate(l); err == nil {
		t.Fatal("expected error for invalid correctness")
	}
}

func TestValidate_FalseReasonOnlyWithFalse(t *testing.T) {
	l := validLabel()
	l.Correctness = CorrectnessGenuine
	l.FalseReason = FalseReasonOther
	if err := validate(l); err == nil {
		t.Fatal("expected error: false_reason set but correctness != false")
	}

	l.Correctness = CorrectnessFalse
	l.FalseReason = "not-a-reason"
	if err := validate(l); err == nil {
		t.Fatal("expected error for invalid false_reason")
	}

	l.FalseReason = FalseReasonQuoteConfabulation
	if err := validate(l); err != nil {
		t.Fatalf("expected valid false+reason to pass, got: %v", err)
	}
}

func TestValidate_SeverityOverride(t *testing.T) {
	l := validLabel()
	l.SeverityOverride = review.Severity("NOT_A_SEVERITY")
	if err := validate(l); err == nil {
		t.Fatal("expected error for invalid severity_override")
	}
	l.SeverityOverride = review.SeverityCritical
	if err := validate(l); err != nil {
		t.Fatalf("expected valid severity_override to pass, got: %v", err)
	}
}

func TestValidate_Pass(t *testing.T) {
	l := validLabel()
	l.Pass = 0
	if err := validate(l); err == nil {
		t.Fatal("expected error for pass=0")
	}
	l.Pass = 3
	if err := validate(l); err == nil {
		t.Fatal("expected error for pass=3")
	}
	l.Pass = 2
	if err := validate(l); err != nil {
		t.Fatalf("expected pass=2 to be valid, got: %v", err)
	}
}

func TestValidate_RequiresFindingIDAndAdjudicator(t *testing.T) {
	l := validLabel()
	l.FindingID = ""
	if err := validate(l); err == nil {
		t.Fatal("expected error for missing finding id")
	}

	l = validLabel()
	l.Adjudicator = ""
	if err := validate(l); err == nil {
		t.Fatal("expected error for missing adjudicator")
	}
}

// TestValidate_FindingIDMustBeUUID guards the storage invariant directly:
// a row keyed by an alias or fingerprint would silently never resolve
// (Resolve matches against the UUIDs read from result.json), so Append's
// sole gate must reject non-UUID finding ids rather than merely non-empty
// ones.
func TestValidate_FindingIDMustBeUUID(t *testing.T) {
	l := validLabel()
	l.FindingID = "R-8d8d19" // an alias, not a UUID
	if err := validate(l); err == nil {
		t.Fatal("expected error: finding id must be a UUID, not an alias")
	}

	l.FindingID = "66e28848b1e74c9a" // a fingerprint, not a UUID
	if err := validate(l); err == nil {
		t.Fatal("expected error: finding id must be a UUID, not a fingerprint")
	}
}
