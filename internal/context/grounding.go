package context

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// NewQuoteGrounder returns Validate's grounding check: it reports whether a
// finding's code_quote appears verbatim — modulo whitespace — in something
// the finder could actually have seen for the file it cites: the file's
// content from the selected immutable tree (or live worktree for a zero Tree),
// the file's own section of the scope diff (where a quote of
// deleted code legitimately lives), or the file's base..HEAD delta when
// rangeSpec is non-empty (branch survey, where git_diff exposes deltas that
// are in no local file and not in sc.Diff).
//
// The corpus is exactly the set of bytes the finder could have seen — no wider,
// no narrower. Wider fails open on confabulation; narrower drops genuine
// findings. Every source added to a review has to be reflected here, and every
// source narrowed has to be narrowed here in the same change.
//
// Callers must pass the PRE-Fit diff: scope.Fit truncates sc.Diff in place,
// and a quote from beyond the truncation point is still genuinely grounded.
//
// Matching collapses every whitespace run to a single space on both sides so
// formatting-only differences do not reject otherwise verbatim quotes.
// Corpora are keyed by the cited file so real diff content cannot ground a
// finding anchored to an unrelated or fabricated path.
func NewQuoteGrounder(repo string, tree Tree, diffText, rangeSpec string) func(file, quote string) bool {
	diffCorpora := splitDiffByFile(diffText)

	fileCache := make(map[string]string)
	fileCorpus := func(file string) string {
		if c, ok := fileCache[file]; ok {
			return c
		}
		// One tree per scope, matching what the reviewer was actually served.
		// For a pinned corpus that is the selected tree's blob and ONLY that
		// blob: NumberedSnippet, read_file and grep_repo all resolve against
		// the same tree (context.go), so no context source can serve later
		// worktree content and admitting it would ground a
		// quote of code the finder could not have seen. That is not
		// fail-open safety margin, it is a fail-open hole — the one
		// direction this check exists to close. Deliberately does NOT stream: this corpus needs
		// the whole normalized file in memory to substring-match, unlike
		// NumberedSnippet's bounded read.
		var raw string
		if tree.Pinned() {
			if c, ok := StagedContent(repo, tree, file); ok {
				raw = c
			}
		} else if p, ok := ResolveUnderRepo(repo, file); ok {
			if b, err := os.ReadFile(p); err == nil {
				raw = string(b)
			}
		}
		norm := normalizeWS(raw)
		fileCache[file] = norm
		return norm
	}

	rangeCache := make(map[string]string)
	rangeCorpus := func(file string) string {
		if rangeSpec == "" {
			return ""
		}
		if c, ok := rangeCache[file]; ok {
			return c
		}
		var norm string
		// Same single-file guard as gitDiffTool: the path comes from model
		// output, so reject pathspec magic before it reaches git, and `--`
		// keeps a filename from being read as a revision.
		if !strings.HasPrefix(file, ":") && !strings.ContainsAny(file, "*?[]\n") && !strings.HasSuffix(file, "/") {
			if _, ok := ResolveUnderRepo(repo, file); ok {
				out, err := exec.Command("git", "-C", repo, "diff", "--no-renames", rangeSpec, "--", file).Output()
				if err == nil {
					// Re-key through the per-file parser and take only the
					// exact cited path: a directory pathspec (`internal`,
					// `.`) expands to every changed descendant, and merging
					// that subtree into one corpus would let any descendant's
					// content ground a directory-cited finding. Sections key
					// to real file paths, so a directory key is empty.
					norm = splitDiffByFile(string(out))[file]
				}
			}
		}
		rangeCache[file] = norm
		return norm
	}

	return func(file, quote string) bool {
		nq := normalizeWS(quote)
		// A NUL in the quote could bridge the sentinels that keep sides,
		// hunks, and corpora from concatenating; no genuine quote contains
		// one (JSON can smuggle an escaped NUL), so reject outright.
		if nq == "" || strings.ContainsRune(nq, '\x00') {
			return false
		}
		if strings.Contains(fileCorpus(file), nq) {
			return true
		}
		if strings.Contains(diffCorpora[file], nq) {
			return true
		}
		return strings.Contains(rangeCorpus(file), nq)
	}
}

// splitDiffByFile parses a unified diff into whitespace-normalized per-file
// corpora, keyed by both old and new path so quotes of deleted code key to
// the finding's file. Most sections name one path on both sides (the scope
// diffs with --no-renames); a collapsed move names its old and new paths,
// and keying both lets a quote ground under either. A section whose paths cannot be parsed even after
// quote-decoding contributes nothing: unattributable content in a shared
// residual would let real diff content ground a finding cited to any path,
// and per-file isolation outranks the (git-diff-shouldn't-produce-this) risk
// of failing closed on an unparseable section.
func splitDiffByFile(diffText string) map[string]string {
	corpora := make(map[string]string)
	if diffText == "" {
		return corpora
	}
	add := func(key, body string) {
		norm := normalizedDiffSides(body)
		if norm == "" {
			return
		}
		// \x00 survives normalization (it is not whitespace) and prevents a
		// quote from matching across a section boundary.
		if prev := corpora[key]; prev != "" {
			corpora[key] = prev + " \x00 " + norm
		} else {
			corpora[key] = norm
		}
	}
	var body strings.Builder
	var paths []string
	inHunk := false
	flush := func() {
		for _, p := range paths {
			add(p, body.String())
		}
		body.Reset()
		paths = nil
	}
	for l := range strings.SplitSeq(diffText, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			flush()
			inHunk = false
		case strings.HasPrefix(l, "@@"):
			inHunk = true
		}
		// Header parsing only outside hunks: a deleted content line reading
		// `-- a/other.go` renders in a hunk as `--- a/other.go` and must not
		// key this section to an unrelated path. (A partially-parseable
		// header pair is fine to accept: outside collapsed moves old and new
		// name the same path, and a move's two paths both belong to it.)
		//
		// `--- /dev/null` (add) and `+++ /dev/null` (delete) yield no path
		// and contribute no key, as intended.
		if !inHunk {
			if p, ok := cutDiffPath(l, "--- ", "a/"); ok {
				paths = append(paths, p)
			} else if p, ok := cutDiffPath(l, "+++ ", "b/"); ok {
				if len(paths) == 0 || paths[len(paths)-1] != p {
					paths = append(paths, p)
				}
			}
		}
		body.WriteString(l)
		body.WriteByte('\n')
	}
	flush()
	return corpora
}

// cutDiffPath extracts the repo-relative path from a `--- a/…` or `+++ b/…`
// header line. core.quotePath (default true) makes git wrap a path holding
// special or non-ASCII bytes in C-style double quotes — `--- "a/f\303\266.go"`
// — which strconv.Unquote decodes (git's escapes are a subset of Go's).
func cutDiffPath(line, marker, prefix string) (string, bool) {
	rest, ok := strings.CutPrefix(line, marker)
	if !ok {
		return "", false
	}
	if strings.HasPrefix(rest, `"`) {
		u, err := strconv.Unquote(rest)
		if err != nil {
			return "", false
		}
		rest = u
	}
	return strings.CutPrefix(rest, prefix)
}

// normalizedDiffSides converts diff text into a whitespace-normalized
// grounding corpus with the old and new sides of each hunk kept separate:
// context lines belong to both sides, deletions to the old, additions to the
// new. Merging the sides into one stream would both admit hybrid quotes that
// span a -/+ interleave (content that existed in neither file version) and
// reject a legitimate old-side multiline quote whose lines have new-side
// lines interleaved between them. Sides and hunks are joined with a \x00
// sentinel (not whitespace, so it survives normalization) so a quote cannot
// match across those boundaries. Non-hunk lines (headers) contribute nothing.
func normalizedDiffSides(s string) string {
	var oldSide, newSide strings.Builder
	sentinel := func(b *strings.Builder) {
		if b.Len() > 0 {
			b.WriteString("\n\x00\n")
		}
	}
	inHunk := false
	for l := range strings.SplitSeq(s, "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			sentinel(&oldSide)
			sentinel(&newSide)
			inHunk = true
			continue
		case strings.HasPrefix(l, "diff --git "):
			inHunk = false
			continue
		}
		if !inHunk || l == "" {
			continue
		}
		switch l[0] {
		case ' ':
			oldSide.WriteString(l[1:])
			oldSide.WriteByte('\n')
			newSide.WriteString(l[1:])
			newSide.WriteByte('\n')
		case '-':
			oldSide.WriteString(l[1:])
			oldSide.WriteByte('\n')
		case '+':
			newSide.WriteString(l[1:])
			newSide.WriteByte('\n')
		}
		// Anything else inside a hunk ("\ No newline at end of file") is
		// diff metadata, not file content — skipped.
	}
	o, n := normalizeWS(oldSide.String()), normalizeWS(newSide.String())
	switch {
	case o == "":
		return n
	case n == "":
		return o
	default:
		return o + " \x00 " + n
	}
}

// normalizeWS collapses every whitespace run to a single space.
func normalizeWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
