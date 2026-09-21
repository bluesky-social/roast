package label

import "time"

// Resolve returns the row that scores for findingID under primaryAdjudicator:
//
//   - only rows from primaryAdjudicator are eligible — every other
//     adjudicator's rows accumulate for a future inter-rater κ but never
//     score in the primary-adjudicator result.
//   - pass:2 rows never score, unconditionally: they exist solely to
//     compute the intra-rater agreement statistic, and letting one replace
//     a pass:1 label by write order alone would destroy that measurement.
//   - among pass:1 rows, the bare row (no Supersedes) scores UNLESS at
//     least one row explicitly supersedes it, in which case the newest
//     superseding row by adjudicated_at wins — a correction is a
//     deliberate act, never an accident of write order.
//
// ok is false when no row scores for findingID: the finding is
// unadjudicated. Callers MUST check ok — the zero Label is not a valid
// "false" label, it is the absence of one; the comma-ok shape (the idiom
// this codebase already uses for "was there a value") is built to make
// that confusion impossible to skip past.
func Resolve(rows []Label, findingID, primaryAdjudicator string) (Label, bool) {
	var base *Label
	var baseAt time.Time
	var superseders []Label

	for i := range rows {
		r := rows[i]
		if r.FindingID != findingID || r.Adjudicator != primaryAdjudicator {
			continue
		}
		if r.Pass != 1 {
			continue // pass:2 never scores, no exceptions
		}
		if r.Supersedes != "" {
			superseders = append(superseders, r)
			continue
		}
		// A bare pass:1 row for the same finding should be unique in normal
		// use, but Append doesn't forbid a second one outright (append-only
		// means the store never rejects a write for this); if it happens,
		// the earliest is the canonical "first adjudication".
		at := parseTime(r.AdjudicatedAt)
		if base == nil || at.Before(baseAt) {
			row := r
			base = &row
			baseAt = at
		}
	}

	if len(superseders) > 0 {
		winner := superseders[0]
		winnerAt := parseTime(winner.AdjudicatedAt)
		for _, s := range superseders[1:] {
			at := parseTime(s.AdjudicatedAt)
			if at.After(winnerAt) {
				winner = s
				winnerAt = at
			}
		}
		return winner, true
	}
	if base != nil {
		return *base, true
	}
	return Label{}, false
}

// parseTime parses an AdjudicatedAt value, returning the zero time.Time on
// failure. Rows are internally generated in RFC3339Nano; a value that
// doesn't parse sorts as earliest-possible rather than panicking.
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
