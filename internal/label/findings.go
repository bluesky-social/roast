package label

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bluesky-social/roast/internal/record"
)

// FindingRef identifies one finding within one run — enough to key a label
// and to display a queue entry. E6 fetches the finding's full content
// (claim, quote, file context) itself from the run's own artifacts.
type FindingRef struct {
	RunID     string `json:"run_id"`
	FindingID string `json:"finding_id"`
	Alias     string `json:"alias,omitempty"`
}

// FindingLabelState pairs a FindingRef with its resolved scoring state.
// Label is meaningless when Adjudicated is false — mirrors Resolve's
// comma-ok contract rather than defaulting to any correctness value.
type FindingLabelState struct {
	FindingRef
	Adjudicated bool  `json:"adjudicated"`
	Label       Label `json:"label,omitempty"`
}

// DefaultAdjudicator returns $USER — the fallback the `roast runs label`
// CLI and this package's read helpers use when no adjudicator is named
// explicitly. Empty when $USER is unset; callers should treat that as
// "adjudicator required", never as labeling under "".
func DefaultAdjudicator() string {
	return os.Getenv("USER")
}

// resultFinding is the subset of a run's result.json finding object this
// package needs. The full object (reasoning, description, code_quote, ...)
// is render.ToJSON's concern, not this package's.
type resultFinding struct {
	ID    string `json:"id"`
	Alias string `json:"alias"`
}

type resultJSON struct {
	Findings        []resultFinding `json:"findings"`
	DroppedFindings []resultFinding `json:"dropped_findings"`
}

// runFindingRefs reads runDir's result.json artifact and returns every
// finding it recorded — kept AND dropped. A dropped finding (e.g. one the
// quote-grounder rejected) is exactly the kind of false proposal
// adjudication needs to see, not one it can skip.
func runFindingRefs(runDir, fullRunID string) ([]FindingRef, error) {
	raw, err := record.ReadArtifact(runDir, "result.json")
	if err != nil {
		return nil, err
	}
	var rj resultJSON
	if err := json.Unmarshal([]byte(raw), &rj); err != nil {
		return nil, fmt.Errorf("label: parse result.json for run %s: %w", fullRunID, err)
	}
	refs := make([]FindingRef, 0, len(rj.Findings)+len(rj.DroppedFindings))
	add := func(fs []resultFinding) {
		for _, f := range fs {
			if f.ID == "" {
				continue // pre-Phase-2 run recorded before findings had stable identity
			}
			refs = append(refs, FindingRef{RunID: fullRunID, FindingID: f.ID, Alias: f.Alias})
		}
	}
	add(rj.Findings)
	add(rj.DroppedFindings)
	return refs, nil
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ResolveFindingID resolves a user-supplied finding reference — a full
// UUID, an unambiguous UUID prefix (dashes optional), or the run-local
// alias ("R-8d8d19") — against the run's recorded findings (kept and
// dropped) and returns the finding's immutable UUID. This is the ONLY path
// from user/CLI input to the identity a label is keyed by: labels are
// never keyed by alias or fingerprint (identity ≠ similarity).
func ResolveFindingID(runID, ref string) (string, error) {
	runDir, err := record.RunDir(runID)
	if err != nil {
		return "", err
	}
	fullRunID := filepath.Base(runDir)

	refs, err := runFindingRefs(runDir, fullRunID)
	if err != nil {
		// Only a genuinely MISSING artifact (pre-Phase-2 run, or recording
		// disabled at record time) falls back to accepting a well-formed
		// UUID unverified. A present-but-corrupt or unreadable result.json
		// must not be treated the same way — silently accepting an
		// unverified UUID there could orphan the label against a finding
		// that was never actually reported, while claiming success.
		if errors.Is(err, fs.ErrNotExist) && uuidRE.MatchString(ref) {
			return strings.ToLower(ref), nil
		}
		return "", fmt.Errorf("resolve finding %q against run %s: %w (pass the full UUID when a run has no result.json to resolve an alias/prefix against)", ref, fullRunID, err)
	}

	for _, r := range refs {
		if strings.EqualFold(r.FindingID, ref) || (r.Alias != "" && strings.EqualFold(r.Alias, ref)) {
			return r.FindingID, nil
		}
	}

	norm := strings.ToLower(ref)
	norm = strings.TrimPrefix(norm, "r-")
	norm = strings.ReplaceAll(norm, "-", "")
	var matches []string
	for _, r := range refs {
		idHex := strings.ReplaceAll(strings.ToLower(r.FindingID), "-", "")
		if norm != "" && strings.HasPrefix(idHex, norm) {
			matches = append(matches, r.FindingID)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no finding in run %s matches %q", fullRunID, ref)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%q is ambiguous in run %s: matches %d findings", ref, fullRunID, len(matches))
	}
}

// ListRunLabels lists every finding recorded for runID alongside its
// resolved label state under primaryAdjudicator (DefaultAdjudicator() when
// empty). Malformed label rows are tolerated (see ReadAll); warnings from
// that tolerance are returned so a caller can surface them.
func ListRunLabels(runID, primaryAdjudicator string) ([]FindingLabelState, []string, error) {
	if primaryAdjudicator == "" {
		primaryAdjudicator = DefaultAdjudicator()
	}
	// An empty adjudicator would resolve against rows attributed to "" —
	// which Append rejects, so nothing ever matches and every finding
	// reports unadjudicated. That is silently wrong in the worst way: it
	// reads as "nobody has judged these" when labels exist. Fail instead.
	if primaryAdjudicator == "" {
		return nil, nil, fmt.Errorf("label: no adjudicator (pass one explicitly or set $USER); resolving against an empty adjudicator would report every finding unadjudicated")
	}
	runDir, err := record.RunDir(runID)
	if err != nil {
		return nil, nil, err
	}
	fullRunID := filepath.Base(runDir)

	refs, err := runFindingRefs(runDir, fullRunID)
	if err != nil {
		return nil, nil, err
	}
	rows, warnings, err := ReadAll(fullRunID)
	if err != nil {
		return nil, warnings, err
	}

	out := make([]FindingLabelState, 0, len(refs))
	for _, ref := range refs {
		lbl, ok := Resolve(rows, ref.FindingID, primaryAdjudicator)
		out = append(out, FindingLabelState{FindingRef: ref, Adjudicated: ok, Label: lbl})
	}
	return out, warnings, nil
}

// UnadjudicatedAcross returns every finding with no scoring label under
// primaryAdjudicator, across all of the given run ids. It is ListRunLabels
// filtered to !Adjudicated, over a set of runs instead of one — E6's
// queue-building block.
func UnadjudicatedAcross(runIDs []string, primaryAdjudicator string) ([]FindingRef, []string, error) {
	var out []FindingRef
	var allWarnings []string
	for _, id := range runIDs {
		states, warnings, err := ListRunLabels(id, primaryAdjudicator)
		if err != nil {
			return nil, allWarnings, fmt.Errorf("run %s: %w", id, err)
		}
		allWarnings = append(allWarnings, warnings...)
		for _, s := range states {
			if !s.Adjudicated {
				out = append(out, s.FindingRef)
			}
		}
	}
	return out, allWarnings, nil
}
