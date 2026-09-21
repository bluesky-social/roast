package label

import (
	"os"
	"strings"
	"testing"

	"github.com/bluesky-social/roast/internal/record"
)

const (
	uuid1 = "aaaaaaaa-1111-4111-8111-111111111111"
	uuid2 = "aaaaaaaa-2222-4222-8222-222222222222" // shares an 8-char prefix with uuid1
	uuid3 = "bbbbbbbb-3333-4333-8333-333333333333"
)

func writeRaw(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func writeResult(t *testing.T, runID, json string) {
	t.Helper()
	if p := record.WriteArtifact(runID, "result.json", json); p == "" {
		t.Fatalf("WriteArtifact returned \"\" for run %s (record dir not resolved?)", runID)
	}
}

func fixtureResultJSON() string {
	return `{"findings":[` +
		`{"id":"` + uuid1 + `","alias":"R-aaaaaaaa1"},` +
		`{"id":"` + uuid2 + `","alias":"R-aaaaaaaa2"}` +
		`],"dropped_findings":[` +
		`{"id":"` + uuid3 + `","alias":"R-bbbbbbbb"}` +
		`]}`
}

func TestResolveFindingID_FullUUID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	writeResult(t, "runX", fixtureResultJSON())

	got, err := ResolveFindingID("runX", uuid1)
	if err != nil {
		t.Fatalf("ResolveFindingID: %v", err)
	}
	if got != uuid1 {
		t.Fatalf("expected %s, got %s", uuid1, got)
	}
}

func TestResolveFindingID_Alias(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	writeResult(t, "runX", fixtureResultJSON())

	got, err := ResolveFindingID("runX", "R-aaaaaaaa2")
	if err != nil {
		t.Fatalf("ResolveFindingID: %v", err)
	}
	if got != uuid2 {
		t.Fatalf("expected %s, got %s", uuid2, got)
	}
}

func TestResolveFindingID_DroppedFindingIsResolvable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	writeResult(t, "runX", fixtureResultJSON())

	got, err := ResolveFindingID("runX", uuid3)
	if err != nil {
		t.Fatalf("ResolveFindingID: %v", err)
	}
	if got != uuid3 {
		t.Fatalf("expected dropped finding %s to resolve, got %s", uuid3, got)
	}
}

func TestResolveFindingID_UnambiguousPrefix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	writeResult(t, "runX", fixtureResultJSON())

	got, err := ResolveFindingID("runX", "bbbbbbbb")
	if err != nil {
		t.Fatalf("ResolveFindingID: %v", err)
	}
	if got != uuid3 {
		t.Fatalf("expected %s, got %s", uuid3, got)
	}
}

func TestResolveFindingID_AmbiguousPrefix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	writeResult(t, "runX", fixtureResultJSON())

	_, err := ResolveFindingID("runX", "aaaaaaaa")
	if err == nil {
		t.Fatal("expected an error for a prefix matching two findings")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected an ambiguity error, got: %v", err)
	}
}

func TestResolveFindingID_NotFound(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	writeResult(t, "runX", fixtureResultJSON())

	_, err := ResolveFindingID("runX", "cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	if err == nil {
		t.Fatal("expected an error for a finding not present in the run")
	}
}

func TestResolveFindingID_NoResultJSON_AcceptsWellFormedUUID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	// Create the run dir with only meta, no result.json artifact.
	record.WriteMeta(record.Meta{RunID: "runY"})

	got, err := ResolveFindingID("runY", uuid1)
	if err != nil {
		t.Fatalf("expected a well-formed UUID to be accepted without result.json, got: %v", err)
	}
	if got != strings.ToLower(uuid1) {
		t.Fatalf("expected %s, got %s", uuid1, got)
	}
}

func TestResolveFindingID_NoResultJSON_RejectsAlias(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	record.WriteMeta(record.Meta{RunID: "runY"})

	_, err := ResolveFindingID("runY", "R-aaaaaaaa1")
	if err == nil {
		t.Fatal("expected an error: an alias cannot be resolved without result.json")
	}
}

// TestResolveFindingID_CorruptResultJSON_RejectsEvenAUUID guards against
// conflating "artifact missing" (safe to fall back on a bare UUID) with
// "artifact present but unreadable/corrupt" (must not silently accept an
// unverified UUID, or the label could be orphaned against a finding the
// run never actually reported).
func TestResolveFindingID_CorruptResultJSON_RejectsEvenAUUID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	record.WriteMeta(record.Meta{RunID: "runZ"})
	// result.json.gz exists but isn't valid gzip.
	runDir, err := record.RunDir("runZ")
	if err != nil {
		t.Fatalf("RunDir: %v", err)
	}
	if err := writeRaw(runDir+"/result.json.gz", "not gzip at all"); err != nil {
		t.Fatalf("writeRaw: %v", err)
	}

	_, err = ResolveFindingID("runZ", uuid1)
	if err == nil {
		t.Fatal("expected an error: a corrupt result.json must not be treated as a missing one")
	}
}

func TestListRunLabels_MixedAdjudicationState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	writeResult(t, "runX", fixtureResultJSON())

	if _, err := Append("runX", Label{FindingID: uuid1, Correctness: CorrectnessGenuine, Adjudicator: "alice", Pass: 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// uuid2 and uuid3 remain unadjudicated.

	states, warnings, err := ListRunLabels("runX", "alice")
	if err != nil {
		t.Fatalf("ListRunLabels: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
	if len(states) != 3 {
		t.Fatalf("expected 3 findings (2 kept + 1 dropped), got %d", len(states))
	}
	byID := map[string]FindingLabelState{}
	for _, s := range states {
		byID[s.FindingID] = s
	}
	if !byID[uuid1].Adjudicated || byID[uuid1].Label.Correctness != CorrectnessGenuine {
		t.Fatalf("expected uuid1 adjudicated genuine, got %+v", byID[uuid1])
	}
	if byID[uuid2].Adjudicated {
		t.Fatalf("expected uuid2 unadjudicated, got %+v", byID[uuid2])
	}
	if byID[uuid3].Adjudicated {
		t.Fatalf("expected dropped finding uuid3 unadjudicated, got %+v", byID[uuid3])
	}
}

func TestListRunLabels_DefaultsAdjudicatorToUSER(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	t.Setenv("USER", "carol")
	writeResult(t, "runX", fixtureResultJSON())

	if _, err := Append("runX", Label{FindingID: uuid1, Correctness: CorrectnessGenuine, Adjudicator: "carol", Pass: 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	states, _, err := ListRunLabels("runX", "")
	if err != nil {
		t.Fatalf("ListRunLabels: %v", err)
	}
	for _, s := range states {
		if s.FindingID == uuid1 && !s.Adjudicated {
			t.Fatal("expected empty primaryAdjudicator to default to $USER and pick up carol's label")
		}
	}
}

func TestUnadjudicatedAcross_MultipleRuns(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	writeResult(t, "runA", `{"findings":[{"id":"`+uuid1+`","alias":"R-a"}],"dropped_findings":[]}`)
	writeResult(t, "runB", `{"findings":[{"id":"`+uuid2+`","alias":"R-b"}],"dropped_findings":[]}`)

	if _, err := Append("runA", Label{FindingID: uuid1, Correctness: CorrectnessGenuine, Adjudicator: "alice", Pass: 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// runB's finding stays unadjudicated.

	refs, warnings, err := UnadjudicatedAcross([]string{"runA", "runB"}, "alice")
	if err != nil {
		t.Fatalf("UnadjudicatedAcross: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
	if len(refs) != 1 || refs[0].FindingID != uuid2 {
		t.Fatalf("expected exactly runB's finding to be unadjudicated, got %+v", refs)
	}
}
