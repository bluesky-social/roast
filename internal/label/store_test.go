package label

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const findingA = "11111111-1111-4111-8111-111111111111"
const findingB = "22222222-2222-4222-8222-222222222222"

func TestAppend_AppendOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)

	first, err := Append("run1", Label{FindingID: findingA, Correctness: CorrectnessGenuine, Adjudicator: "alice", Pass: 1})
	if err != nil {
		t.Fatalf("first Append: %v", err)
	}
	if first.RowID == "" || first.AdjudicatedAt == "" || first.RunID != "run1" {
		t.Fatalf("expected minted row id/timestamp/run id, got %+v", first)
	}

	second, err := Append("run1", Label{FindingID: findingA, Correctness: CorrectnessFalse, FalseReason: FalseReasonOther, Adjudicator: "alice", Pass: 1, Supersedes: first.RowID})
	if err != nil {
		t.Fatalf("second Append: %v", err)
	}
	if second.RowID == first.RowID {
		t.Fatal("expected a fresh row id for the second append")
	}

	rows, warnings, err := ReadAll("run1")
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows (nothing rewritten), got %d: %+v", len(rows), rows)
	}
	if rows[0].RowID != first.RowID || rows[1].RowID != second.RowID {
		t.Fatalf("expected append order preserved, got %+v", rows)
	}
}

func TestAppend_RequiresRunID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	if _, err := Append("", Label{FindingID: findingA, Correctness: CorrectnessGenuine, Adjudicator: "alice", Pass: 1}); err == nil {
		t.Fatal("expected error for empty run id")
	}
}

func TestAppend_RejectsInvalidLabel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	if _, err := Append("run1", Label{FindingID: findingA, Correctness: "bogus", Adjudicator: "alice", Pass: 1}); err == nil {
		t.Fatal("expected error for invalid correctness")
	}
	// A rejected write must not have created the file.
	rows, _, err := ReadAll("run1")
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected no rows written after a rejected append, got %d", len(rows))
	}
}

func TestAppend_SupersedesMustExist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	_, err := Append("run1", Label{FindingID: findingA, Correctness: CorrectnessGenuine, Adjudicator: "alice", Pass: 1, Supersedes: "nonexistent-row"})
	if err == nil {
		t.Fatal("expected error superseding a row that does not exist")
	}
}

func TestAppend_SupersedesMustMatchFinding(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)

	first, err := Append("run1", Label{FindingID: findingA, Correctness: CorrectnessGenuine, Adjudicator: "alice", Pass: 1})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	_, err = Append("run1", Label{FindingID: findingB, Correctness: CorrectnessFalse, FalseReason: FalseReasonOther, Adjudicator: "alice", Pass: 1, Supersedes: first.RowID})
	if err == nil {
		t.Fatal("expected error superseding a row belonging to a different finding")
	}
}

// TestAppend_SupersedesMustMatchAdjudicator guards against a correction row
// from one adjudicator scoring as though it corrected another adjudicator's
// label: Resolve trusts any pass:1 Supersedes row from primaryAdjudicator,
// so Append must refuse to create one that points at someone else's row.
func TestAppend_SupersedesMustMatchAdjudicator(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)

	bobs, err := Append("run1", Label{FindingID: findingA, Correctness: CorrectnessGenuine, Adjudicator: "bob", Pass: 1})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	_, err = Append("run1", Label{FindingID: findingA, Correctness: CorrectnessFalse, FalseReason: FalseReasonOther, Adjudicator: "alice", Pass: 1, Supersedes: bobs.RowID})
	if err == nil {
		t.Fatal("expected error: alice cannot supersede bob's row")
	}
}

// TestAppend_SupersedesMustTargetPass1 guards against a pass:1 correction
// naming a pass:2 row as its target: pass:2 rows never score, and letting
// one be "superseded" would make it indistinguishable from a real scoring
// correction.
func TestAppend_SupersedesMustTargetPass1(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)

	pass2, err := Append("run1", Label{FindingID: findingA, Correctness: CorrectnessGenuine, Adjudicator: "alice", Pass: 2})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	_, err = Append("run1", Label{FindingID: findingA, Correctness: CorrectnessFalse, FalseReason: FalseReasonOther, Adjudicator: "alice", Pass: 1, Supersedes: pass2.RowID})
	if err == nil {
		t.Fatal("expected error: a pass:1 row cannot supersede a pass:2 row")
	}
}

func TestReadAll_MissingFileIsNotError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	rows, warnings, err := ReadAll("no-such-run")
	if err != nil {
		t.Fatalf("expected no error for a run with no labels yet, got: %v", err)
	}
	if rows != nil || warnings != nil {
		t.Fatalf("expected nil, nil for a missing labels file, got rows=%v warnings=%v", rows, warnings)
	}
}

func TestReadAll_ToleratesMalformedLine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)

	if _, err := Append("run1", Label{FindingID: findingA, Correctness: CorrectnessGenuine, Adjudicator: "alice", Pass: 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Hand-corrupt the file: inject a malformed line and a blank line
	// between two well-formed ones.
	p := filepath.Join(dir, "labels", "run1.jsonl")
	existing, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	corrupted := string(existing) + "not-json-at-all\n\n"
	if err := os.WriteFile(p, []byte(corrupted), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Append("run1", Label{FindingID: findingB, Correctness: CorrectnessGenuine, Adjudicator: "alice", Pass: 1}); err != nil {
		t.Fatalf("Append after corruption: %v", err)
	}

	rows, warnings, err := ReadAll("run1")
	if err != nil {
		t.Fatalf("ReadAll must not fail on a malformed line, got: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected the 2 well-formed rows to survive, got %d: %+v", len(rows), rows)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected exactly 1 warning for the malformed line, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "malformed row") {
		t.Fatalf("expected warning to mention the malformed row, got: %s", warnings[0])
	}
}

func TestAppend_ConcurrentWritesAllSurvive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)

	const n = 20
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			_, err := Append("run1", Label{
				FindingID:   findingA,
				Correctness: CorrectnessGenuine,
				Adjudicator: "alice",
				Pass:        1,
			})
			errCh <- err
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent Append: %v", err)
		}
	}

	rows, warnings, err := ReadAll("run1")
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings from interleaved writes, got %v", warnings)
	}
	if len(rows) != n {
		t.Fatalf("expected all %d concurrent appends to land as distinct rows, got %d", n, len(rows))
	}
}

func TestPathRejectsTraversalRunID(t *testing.T) {
	// Regression for R-410823 (run 20260727T070835Z-9e96b1b0): runID reaches
	// path() through exported APIs, so an unvalidated component would let
	// Append("../runs-2026", …) write outside labels/.
	t.Setenv("ROAST_RECORD_DIR", t.TempDir())
	for _, bad := range []string{"../runs-2026", "a/b", "", ".hidden", "a..b"} {
		if _, _, err := ReadAll(bad); err == nil {
			t.Errorf("ReadAll(%q) should reject a non-run-id, got nil error", bad)
		}
	}
	if _, _, err := ReadAll("20260727T070835Z-9e96b1b0"); err != nil {
		t.Errorf("a well-formed run id must be accepted, got: %v", err)
	}
}

func TestReadAllSkipsSchemaInvalidRows(t *testing.T) {
	// Regression for R-053951: syntactic JSON is not a well-formed label; an
	// invalid correctness value must not reach Resolve as if it were a verdict.
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	runID := "20260727T070835Z-9e96b1b0"
	if err := os.MkdirAll(filepath.Join(dir, "labels"), 0o755); err != nil {
		t.Fatal(err)
	}
	rows := `{"row_id":"a","finding_id":"3f2a0155-0000-4000-8000-000000000001","correctness":"wat","pass":1,"adjudicator":"x","adjudicated_at":"2026-07-27T00:00:00Z"}
{"row_id":"b","finding_id":"3f2a0155-0000-4000-8000-000000000002","correctness":"genuine","pass":1,"adjudicator":"x","adjudicated_at":"2026-07-27T00:00:00Z"}
`
	if err := os.WriteFile(filepath.Join(dir, "labels", runID+".jsonl"), []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	got, warnings, err := ReadAll(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RowID != "b" {
		t.Errorf("expected only the valid row, got %+v", got)
	}
	if len(warnings) != 1 {
		t.Errorf("expected one warning for the invalid row, got %v", warnings)
	}
}

func TestValidateRejectsMalformedTimestamp(t *testing.T) {
	// Regression for R-881548: AdjudicatedAt orders supersession, so an
	// unparseable value must not persist.
	l := Label{
		RowID: "a", FindingID: "3f2a0155-0000-4000-8000-000000000001",
		Correctness: CorrectnessGenuine, Pass: 1, Adjudicator: "x",
		AdjudicatedAt: "yesterday",
	}
	if err := validate(l); err == nil {
		t.Error("validate should reject a non-RFC3339Nano adjudicated_at")
	}
	l.AdjudicatedAt = "2026-07-27T00:00:00.123456789Z"
	if err := validate(l); err != nil {
		t.Errorf("valid RFC3339Nano rejected: %v", err)
	}
}
