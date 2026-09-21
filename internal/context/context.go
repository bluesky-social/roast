// Package context provides file-content access for the reviewer.
//
// All paths supplied by the model are untrusted; ResolveUnderRepo is the
// single chokepoint that keeps reads inside the repository.
//
// ONE REVIEW SEES ONE TREE. Staged reviews use an immutable index tree. Normal
// reviews use the immutable worktree tree captured for replay, including
// captured non-ignored untracked files. NumberedSnippet, read_file, list_files,
// grep_repo, and NewQuoteGrounder all receive that same Tree, so later index or
// checkout edits cannot change what a running reviewer sees. The zero Tree is
// retained for callers that intentionally request the live working tree.
package context

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bluesky-social/roast/internal/llm"
)

const (
	maxLineBytes = 1_000_000
	listFilesCap = 500
	grepMatchCap = 200
	// gitDiffByteCap is the hard server-side ceiling on one git_diff window,
	// enforced regardless of the model's offset/limit args. An unbounded
	// git_diff on one generated/huge file would re-import the multi-MB blob
	// survey mode exists to avoid, so the window is capped the same way
	// read_file is windowed.
	gitDiffByteCap   = 64 * 1024
	gitDiffLineLimit = 400
)

// ResolveUnderRepo is the single chokepoint for model-supplied paths: rejects
// empty, absolute, and anything that escapes repo after symlink resolution.
// Returns the resolved absolute path and true, or ("", false) on rejection.
func ResolveUnderRepo(repo, userPath string) (string, bool) {
	if userPath == "" {
		return "", false
	}
	if filepath.IsAbs(userPath) {
		return "", false
	}
	root, err := filepath.EvalSymlinks(repo)
	if err != nil {
		root = repo
	}
	// EvalSymlinks requires the path to exist; walk up to find deepest existing
	// ancestor so we can still confine paths to not-yet-created files.
	candidate := filepath.Join(root, filepath.Clean(userPath))
	resolved := resolveDeepest(candidate)
	if !strings.HasPrefix(resolved+string(filepath.Separator), root+string(filepath.Separator)) &&
		resolved != root {
		return "", false
	}
	return candidate, true
}

// resolveDeepest resolves symlinks on the deepest existing ancestor of path.
func resolveDeepest(path string) string {
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return resolved
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

// StagedContent returns tree's version of a file (git cat-file blob
// <OID>:<path>), or ("", false) if the path isn't in tree (e.g. staged
// deletion). tree must be pinned (tree.Staged() true) — callers that mean
// the working tree don't come through here.
//
// This is the one chokepoint every staged reader shares — NumberedSnippet,
// readFileTool, and the grounder's staged corpus — so the revision-grammar
// guard lives here rather than at each caller (the grounder in particular has
// no ResolveUnderRepo on this path). See stagedPathIsMagic.
func StagedContent(repo string, tree Tree, relpath string) (string, bool) {
	if stagedPathIsMagic(relpath) {
		return "", false
	}
	out, err := exec.Command("git", "-C", repo, "cat-file", "blob", tree.OID+":"+relpath).Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// stagedContentCeiling reads at most ceiling bytes of tree's blob for
// relpath, streaming from `git cat-file blob` rather than buffering the whole
// blob the way StagedContent does — NumberedSnippet needs at most ceiling
// bytes to decide whether to middle-truncate, so a blob far larger than the
// ceiling is never read past it. Mirrors the worktree branch's own
// io.ReadFull(f, buf) bound below, which already stops at the same ceiling
// for large on-disk files.
//
// truncated reports whether the blob held more than ceiling bytes. ok is
// false when relpath is magic-spelled or absent from tree (a staged
// deletion) — the same contract as StagedContent.
func stagedContentCeiling(repo string, tree Tree, relpath string, ceiling int) (content string, truncated, ok bool) {
	if stagedPathIsMagic(relpath) {
		return "", false, false
	}
	cmd := exec.Command("git", "-C", repo, "cat-file", "blob", tree.OID+":"+relpath)
	stdout, perr := cmd.StdoutPipe()
	if perr != nil {
		return "", false, false
	}
	if err := cmd.Start(); err != nil {
		return "", false, false
	}
	buf := make([]byte, ceiling)
	n, _ := io.ReadFull(stdout, buf)
	// One more byte beyond the ceiling, read before deciding whether to kill,
	// distinguishes "blob is exactly ceiling bytes" from "blob is bigger".
	var extra [1]byte
	m, _ := stdout.Read(extra[:])
	truncated = m > 0
	if truncated {
		// More content remains in the pipe than this caller will ever use;
		// killing rather than draining to EOF is the whole point — a blob far
		// larger than the ceiling must not cost a full read.
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if waitErr != nil && !truncated {
		// cat-file failed outright (relpath not in tree) — nothing it wrote
		// to stdout, if anything, is trustworthy.
		return "", false, false
	}
	return string(buf[:n]), truncated, true
}

// StagedPathCanonical reports whether relpath is safe to key staged content
// by — the positive-sense export of stagedPathIsMagic's guard, for callers
// outside this package that need to
// tell "this path is genuinely not in the index" apart from "StagedContent
// rejected this spelling before git was ever asked." A false here means a
// StagedContent("", false) for relpath proves NOTHING about whether the file,
// under its real canonical name, is in the reviewed commit — it must never be
// treated as absence evidence. See stagedPathIsMagic for what disqualifies a
// path.
func StagedPathCanonical(relpath string) bool {
	return !stagedPathIsMagic(relpath)
}

// stagedPathIsMagic reports whether relpath would land in git's REVISION
// grammar instead of naming the pinned-tree entry relpath — today interpolated
// as `<OID>:`+relpath, but the guard predates the tree-ish prefix and still
// applies to it unchanged. relpath is model-supplied at every StagedContent
// call site, and git reads:
//
//   - `:<n>:<path>` (n 0-3) as merge stage n — `<OID>:0:f.go` is f.go's
//     ordinary blob returned under the fabricated key "0:f.go";
//   - `:/<text>` as a commit-message search, which returns a whole COMMIT
//     (log + diff), content belonging to no file at all;
//   - a path starting `./` or `../` as cwd-relative, rewritten worktree-root-
//     relative — so `<OID>:./f.go` is a second spelling of `<OID>:f.go`.
//
// Under a tree-ish OID the stage-selector and commit-search escapes no longer
// reach outside the pinned tree the way they could reach outside a bare `:`
// revision, so the justification narrows: these spellings now return the
// RIGHT content under the WRONG key, rather than foreign content. That is
// still disqualifying — the corpora are keyed by the cited file exactly so
// real content cannot ground a finding anchored to a fabricated path, and an
// alias splits the key from the canonical one git's own diff output uses. So
// the requirement is stronger than "not magic" — the path must already be the
// canonical index spelling. A canonical path contains no empty, "." or ".."
// segment, so rejecting all three costs nothing real and does not depend on
// which alias spellings a given git version normalizes.
//
// Fails closed, mirroring the pathspec-magic rejection gitDiffTool and
// rangeCorpus already do. A file genuinely named `0:x.go` becomes unreadable in
// staged scope; that is the correct side to err on, and it still reads normally
// outside staged scope.
func stagedPathIsMagic(relpath string) bool {
	if relpath == "" || strings.HasPrefix(relpath, ":") {
		return true
	}
	// Stage selector: `<n>:` where n is a merge stage.
	if len(relpath) > 1 && relpath[1] == ':' && relpath[0] >= '0' && relpath[0] <= '3' {
		return true
	}
	// Canonical-spelling requirement. An empty leading segment also covers the
	// absolute-path form that git reads as `:/<text>` commit search.
	for seg := range strings.SplitSeq(relpath, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// NumberedSnippet renders a file with real 1-indexed line numbers, middle-
// truncated to maxBytes (head ~2/3 + tail ~1/3) so both ends of a long file
// stay visible and cited line numbers remain accurate.
//
// A pinned tree reads the captured blob so concurrent checkout edits cannot
// change what the reviewer sees.
func NumberedSnippet(repo, relpath string, maxBytes int, tree Tree) string {
	if tree.Pinned() {
		if !StagedPathCanonical(relpath) {
			return fmt.Sprintf("### %s\n(path is outside the repository or invalid)\n", relpath)
		}
	} else if _, ok := ResolveUnderRepo(repo, relpath); !ok {
		return fmt.Sprintf("### %s\n(path is outside the repository or invalid)\n", relpath)
	}

	var content string
	var header string

	if tree.Pinned() {
		readCeiling := 8 * maxBytes
		c, truncated, ok := stagedContentCeiling(repo, tree, relpath, readCeiling)
		if !ok {
			if tree.Staged() {
				return fmt.Sprintf("### %s\n(not in the index — staged deletion?)\n", relpath)
			}
			return fmt.Sprintf("### %s\n(not in the captured worktree)\n", relpath)
		}
		if truncated {
			corpus := "captured worktree blob"
			if tree.Staged() {
				corpus = "staged blob"
			}
			header = fmt.Sprintf(
				"### %s (%s exceeds %d bytes; only the first %d were read — true tail NOT shown below)",
				relpath, corpus, readCeiling, readCeiling,
			)
		}
		content = c
	} else {
		p := filepath.Join(repo, relpath)
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Sprintf("### %s\n(deleted or not a regular file)\n", relpath)
		}
		readCeiling := 8 * maxBytes
		if int(info.Size()) > readCeiling {
			f, err := os.Open(p)
			if err != nil {
				return fmt.Sprintf("### %s\n(read error: %v)\n", relpath, err)
			}
			defer func() { _ = f.Close() }()
			buf := make([]byte, readCeiling)
			n, _ := io.ReadFull(f, buf)
			content = string(buf[:n])
			header = fmt.Sprintf(
				"### %s (file is %d bytes; only the first %d were read — true tail NOT shown below)",
				relpath, info.Size(), readCeiling,
			)
		} else {
			data, err := os.ReadFile(p)
			if err != nil {
				return fmt.Sprintf("### %s\n(read error: %v)\n", relpath, err)
			}
			content = string(data)
		}
	}

	lines := splitLines(content)

	if len(content) <= maxBytes {
		var sb strings.Builder
		_, _ = fmt.Fprintf(&sb, "### %s\n```\n", relpath)
		for i, line := range lines {
			_, _ = fmt.Fprintf(&sb, "%5d  %s\n", i+1, line)
		}
		sb.WriteString("```\n")
		return sb.String()
	}

	headBudget := int(float64(maxBytes) * 0.66)
	type numberedLine struct {
		n    int
		text string
	}
	var head []numberedLine
	used := 0
	for i, line := range lines {
		cost := len(line) + 1
		if used+cost > headBudget {
			break
		}
		head = append(head, numberedLine{i + 1, line})
		used += cost
	}
	var headLast int
	if len(head) > 0 {
		headLast = head[len(head)-1].n
	}

	tailBudget := maxBytes - headBudget
	var tail []numberedLine
	used = 0
	for i := len(lines) - 1; i > headLast; i-- {
		cost := len(lines[i]) + 1
		if used+cost > tailBudget {
			break
		}
		tail = append(tail, numberedLine{i + 1, lines[i]})
		used += cost
	}
	// reverse tail so lines are in ascending order
	for i, j := 0, len(tail)-1; i < j; i, j = i+1, j-1 {
		tail[i], tail[j] = tail[j], tail[i]
	}
	var tailFirst int
	if len(tail) > 0 {
		tailFirst = tail[0].n
	} else {
		tailFirst = len(lines) + 1
	}

	if header == "" {
		header = fmt.Sprintf("### %s (%d lines, %d bytes — middle truncated)", relpath, len(lines), len(content))
	}

	var out []string
	out = append(out, header, "```")
	for _, l := range head {
		out = append(out, fmt.Sprintf("%5d  %s", l.n, l.text))
	}
	if tailFirst > headLast+1 {
		out = append(out, fmt.Sprintf("       ... (%d lines omitted; real line numbers continue below)", tailFirst-headLast-1))
	}
	for _, l := range tail {
		out = append(out, fmt.Sprintf("%5d  %s", l.n, l.text))
	}
	out = append(out, "```", "")
	return strings.Join(out, "\n")
}

// BuildFileContext concatenates numbered snippets for files up to budget bytes.
// Files that would exceed the budget render as a one-line omission notice.
// Returns "(no files)" for empty input.
func BuildFileContext(repo string, files []string, budget int, tree Tree) string {
	if len(files) == 0 {
		return "(no files)"
	}
	var chunks []string
	used := 0
	for _, f := range files {
		snippet := NumberedSnippet(repo, f, 120_000, tree)
		if used+len(snippet) > budget {
			chunks = append(chunks, fmt.Sprintf("### %s\n(omitted — context budget exceeded)\n", f))
			continue
		}
		chunks = append(chunks, snippet)
		used += len(snippet)
	}
	return strings.Join(chunks, "\n")
}

// ExplorationTools returns the read_file, list_files, and grep_repo tools
// for the LLM tool loop. All path access routes through ResolveUnderRepo.
//
// tree: when pinned, serve that tree rather than the live working tree. The
// descriptions say so as well as the code
// doing it — corpus disclosure is a standing invariant of every search tool
// here, and a model told "read a file from the repository" reasonably
// assumes the working tree, which in staged scope is not what it is being
// shown.
func ExplorationTools(repo string, tree Tree) []llm.Tool {
	readCorpus := "Reads the working tree of this checkout."
	grepCorpus := "Search the working tree of this checkout (tracked plus untracked, non-ignored files) for a pattern."
	listCorpus := "Lists files in the working tree of this checkout."
	staged := tree.Staged()
	if staged {
		readCorpus = "This review covers STAGED content, so this reads the git index blob — exactly what the commit would contain — NOT the working tree: unstaged edits and untracked files are invisible here, and a path staged for deletion reads as absent from the index."
		grepCorpus = "Search the git INDEX of this checkout for a pattern — exactly the content the review covers. Unstaged edits and untracked files are NOT searched."
		listCorpus = "Lists regular files in the captured git INDEX, not the live working tree."
	} else if tree.Pinned() {
		readCorpus = "Reads the immutable captured worktree used by this run, not later checkout edits."
		grepCorpus = "Search the immutable captured worktree used by this run, including captured non-ignored untracked files."
		listCorpus = "Lists regular files in the immutable captured worktree used by this run."
	}
	return []llm.Tool{
		{
			Name:        "read_file",
			Description: "Read a file from the repository under review. " + readCorpus + " Use when a finding depends on code you haven't seen — a helper, a type definition, a config, a test. For large files request a window via offset/limit.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":   map[string]any{"type": "string", "description": "Repo-relative path."},
					"offset": map[string]any{"type": "integer", "description": "1-indexed starting line."},
					"limit":  map[string]any{"type": "integer", "description": "Max lines to return (default 500)."},
				},
				"required": []string{"path"},
			},
			Run: func(ctx context.Context, args map[string]any) (any, error) {
				return readFileTool(repo, args, tree), nil
			},
		},
		{
			Name:        "list_files",
			Description: "List files under a repo directory, optionally filtered by glob (e.g. '*.go', '**/*_test.go'). Returns relative paths. " + listCorpus,
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"directory": map[string]any{"type": "string", "description": "Repo-relative directory (default: root)."},
					"pattern":   map[string]any{"type": "string", "description": "Optional glob."},
				},
			},
			Run: func(ctx context.Context, args map[string]any) (any, error) {
				return listFilesTool(repo, args, tree), nil
			},
		},
		{
			Name:        "grep_repo",
			Description: grepCorpus + " Returns matching lines with filenames and line numbers. An empty result is not proof of absence — it cannot see ignored/generated files, other repos, interface/reflection dispatch, or build-tag-excluded code.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern":   map[string]any{"type": "string", "description": "Search pattern (literal string or Go regexp)."},
					"path_glob": map[string]any{"type": "string", "description": "Optional glob to restrict which files are searched (e.g. '*.go')."},
				},
				"required": []string{"pattern"},
			},
			Run: func(ctx context.Context, args map[string]any) (any, error) {
				return grepRepoTool(repo, args, tree), nil
			},
		},
	}
}

// GitDiffTool returns the git_diff exploration tool for survey mode. rangeSpec
// is bound at construction (e.g. "<mergeBaseOID>..<headOID>") so the model
// cannot pass an arbitrary ref — it names one file and gets that file's delta
// over the branch under review. allowed is the manifest's file set (sc.Files):
// git_diff can only reach a path that is an exact member of that set, which
// subsumes rejecting directories (a directory is never a member), custom
// --exclude entries (already filtered out of the manifest), and correctly
// admits --include-vendored files (already reflected in the manifest).
func GitDiffTool(repo, rangeSpec string, allowed []string) llm.Tool {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, f := range allowed {
		allowedSet[f] = struct{}{}
	}
	return llm.Tool{
		Name:        "git_diff",
		Description: "Show the diff of ONE changed file over the branch under review (base..HEAD). Use in survey mode to see what actually changed in a file listed in the manifest. Large diffs are windowed — page with offset/limit.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "Repo-relative path to a single changed file (no globs or pathspec magic)."},
				"offset": map[string]any{"type": "integer", "description": "1-indexed starting line of the diff window."},
				"limit":  map[string]any{"type": "integer", "description": "Max diff lines to return (default 400)."},
			},
			"required": []string{"path"},
		},
		Run: func(ctx context.Context, args map[string]any) (any, error) {
			return gitDiffTool(ctx, repo, rangeSpec, allowedSet, args), nil
		},
	}
}

// --- tool implementations ---------------------------------------------------

// gitDiffTool runs `git -C repo diff --no-renames <rangeSpec> -- <path>` for a
// single manifest file and returns a windowed slice of the output. rangeSpec
// is caller-bound; only path and the window come from the model. allowed is
// the manifest's file set — the only paths the model may request.
//
// The diff is streamed line-by-line rather than buffered whole: only lines in
// the requested [offset, offset+limit) window are retained, up to
// gitDiffByteCap bytes, and every other line is read and discarded. This
// keeps memory bounded to one window even when the underlying diff for a
// single huge generated/vendored-adjacent file runs into the megabytes.
func gitDiffTool(ctx context.Context, repo, rangeSpec string, allowed map[string]struct{}, args map[string]any) map[string]any {
	path, _ := args["path"].(string)
	if path == "" {
		return errResult("path is required")
	}
	// One file only: reject pathspec magic (a leading ':' selects git's
	// pathspec DSL), glob metacharacters, and a trailing '/' (a directory).
	// Without this the model could widen a single-file diff into a whole
	// subtree or escape the exclude set via :(exclude) magic.
	if strings.HasPrefix(path, ":") || strings.ContainsAny(path, "*?[]\n") || strings.HasSuffix(path, "/") {
		return errResult("path %q must be a single file (no globs, pathspec magic, or trailing slash)", path)
	}
	if _, ok := ResolveUnderRepo(repo, path); !ok {
		return errResult("path '%s' is outside the repository or invalid", path)
	}
	// Exact membership in the manifest's file set is authoritative and
	// subsumes the directory-recursion bypass (a directory is never a
	// member), the custom --exclude bypass (excluded files never made it
	// into the manifest), and --include-vendored (vendored files the
	// manifest chose to include are already members).
	if _, ok := allowed[path]; !ok {
		return errResult("path %q is not one of the files under review", path)
	}

	offset, limit, err := parseOffsetLimit(args)
	if err != nil {
		return errResult("offset and limit must be integers")
	}
	if limit > gitDiffLineLimit {
		limit = gitDiffLineLimit
	}
	start := offset
	if start < 1 {
		start = 1
	}

	// rangeSpec is a caller-bound OID range; `--` separates it from the path
	// so a filename can never be reinterpreted as a revision.
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "diff", "--no-renames", rangeSpec, "--", path)
	stdout, perr := cmd.StdoutPipe()
	if perr != nil {
		return errResult("git diff failed: %v", perr)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return errResult("git diff failed: %v", err)
	}

	r := bufio.NewReaderSize(stdout, 64*1024)
	var sb strings.Builder
	total := 0
	last := start - 1 // matches diff-mode semantics: nothing returned yet
	truncated := false
	windowCapped := false
	for {
		// Read one logical line via a manual ReadSlice loop rather than
		// ReadString/ReadBytes: those accumulate an unbounded []byte for any
		// line that doesn't fit the reader's fixed buffer (e.g. one giant
		// minified-diff line with no newline), which is exactly the
		// unbounded-allocation this fix removes. lineBuf is capped at
		// gitDiffByteCap regardless of the underlying line's real length;
		// bytes beyond the cap are read (to advance the stream) but never
		// stored.
		var lineBuf []byte
		lineTruncated := false
		atEOF := false
		for {
			frag, ferr := r.ReadSlice('\n')
			if len(lineBuf) < gitDiffByteCap {
				room := gitDiffByteCap - len(lineBuf)
				take := len(frag)
				if take > room {
					take = room
					lineTruncated = true
				}
				lineBuf = append(lineBuf, frag[:take]...)
			} else if len(frag) > 0 {
				lineTruncated = true
			}
			if ferr == nil {
				break // frag ended in the delimiter: full line found
			}
			if ferr == bufio.ErrBufferFull {
				continue // no delimiter yet; keep reading fragments of this line
			}
			atEOF = true // io.EOF or another read error: stream is done
			break
		}
		if len(lineBuf) == 0 && atEOF {
			break // nothing left to read
		}
		total++
		if lineTruncated {
			truncated = true
		}
		text := strings.TrimSuffix(string(lineBuf), "\n")
		if !windowCapped && total >= start && total < start+limit {
			separatorBytes := 0
			if sb.Len() > 0 {
				separatorBytes = 1
			}
			room := gitDiffByteCap - sb.Len() - separatorBytes
			if room <= 0 {
				windowCapped = true
				truncated = true
			} else {
				if separatorBytes != 0 {
					sb.WriteByte('\n')
				}
				// A line longer than the remaining room is written truncated
				// rather than dropped: dropping it makes the line unreachable
				// via any offset (line-based pagination), so a one-line
				// minified diff — the exact case this cap exists for — would
				// return empty content forever.
				if len(text) > room {
					sb.WriteString(text[:room])
					windowCapped = true
					truncated = true
				} else {
					sb.WriteString(text)
				}
				last = total
			}
		}
		if atEOF {
			break
		}
	}
	if waitErr := cmd.Wait(); waitErr != nil {
		if ctx.Err() != nil {
			return errResult("git diff failed: %v", ctx.Err())
		}
		return errResult("git diff failed: %s", strings.TrimSpace(stderr.String()))
	}

	if total == 0 {
		return map[string]any{
			"content":        "(no diff for this file over the reviewed range)",
			"returned_range": "0-0",
			"more_before":    false,
			"more_below":     false,
			"total_lines":    0,
			"truncated":      false,
		}
	}
	if start > total {
		start = total + 1
		last = start - 1
	}

	moreBefore := start > 1
	moreBelow := last < total
	return map[string]any{
		"content":        sb.String(),
		"returned_range": fmt.Sprintf("%d-%d", start, last),
		"more_before":    moreBefore,
		"more_below":     moreBelow,
		"total_lines":    total,
		"truncated":      truncated,
	}
}

// readFileTool serves one windowed slice of a file. tree selects an immutable
// pinned blob or the live working tree. Both paths feed the SAME line loop below — the windowing,
// more-below, past-EOF and pathological-long-line handling are contract, not
// per-corpus behavior, and duplicating them per tree is how the two drift
// apart again.
//
// The pinned path streams `git cat-file blob` directly into that loop rather
// than buffering the blob first (StagedContent's contract, used by
// NumberedSnippet and the grounder, is deliberately different — see their own
// docs): a huge staged blob windowed to its first 500 lines must not cost a
// full read, so once the loop's own window fills (moreBelow), the underlying
// git process is killed rather than drained to EOF.
func readFileTool(repo string, args map[string]any, tree Tree) map[string]any {
	path, _ := args["path"].(string)
	p := ""
	if tree.Pinned() {
		if !StagedPathCanonical(path) {
			return errResult("path '%s' is outside the repository or invalid", path)
		}
	} else {
		var ok bool
		p, ok = ResolveUnderRepo(repo, path)
		if !ok {
			return errResult("path '%s' is outside the repository or invalid", path)
		}
	}

	pinned := tree.Pinned()
	notInIndex := func() map[string]any {
		// NOT "does not exist": a path staged for deletion is still on disk
		// until the commit lands, and telling the reviewer the file is
		// missing invites a finding about the wrong thing.
		return errResult("%s is not in the index — staged deletion? "+
			"this review covers the index, not the working tree", path)
	}
	notInCapturedWorktree := func() map[string]any {
		return errResult("%s is not in the captured worktree for this run", path)
	}

	// Existence is resolved against the corpus being served, before parsing the
	// window, so the error a caller gets names the real problem in either tree.
	if pinned {
		if stagedPathIsMagic(path) {
			if tree.Staged() {
				return notInIndex()
			}
			return notInCapturedWorktree()
		}
		if err := exec.Command("git", "-C", repo, "cat-file", "-e", tree.OID+":"+path).Run(); err != nil {
			if tree.Staged() {
				return notInIndex()
			}
			return notInCapturedWorktree()
		}
	} else if info, err := os.Stat(p); err != nil || !info.Mode().IsRegular() {
		return errResult("%s does not exist or is not a regular file", path)
	}

	offset, limit, err := parseOffsetLimit(args)
	if err != nil {
		return errResult("offset and limit must be integers")
	}
	end := offset + limit // first line NOT in the window

	var src io.Reader
	var stagedCmd *exec.Cmd
	if pinned {
		cmd := exec.Command("git", "-C", repo, "cat-file", "blob", tree.OID+":"+path)
		stdout, perr := cmd.StdoutPipe()
		if perr != nil {
			return errResult("read failed: %v", perr)
		}
		if serr := cmd.Start(); serr != nil {
			return errResult("read failed: %v", serr)
		}
		stagedCmd = cmd
		src = stdout
	} else {
		f, ferr := os.Open(p)
		if ferr != nil {
			return errResult("read failed: %v", ferr)
		}
		defer func() { _ = f.Close() }()
		src = f
	}

	var sliced []string
	lineNo := 0
	moreBelow := false
	var truncatedLines []int
	r := bufio.NewReaderSize(src, 64*1024)

	for {
		line, err := readLineCapped(r, maxLineBytes)
		if len(line) == 0 && err != nil {
			break
		}
		lineNo++
		// Detect pathological lines — readLineCapped stops AT the cap rather
		// than at the delimiter, so a line that hits maxLineBytes without a
		// newline needs draining.
		if len(line) >= maxLineBytes && !strings.HasSuffix(line, "\n") {
			truncatedLines = append(truncatedLines, lineNo)
			// Drain to the newline WITHOUT accumulating: ReadSlice returns the
			// internal buffer, so a 40 MB single-line blob costs one buffer,
			// not 40 MB.
			for {
				_, e := r.ReadSlice('\n')
				if e == nil || !errors.Is(e, bufio.ErrBufferFull) {
					break
				}
			}
			line = strings.TrimRight(line, "\n") + fmt.Sprintf("  ... (line truncated at %d bytes)", maxLineBytes)
		} else {
			line = strings.TrimRight(line, "\n")
		}
		if offset <= lineNo && lineNo < end {
			sliced = append(sliced, line)
		} else if lineNo >= end {
			moreBelow = true
			break
		}
		if err != nil {
			break
		}
	}

	if stagedCmd != nil {
		if moreBelow {
			// More content remains in the pipe than this caller will ever use;
			// killing rather than draining to EOF is the whole point of
			// streaming — a blob far larger than the window must not cost a
			// full read.
			_ = stagedCmd.Process.Kill()
		}
		_ = stagedCmd.Wait()
	}

	var bodyLines []string
	for i, l := range sliced {
		bodyLines = append(bodyLines, fmt.Sprintf("%5d  %s", offset+i, l))
	}
	body := strings.Join(bodyLines, "\n")

	switch {
	case moreBelow:
		body += fmt.Sprintf("\n... (more lines below; request offset>=%d to continue)", end)
	case lineNo == 0:
		body += "\n(file is empty)"
	case offset > lineNo:
		body += fmt.Sprintf("\n(offset %d is past EOF — file has %d lines)", offset, lineNo)
	}
	if len(truncatedLines) > 0 {
		body += fmt.Sprintf("\n(lines truncated at %d bytes: %v)", maxLineBytes, truncatedLines)
	}

	retEnd := end - 1
	if retEnd > lineNo {
		retEnd = lineNo
	}
	return map[string]any{
		"path":           path,
		"returned_range": []int{offset, retEnd},
		"more_below":     moreBelow,
		"content":        body,
	}
}

// readLineCapped reads one line, stopping at cap bytes rather than at the
// delimiter. bufio.Reader.ReadString accumulates until it finds the newline, so
// on a minified or generated single-line blob it allocates the WHOLE blob before
// any byte cap is consulted — which defeats the streaming this path exists to
// do, and can OOM a review on a file it was supposed to serve 200 lines of.
// ReadSlice returns the internal buffer instead, so the cost is bounded by the
// reader's buffer size no matter how long the line is.
//
// The returned line keeps its trailing newline when it had one, matching
// ReadString, so callers can still distinguish "line ended" from "hit the cap".
// err is the first read error encountered; a line ending exactly at the cap
// returns a nil error and the caller detects the missing newline.
func readLineCapped(r *bufio.Reader, cap int) (string, error) {
	var sb strings.Builder
	for {
		chunk, err := r.ReadSlice('\n')
		if len(chunk) > 0 {
			room := cap - sb.Len()
			if room > 0 {
				if len(chunk) <= room {
					sb.Write(chunk)
				} else {
					sb.Write(chunk[:room])
				}
			}
		}
		switch {
		case err == nil:
			// Found the delimiter. If the cap swallowed it, report the line as
			// capped (no trailing newline) so the caller drains the remainder.
			return sb.String(), nil
		case errors.Is(err, bufio.ErrBufferFull):
			if sb.Len() >= cap {
				// At the cap with no newline seen: the caller drains.
				return sb.String(), nil
			}
			continue
		default:
			return sb.String(), err
		}
	}
}

func listFilesTool(repo string, args map[string]any, tree Tree) map[string]any {
	directory, _ := args["directory"].(string)
	if directory == "" {
		directory = "."
	}
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		pattern = "*"
	}
	if len(pattern) > 256 {
		return errResult("pattern exceeds 256 bytes")
	}

	if tree.Pinned() {
		if !pinnedDirectoryCanonical(directory) {
			return errResult("directory '%s' is outside the repository or invalid", directory)
		}
		return listPinnedFilesTool(repo, directory, pattern, tree)
	}
	base, ok := ResolveUnderRepo(repo, directory)
	if !ok {
		return errResult("directory '%s' is outside the repository or invalid", directory)
	}
	info, err := os.Stat(base)
	if err != nil || !info.IsDir() {
		return errResult("%s does not exist or is not a directory", directory)
	}

	root := repo
	if r, err := filepath.EvalSymlinks(repo); err == nil {
		root = r
	}

	var matches []string
	err = filepath.Walk(base, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		// Skip hidden path segments (.git, .venv, etc.)
		for _, seg := range strings.Split(rel, "/") {
			if strings.HasPrefix(seg, ".") {
				return nil
			}
		}
		name := fi.Name()
		relMatch := globstarMatch(pattern, rel)
		nameMatch := !strings.Contains(pattern, "/") && globstarMatch(pattern, name)
		if relMatch || nameMatch {
			matches = append(matches, rel)
		}
		if len(matches) >= listFilesCap {
			matches = append(matches, fmt.Sprintf("... (truncated at %d matches)", listFilesCap))
			return io.EOF // stop walk
		}
		return nil
	})
	if err != nil && err != io.EOF {
		return errResult("walk error: %v", err)
	}

	return map[string]any{
		"directory": directory,
		"pattern":   pattern,
		"matches":   matches,
	}
}

func pinnedDirectoryCanonical(directory string) bool {
	if directory == "." {
		return true
	}
	return StagedPathCanonical(directory) && !strings.HasSuffix(directory, "/")
}

func listPinnedFilesTool(repo, directory, pattern string, tree Tree) map[string]any {
	out, err := exec.Command("git", "-C", repo, "ls-tree", "-r", "-z", "--full-tree", tree.OID).Output()
	if err != nil {
		return errResult("list captured tree failed: %v", err)
	}
	dir := filepath.ToSlash(filepath.Clean(directory))
	if dir == "." {
		dir = ""
	}
	prefix := dir
	if prefix != "" {
		prefix += "/"
	}
	var matches []string
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		metadata, relBytes, ok := bytes.Cut(raw, []byte{'\t'})
		if !ok {
			continue
		}
		fields := bytes.Fields(metadata)
		if len(fields) < 2 || (string(fields[0]) != "100644" && string(fields[0]) != "100755") || string(fields[1]) != "blob" {
			continue
		}
		rel := string(relBytes)
		if prefix != "" && !strings.HasPrefix(rel, prefix) {
			continue
		}
		hidden := false
		for _, seg := range strings.Split(rel, "/") {
			if strings.HasPrefix(seg, ".") {
				hidden = true
				break
			}
		}
		if hidden {
			continue
		}
		name := path.Base(rel)
		relMatch := globstarMatch(pattern, rel)
		nameMatch := !strings.Contains(pattern, "/") && globstarMatch(pattern, name)
		if relMatch || nameMatch {
			matches = append(matches, rel)
		}
		if len(matches) >= listFilesCap {
			matches = append(matches, fmt.Sprintf("... (truncated at %d matches)", listFilesCap))
			break
		}
	}
	return map[string]any{"directory": directory, "pattern": pattern, "matches": matches}
}

// globstarMatch matches slash-separated paths with standard path.Match
// syntax plus a whole-segment ** wildcard that spans zero or more segments.
func globstarMatch(pattern, value string) bool {
	patterns := strings.Split(filepath.ToSlash(pattern), "/")
	values := strings.Split(filepath.ToSlash(value), "/")
	dp := make([][]bool, len(patterns)+1)
	for i := range dp {
		dp[i] = make([]bool, len(values)+1)
	}
	dp[0][0] = true
	for i, part := range patterns {
		for j := 0; j <= len(values); j++ {
			if part == "**" {
				dp[i+1][j] = dp[i][j] || (j > 0 && dp[i+1][j-1])
				continue
			}
			if j == 0 || !dp[i][j-1] {
				continue
			}
			matched, err := path.Match(part, values[j-1])
			dp[i+1][j] = err == nil && matched
		}
	}
	return dp[len(patterns)][len(values)]
}

// grepRepoTool searches the tree under review. tree selects which one, the
// same way readFileTool does — see the package doc for why they must not
// disagree.
func grepRepoTool(repo string, args map[string]any, tree Tree) map[string]any {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return errResult("pattern is required")
	}
	// Validate as regexp so callers get a clear error on bad syntax.
	if _, err := regexp.Compile(pattern); err != nil {
		return errResult("invalid pattern: %v", err)
	}

	pathGlob, _ := args["path_glob"].(string)
	pinned := tree.Pinned()

	// A pinned tree-ish OID and --untracked are alternatives, not companions. The
	// index tree contains every staged file; the captured worktree tree also
	// contains captured non-ignored untracked files.
	//
	// For intentional live-worktree callers --untracked is load-bearing, not a convenience:
	// roast's own scope puts untracked files under review (scope.go, `git
	// ls-files --others --exclude-standard`), so a plain `git grep` — tracked
	// files only — is blind to exactly the brand-new package an
	// --uncommitted-only review is about, while read_file and list_files can see
	// it. A reviewer that greps for a caller inside such a package could get an
	// empty result and incorrectly infer that the function is never called.
	//
	// --untracked honors .gitignore on its own (only --no-exclude-standard
	// turns that off), so this widens the corpus to the reviewed tree without
	// dragging in ignored build output. Pinned by
	// TestGrepRepoTool_StillHonorsGitignore.
	gitArgs := []string{"-C", repo, "grep", "-n", "--no-color"}
	if !pinned {
		gitArgs = append(gitArgs, "--untracked")
	}
	gitArgs = append(gitArgs, "-E", "-e", pattern)
	if pinned {
		// git grep [options] pattern [<tree>…] [--] [<pathspec>…] — the pinned
		// tree-ish goes after the pattern, before the pathspec separator.
		gitArgs = append(gitArgs, tree.OID)
	}
	gitArgs = append(gitArgs, "--")
	if pathGlob != "" {
		gitArgs = append(gitArgs, pathGlob)
	}

	cmd := exec.Command("git", gitArgs...)
	out, err := cmd.Output()
	if err != nil {
		// git grep exits 1 when there are no matches — that's not an error.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return noGrepMatches(pattern, pathGlob, tree)
		}
		return errResult("grep failed: %v", err)
	}

	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	var matches []string
	for _, l := range lines {
		if l == "" {
			continue
		}
		// `git grep <tree>` prefixes every match with the tree-ish it searched
		// ("<OID>:a.go:2:func Foo(){}"), which `git grep --cached` does not.
		// These lines go straight to the model, which reads them as
		// file:line:text and cites them — so an un-stripped prefix teaches it a
		// path that does not exist, and a follow-up read_file on
		// "<OID>:a.go" would be rejected by stagedPathIsMagic's stage-selector
		// guard. Strip it so the staged and worktree corpora present matches in
		// ONE shape, which is the same reason readFileTool funnels both trees
		// through a single line loop.
		l = strings.TrimPrefix(l, tree.OID+":")
		matches = append(matches, l)
		if len(matches) >= grepMatchCap {
			matches = append(matches, fmt.Sprintf("... (truncated at %d matching lines)", grepMatchCap))
			break
		}
	}
	if len(matches) == 0 {
		// Exit 0 with nothing usable: same epistemic situation as exit 1, so it
		// gets the same caveat rather than a bare empty list.
		return noGrepMatches(pattern, pathGlob, tree)
	}
	return map[string]any{"pattern": pattern, "matches": matches}
}

// noGrepMatches builds the zero-match grep result. The empty-result path is
// where a reviewer invents a negative existence claim — "no callers", "never
// invoked", "unreachable" — and then cites the empty search as grounding, so
// the result itself carries what the search could and could not have seen. The
// caveat lives on the tool output, not only in a system prompt, because the
// model reasons over this JSON many turns after the prompt.
//
// Deliberately only on the zero-match path: a result with hits has no absence
// to disclaim, and paying these bytes on every successful grep is pure cost.
//
// With two possible corpora, WHICH TREE was searched is the thing a reader most
// needs from an empty result: "not found" means different things in the index
// and in the working tree, and a reviewer that guesses wrong draws exactly the
// false absence inference this caveat exists to stop.
func noGrepMatches(pattern, pathGlob string, tree Tree) map[string]any {
	searched := "tracked + untracked (non-ignored) files in the working tree of this checkout"
	blindTo := ""
	if tree.Staged() {
		searched = "the git INDEX of this checkout — the staged content this review covers, NOT the working tree"
		blindTo = "unstaged working-tree edits, untracked files, "
	} else if tree.Pinned() {
		searched = "the immutable captured worktree used by this run"
	}
	if pathGlob != "" {
		searched += fmt.Sprintf(", restricted to path_glob %q", pathGlob)
	}
	return map[string]any{
		"pattern":  pattern,
		"matches":  []string{},
		"searched": searched,
		"note": "No matches. This is NOT proof the symbol is absent or unused: this search " +
			"cannot see " + blindTo + ".gitignore'd or generated files, other repositories, calls made " +
			"through an interface, embedding, reflection, or a struct/func value, code behind " +
			"build tags, or a name spelled differently at the call site. Do not cite an empty " +
			"grep as grounding for 'no callers' / 'never invoked' / 'unreachable' / 'dead code'. " +
			"If a verdict or a severity downgrade depends on such a claim, establish it " +
			"positively or report insufficient_context.",
	}
}

// --- helpers ----------------------------------------------------------------

func errResult(format string, args ...any) map[string]any {
	return map[string]any{"error": fmt.Sprintf(format, args...)}
}

// splitLines is equivalent to Python's str.splitlines(): splits on \n and
// does NOT produce a trailing empty element for a trailing newline.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func parseOffsetLimit(args map[string]any) (offset, limit int, err error) {
	offset = 1
	limit = 500
	if v := args["offset"]; v != nil {
		switch vv := v.(type) {
		case int:
			offset = vv
		case float64:
			offset = int(vv)
		case string:
			if _, err := fmt.Sscan(vv, &offset); err != nil {
				return 0, 0, fmt.Errorf("bad offset")
			}
		default:
			return 0, 0, fmt.Errorf("bad offset type")
		}
		if offset < 1 {
			offset = 1
		}
	}
	if v := args["limit"]; v != nil {
		switch vv := v.(type) {
		case int:
			limit = vv
		case float64:
			limit = int(vv)
		case string:
			if _, err := fmt.Sscan(vv, &limit); err != nil {
				return 0, 0, fmt.Errorf("bad limit")
			}
		default:
			return 0, 0, fmt.Errorf("bad limit type")
		}
		if limit < 1 {
			limit = 1
		}
	}
	return offset, limit, nil
}
