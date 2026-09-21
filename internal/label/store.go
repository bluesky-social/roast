package label

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bluesky-social/roast/internal/record"
)

// Dir returns labels/ under record.DefaultDir(), or "" when the record dir
// itself can't be resolved. Mirrors record.DefaultDir's best-effort
// contract for reads; Append is user-visible and surfaces the failure.
func Dir() string {
	dir := record.DefaultDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "labels")
}

// safeRunID matches a single filename-safe path segment. Enforced on every
// path build because runID reaches path() through exported APIs
// (Append/ReadAll), and an unvalidated component in a filename is a path
// traversal — Append("../runs-2026", …) would otherwise write outside
// labels/ and could clobber the run index. Deliberately a shape constraint
// on the *segment*, not on record.NewRunID's format: the store treats a run
// id as opaque, and coupling it to today's id format would break the moment
// that format moves.
var safeRunID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// path returns labels/<run-id>.jsonl for an already-resolved (full) run id.
func path(runID string) (string, error) {
	if !safeRunID.MatchString(runID) || strings.Contains(runID, "..") {
		return "", fmt.Errorf("label: %q is not a valid run id (must be a single path-safe segment)", runID)
	}
	dir := Dir()
	if dir == "" {
		return "", fmt.Errorf("cannot resolve record dir (no HOME / XDG_DATA_HOME); set ROAST_RECORD_DIR")
	}
	return filepath.Join(dir, runID+".jsonl"), nil
}

// Append appends one adjudication row to labels/<run-id>.jsonl, minting
// RowID/AdjudicatedAt when unset. Append-only: a correction is a new row
// (Supersedes set), never a rewrite of an existing one. Concurrent-append
// safe — the row is marshaled once and written with a single os.File.Write
// under O_APPEND, mirroring record.OpenFindingSpill's contract: a single
// write() that fits the OS pipe/file buffer does not interleave with a
// concurrent writer's own single write().
//
// Unlike every other record.* operation, Append is NOT best-effort: a
// human just spent attention adjudicating a finding, and swallowing a
// write failure here would silently discard that work. Errors are
// returned, not eaten.
func Append(runID string, l Label) (Label, error) {
	if runID == "" {
		return Label{}, fmt.Errorf("label: run id is required")
	}
	l.RunID = runID
	if l.RowID == "" {
		l.RowID = newRowID()
	}
	if l.AdjudicatedAt == "" {
		l.AdjudicatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if err := validate(l); err != nil {
		return Label{}, err
	}

	if l.Supersedes != "" {
		existing, _, err := ReadAll(runID)
		if err != nil {
			return Label{}, fmt.Errorf("label: validate supersedes: %w", err)
		}
		var target *Label
		for i := range existing {
			if existing[i].RowID == l.Supersedes {
				target = &existing[i]
				break
			}
		}
		if target == nil {
			return Label{}, fmt.Errorf("label: supersedes row %q not found in run %s", l.Supersedes, runID)
		}
		if target.FindingID != l.FindingID {
			return Label{}, fmt.Errorf("label: supersedes row %q belongs to finding %s, not %s", l.Supersedes, target.FindingID, l.FindingID)
		}
		// A correction can only correct a prior SCORING row: pass:1, from
		// the same adjudicator. Without this, Resolve's primaryAdjudicator
		// + pass:1 + Supersedes!="" filter would treat a pass:1 row that
		// happens to name another adjudicator's row (or a pass:2 row) as a
		// legitimate superseder, letting an adjudicator "correct" a label
		// they never authored.
		if l.Pass != 1 || target.Pass != 1 {
			return Label{}, fmt.Errorf("label: supersedes is only valid between pass:1 rows (this row is pass:%d, target is pass:%d)", l.Pass, target.Pass)
		}
		if target.Adjudicator != l.Adjudicator {
			return Label{}, fmt.Errorf("label: supersedes row %q belongs to adjudicator %s, not %s", l.Supersedes, target.Adjudicator, l.Adjudicator)
		}
	}

	p, err := path(runID)
	if err != nil {
		return Label{}, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return Label{}, fmt.Errorf("label: create labels dir: %w", err)
	}

	b, err := json.Marshal(l)
	if err != nil {
		return Label{}, fmt.Errorf("label: marshal: %w", err)
	}
	fh, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return Label{}, fmt.Errorf("label: open %s: %w", p, err)
	}
	defer func() { _ = fh.Close() }()
	if _, err := fh.Write(append(b, '\n')); err != nil {
		return Label{}, fmt.Errorf("label: write %s: %w", p, err)
	}
	return l, nil
}

// ReadAll reads every well-formed row from labels/<run-id>.jsonl. A missing
// file (no labels yet) is not an error: it returns (nil, nil, nil). A
// malformed line is skipped and reported in the returned warnings rather
// than failing the read — one corrupt append must not make an entire run's
// labels unreadable.
func ReadAll(runID string) ([]Label, []string, error) {
	p, err := path(runID)
	if err != nil {
		return nil, nil, err
	}
	fh, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("label: open %s: %w", p, err)
	}
	defer func() { _ = fh.Close() }()

	var rows []Label
	var warnings []string
	scanner := bufio.NewScanner(fh)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var l Label
		if err := json.Unmarshal(line, &l); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s:%d: skipped malformed row: %v", p, lineNo, err))
			continue
		}
		// Syntactic JSON is not a well-formed label. Rows reaching disk
		// through any path other than Append (hand edits, a partially
		// written row, a future schema) must face the same schema check
		// writes do — otherwise an invalid correctness value flows into
		// Resolve and out into precision numbers as if it were a verdict.
		if err := validate(l); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s:%d: skipped invalid row: %v", p, lineNo, err))
			continue
		}
		rows = append(rows, l)
	}
	if err := scanner.Err(); err != nil {
		warnings = append(warnings, fmt.Sprintf("%s: scan error: %v", p, err))
	}
	return rows, warnings, nil
}

// newRowID mints a stable id for one label row. Mirrors record.NewRunID's
// rationale: never collapse an rng failure into a constant that could
// collide two rows appended in the same process.
func newRowID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		n := uint64(time.Now().UnixNano()) ^ uint64(os.Getpid())
		binary.BigEndian.PutUint64(b[:], n)
	}
	return "lbl-" + hex.EncodeToString(b[:])
}
