package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/roast/internal/label"
	"github.com/bluesky-social/roast/internal/record"
	"github.com/bluesky-social/roast/internal/review"
)

// newRunsLabelCommand is the manual write path for adjudication labels:
// `roast runs label <run-id> <finding-id> --correctness ...` appends one
// row to labels/<run-id>.jsonl. Append-only — a re-adjudication is a new
// row (--supersedes), never an edit of an old one.
func newRunsLabelCommand(stdout, stderr io.Writer) *cobra.Command {
	var correctness, falseReason, severityOverride, adjudicator, supersedes string
	var pass int
	cmd := &cobra.Command{
		Use:   "label <run-id> <finding-id>",
		Short: "Record a human adjudication label for one finding (append-only)",
		Long: `Appends one adjudication row to labels/<run-id>.jsonl under the record dir.

<finding-id> accepts the finding's full UUID, an unambiguous UUID prefix, or
its run-local alias (R-xxxxxx) — resolved against the run's recorded
findings and stored as the UUID (identity != similarity: labels are never
keyed by alias or fingerprint).

A re-adjudication is a new row, never a rewrite: pass --supersedes <row-id>
(printed by this command on write) to make a correction score in place of
an earlier one. --pass 2 rows never score; they exist only to measure
intra-rater agreement.`,
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if correctness == "" {
				return &usageError{msg: "--correctness is required (genuine|false|uncertain)"}
			}
			runRef, findingRef := args[0], args[1]

			runDir, err := record.RunDir(runRef)
			if err != nil {
				return &usageError{msg: err.Error()}
			}
			fullRunID := filepath.Base(runDir)

			findingID, err := label.ResolveFindingID(runRef, findingRef)
			if err != nil {
				return &usageError{msg: err.Error()}
			}

			adj := adjudicator
			if adj == "" {
				adj = label.DefaultAdjudicator()
			}
			if adj == "" {
				return &usageError{msg: "adjudicator required: pass --adjudicator or set $USER"}
			}

			l := label.Label{
				FindingID:        findingID,
				Correctness:      label.Correctness(correctness),
				FalseReason:      label.FalseReason(falseReason),
				SeverityOverride: review.Severity(severityOverride),
				Adjudicator:      adj,
				Pass:             pass,
				Supersedes:       supersedes,
			}
			written, err := label.Append(fullRunID, l)
			if err != nil {
				return &usageError{msg: err.Error()}
			}

			logf(stderr, "labeled finding %s (run %s): correctness=%s pass=%d adjudicator=%s row=%s",
				written.FindingID, fullRunID, written.Correctness, written.Pass, written.Adjudicator, written.RowID)

			b, err := json.Marshal(written)
			if err != nil {
				return fmt.Errorf("marshal label: %w", err)
			}
			fmt.Fprintln(stdout, string(b))
			return nil
		},
	}
	cmd.Flags().StringVar(&correctness, "correctness", "", "genuine|false|uncertain (required)")
	cmd.Flags().StringVar(&falseReason, "reason", "", "false_reason when correctness=false: quote_confabulation|standard_misapplied|stale_assumption|other")
	cmd.Flags().StringVar(&severityOverride, "severity-override", "", "override the finding's severity: INFO|LOW|MEDIUM|HIGH|CRITICAL")
	cmd.Flags().IntVar(&pass, "pass", 1, "1 = first adjudication, 2 = intra-rater re-adjudication (never scores)")
	cmd.Flags().StringVar(&supersedes, "supersedes", "", "row id this label corrects (the newest superseding row wins at scoring time)")
	cmd.Flags().StringVar(&adjudicator, "adjudicator", "", "adjudicator name (default: $USER)")
	return cmd
}
