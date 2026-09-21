package label

import "testing"

func TestResolve_NoLabelIsUnadjudicated(t *testing.T) {
	_, ok := Resolve(nil, findingA, "alice")
	if ok {
		t.Fatal("expected ok=false when no rows exist for the finding")
	}

	rows := []Label{
		{FindingID: findingB, Correctness: CorrectnessGenuine, Adjudicator: "alice", Pass: 1, AdjudicatedAt: "2026-01-01T00:00:00Z"},
	}
	_, ok = Resolve(rows, findingA, "alice")
	if ok {
		t.Fatal("expected ok=false: rows exist but none for this finding id")
	}
}

func TestResolve_PlainPass1Scores(t *testing.T) {
	rows := []Label{
		{FindingID: findingA, Correctness: CorrectnessGenuine, Adjudicator: "alice", Pass: 1, AdjudicatedAt: "2026-01-01T00:00:00Z"},
	}
	got, ok := Resolve(rows, findingA, "alice")
	if !ok {
		t.Fatal("expected a plain pass:1 row to score")
	}
	if got.Correctness != CorrectnessGenuine {
		t.Fatalf("expected genuine, got %v", got.Correctness)
	}
}

func TestResolve_SupersededRowWins(t *testing.T) {
	rows := []Label{
		{RowID: "r1", FindingID: findingA, Correctness: CorrectnessGenuine, Adjudicator: "alice", Pass: 1, AdjudicatedAt: "2026-01-01T00:00:00Z"},
		{RowID: "r2", FindingID: findingA, Correctness: CorrectnessFalse, FalseReason: FalseReasonOther, Adjudicator: "alice", Pass: 1, Supersedes: "r1", AdjudicatedAt: "2026-01-02T00:00:00Z"},
	}
	got, ok := Resolve(rows, findingA, "alice")
	if !ok {
		t.Fatal("expected a scoring row")
	}
	if got.Correctness != CorrectnessFalse || got.RowID != "r2" {
		t.Fatalf("expected the superseding row (r2, false) to win, got %+v", got)
	}
}

func TestResolve_NewestSupersederWinsAmongMultiple(t *testing.T) {
	rows := []Label{
		{RowID: "r1", FindingID: findingA, Correctness: CorrectnessGenuine, Adjudicator: "alice", Pass: 1, AdjudicatedAt: "2026-01-01T00:00:00Z"},
		{RowID: "r2", FindingID: findingA, Correctness: CorrectnessFalse, FalseReason: FalseReasonOther, Adjudicator: "alice", Pass: 1, Supersedes: "r1", AdjudicatedAt: "2026-01-02T00:00:00Z"},
		{RowID: "r3", FindingID: findingA, Correctness: CorrectnessUncertain, Adjudicator: "alice", Pass: 1, Supersedes: "r1", AdjudicatedAt: "2026-01-03T00:00:00Z"},
	}
	got, ok := Resolve(rows, findingA, "alice")
	if !ok {
		t.Fatal("expected a scoring row")
	}
	if got.RowID != "r3" || got.Correctness != CorrectnessUncertain {
		t.Fatalf("expected the newest-by-adjudicated_at superseder (r3) to win, got %+v", got)
	}
}

func TestResolve_Pass2NeverScores(t *testing.T) {
	rows := []Label{
		{RowID: "r1", FindingID: findingA, Correctness: CorrectnessGenuine, Adjudicator: "alice", Pass: 1, AdjudicatedAt: "2026-01-01T00:00:00Z"},
		{RowID: "r2", FindingID: findingA, Correctness: CorrectnessFalse, FalseReason: FalseReasonOther, Adjudicator: "alice", Pass: 2, AdjudicatedAt: "2026-01-02T00:00:00Z"},
	}
	got, ok := Resolve(rows, findingA, "alice")
	if !ok {
		t.Fatal("expected the pass:1 row to score even though a later pass:2 row exists")
	}
	if got.RowID != "r1" || got.Correctness != CorrectnessGenuine {
		t.Fatalf("expected pass:1 row to score untouched by the pass:2 row, got %+v", got)
	}
}

func TestResolve_Pass2OnlyMeansUnadjudicated(t *testing.T) {
	rows := []Label{
		{RowID: "r1", FindingID: findingA, Correctness: CorrectnessFalse, FalseReason: FalseReasonOther, Adjudicator: "alice", Pass: 2, AdjudicatedAt: "2026-01-01T00:00:00Z"},
	}
	_, ok := Resolve(rows, findingA, "alice")
	if ok {
		t.Fatal("expected ok=false: only a pass:2 row exists, so the finding is unadjudicated")
	}
}

func TestResolve_NonPrimaryAdjudicatorNeverScores(t *testing.T) {
	rows := []Label{
		{RowID: "r1", FindingID: findingA, Correctness: CorrectnessGenuine, Adjudicator: "bob", Pass: 1, AdjudicatedAt: "2026-01-01T00:00:00Z"},
	}
	_, ok := Resolve(rows, findingA, "alice")
	if ok {
		t.Fatal("expected ok=false: the only row is from a non-primary adjudicator")
	}

	// It also must not be picked up when alice is queried alongside bob's row
	// present in the same slice.
	rows = append(rows, Label{RowID: "r2", FindingID: findingA, Correctness: CorrectnessUncertain, Adjudicator: "alice", Pass: 1, AdjudicatedAt: "2026-01-02T00:00:00Z"})
	got, ok := Resolve(rows, findingA, "alice")
	if !ok || got.RowID != "r2" {
		t.Fatalf("expected alice's own row to score, ignoring bob's, got ok=%v got=%+v", ok, got)
	}
}
