// Package sync re-applies the carried patch stack onto a new upstream ref.
//
// Each patch is cherry-picked individually (rather than a single rebase) so
// that conflicts are attributed per patch and the run reports all of them in
// one pass. rerere is consulted for every conflict before giving up.
package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/helayoty/forksync/internal/gitx"
	"github.com/helayoty/forksync/internal/patch"
)

const (
	BaseFile = "UPSTREAM_BASE"
	BotName  = "forksync-bot"
)

type Options struct {
	Remote             string // upstream remote name, e.g. "upstream"
	Ref                string // target upstream tag or sha
	Head               string // downstream branch to re-base, e.g. "main"
	Branch             string // work branch to create, e.g. "sync/v1.38.0"
	BumpOwner          string // Downstream-Owner for the generated UPSTREAM_BASE commit
	ReportPath         string
	ContinueOnConflict bool
}

type Dropped struct {
	Patch  patch.Patch
	Reason string
}

type Conflict struct {
	Patch patch.Patch
	Files []string
}

type Result struct {
	OldBase   string
	NewBase   string
	Kept      []patch.Patch
	Dropped   []Dropped
	Conflicts []Conflict
	RangeDiff string
}

var ErrConflicts = errors.New("one or more patches conflicted")

func ReadBase(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, BaseFile))
	if err != nil {
		return "", fmt.Errorf("read %s (create it with the current upstream tag): %w", BaseFile, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// ResolveRef returns a revision for tag that exists locally, trying
// <remote>/<tag> first and the bare tag second.
func ResolveRef(ctx context.Context, g gitx.Runner, remote, tag string) string {
	for _, r := range []string{remote + "/" + tag, tag} {
		if _, err := g.Git(ctx, "rev-parse", "--verify", "--quiet", r+"^{commit}"); err == nil {
			return r
		}
	}
	return tag
}

// BaseOf returns the UPSTREAM_BASE recorded in rev's tree.
func BaseOf(ctx context.Context, g gitx.Runner, rev string) (string, error) {
	out, err := g.Git(ctx, "show", rev+":"+BaseFile)
	if err != nil {
		return "", fmt.Errorf("read %s at %s: %w", BaseFile, rev, err)
	}
	return strings.TrimSpace(out), nil
}

// EnsureRef resolves tag like ResolveRef, fetching it from remote once if it
// is not available locally.
func EnsureRef(ctx context.Context, g gitx.Runner, remote, tag string) (string, error) {
	ref := ResolveRef(ctx, g, remote, tag)
	if _, err := g.Git(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
		return ref, nil
	}
	if _, err := g.Git(ctx, "fetch", "--no-tags", remote, "refs/tags/"+tag+":refs/tags/"+tag); err != nil {
		return "", fmt.Errorf("upstream base %s is not available locally and could not be fetched from %s: %w", tag, remote, err)
	}
	return tag, nil
}

// Superseded returns the heads that syncs reachable from head (and not from
// exclude) have replaced, read from their Forksync-Supersedes trailers.
func Superseded(ctx context.Context, g gitx.Runner, head string, exclude ...string) ([]string, error) {
	args := []string{"log", "--merges", "--ignore-missing", "--format=%B%x1e", head}
	if len(exclude) > 0 {
		args = append(append(args, "--not"), exclude...)
	}
	out, err := g.Git(ctx, args...)
	if err != nil {
		return nil, err
	}
	var heads []string
	for _, msg := range strings.Split(out, "\x1e") {
		for _, line := range strings.Split(msg, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), patch.TrailerSupersedes+":"); ok {
				if v = strings.TrimSpace(v); v != "" {
					heads = append(heads, v)
				}
			}
		}
	}
	return heads, nil
}

// StackOf returns the UPSTREAM_BASE recorded at head and the patch stack head
// carries on top of it, oldest first. Upstream commits, anything reachable
// from exclude, and every line a landed sync superseded are left out, so the
// result is the same before and after a sync PR lands as a merge commit.
func StackOf(ctx context.Context, g gitx.Runner, remote, head string, exclude ...string) (string, []patch.Patch, error) {
	base, err := BaseOf(ctx, g, head)
	if err != nil {
		return "", nil, err
	}
	baseRef, err := EnsureRef(ctx, g, remote, base)
	if err != nil {
		return base, nil, err
	}
	ex := append([]string{baseRef}, exclude...)
	sup, err := Superseded(ctx, g, head, ex...)
	if err != nil {
		return base, nil, err
	}
	stack, err := patch.ListExcluding(ctx, g, head, append(ex, sup...)...)
	return base, stack, err
}

// HumanCommits lists first-parent commits on branch (beyond base) that the bot
// did not commit. A non-empty result means an owner has worked on the branch
// and the bot must not regenerate it. Committers are compared, not authors,
// because the bot's cherry-picks keep each patch's original author.
func HumanCommits(ctx context.Context, g gitx.Runner, base, branch string) ([]string, error) {
	out, err := g.Git(ctx, "log", "--first-parent", "--format=%H %cn", base+".."+branch)
	if err != nil {
		return nil, err
	}
	var human []string
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		sha, committer, _ := strings.Cut(line, " ")
		if committer != BotName {
			human = append(human, sha)
		}
	}
	return human, nil
}

func Run(ctx context.Context, g gitx.Runner, o Options) (*Result, error) {
	if _, err := g.Git(ctx, "fetch", "--tags", o.Remote); err != nil {
		return nil, err
	}
	headSHA, err := g.Git(ctx, "rev-parse", "--verify", o.Head+"^{commit}")
	if err != nil {
		return nil, err
	}
	oldBase, stack, err := StackOf(ctx, g, o.Remote, headSHA)
	if err != nil {
		return nil, err
	}
	res := &Result{OldBase: oldBase, NewBase: o.Ref}
	newRef := ResolveRef(ctx, g, o.Remote, o.Ref)

	for _, kv := range [][2]string{{"rerere.enabled", "true"}, {"rerere.autoupdate", "true"}} {
		if _, err := g.Git(ctx, "config", kv[0], kv[1]); err != nil {
			return nil, err
		}
	}
	if _, err := g.Git(ctx, "rev-parse", "--verify", "--quiet", "CHERRY_PICK_HEAD"); err == nil {
		return nil, errors.New("a cherry-pick is already in progress; run 'git cherry-pick --abort' first")
	}
	if dirty, _ := g.Git(ctx, "status", "--porcelain", "--untracked-files=no"); dirty != "" {
		return nil, errors.New("worktree has uncommitted changes; sync requires a clean tree")
	}
	if _, err := g.Git(ctx, "checkout", "-B", o.Branch, newRef); err != nil {
		return nil, err
	}

	var pairs [][2]string
	for _, p := range stack {
		if p.IsBaseBump {
			continue // regenerated at the end
		}
		if reason := DropReason(ctx, g, p, newRef, o.Ref); reason != "" {
			res.Dropped = append(res.Dropped, Dropped{p, reason})
			continue
		}
		outcome, files, err := pick(ctx, g, p.SHA)
		if err != nil {
			return res, err
		}
		switch outcome {
		case pickApplied:
			res.Kept = append(res.Kept, p)
			if picked, err := g.HeadSHA(ctx); err == nil {
				pairs = append(pairs, [2]string{p.SHA, picked})
			}
		case pickEmpty:
			res.Dropped = append(res.Dropped, Dropped{p, "absorbed upstream: cherry-pick onto " + o.Ref + " is empty"})
		case pickConflict:
			res.Conflicts = append(res.Conflicts, Conflict{p, files})
			if !o.ContinueOnConflict {
				goto done
			}
		}
	}
done:
	if err := writeBaseBump(ctx, g, o); err != nil {
		return res, err
	}
	res.RangeDiff = rangeDiff(ctx, g, pairs)
	if err := supersede(ctx, g, headSHA, o.Ref); err != nil {
		return res, err
	}

	if o.ReportPath != "" {
		if err := os.WriteFile(o.ReportPath, []byte(Report(res, o)), 0o644); err != nil {
			return res, err
		}
	}
	if len(res.Conflicts) > 0 {
		return res, ErrConflicts
	}
	return res, nil
}

type pickOutcome int

const (
	pickApplied pickOutcome = iota
	pickEmpty
	pickConflict
)

// pick cherry-picks sha onto HEAD, distinguishing a clean apply, an empty
// result (change already present) and a real conflict. Conflicts are aborted;
// the caller decides what to do. rerere-resolved conflicts count as applied.
func pick(ctx context.Context, g gitx.Runner, sha string) (pickOutcome, []string, error) {
	_, err := g.Git(ctx, "cherry-pick", sha)
	if err == nil {
		return pickApplied, nil, nil
	}
	if _, perr := g.Git(ctx, "rev-parse", "--verify", "--quiet", "CHERRY_PICK_HEAD"); perr != nil {
		return 0, nil, err // never started (bad sha, dirty tree): surface git's error
	}
	unmerged, uerr := g.Unmerged(ctx)
	if uerr != nil {
		return 0, nil, uerr
	}
	if len(unmerged) > 0 {
		_, _ = g.Git(ctx, "cherry-pick", "--abort")
		return pickConflict, unmerged, nil
	}
	if _, derr := g.Git(ctx, "diff", "--cached", "--quiet"); derr == nil {
		// index == HEAD: the change is already in the tree
		if _, err := g.Git(ctx, "cherry-pick", "--skip"); err != nil {
			return 0, nil, err
		}
		return pickEmpty, nil, nil
	}
	// rerere replayed a recorded resolution and staged it
	if _, err := g.Git(ctx, "-c", "core.editor=true", "cherry-pick", "--continue"); err != nil {
		return 0, nil, err
	}
	return pickApplied, nil, nil
}

// DropReason returns a non-empty explanation when the patch is no longer
// needed on top of newRef. Signals, in order of reliability:
//
//  1. an upstream sha from "(cherry picked from commit …)" is an ancestor of newRef
//  2. a commit reachable from newRef cites that same sha in its own
//     "(cherry picked from commit …)" line — the upstream release-branch backport
//  3. a merge on newRef titled "Merge pull request #N" or a commit mentioning
//     "Automated cherry pick of #N" / "cherry-pick of #N" — PR number match
//  4. <drop> whose Drop-when is satisfied
//
// A <carry> with "Upstream: <PR>" participates in signal 3 exactly like a backport.
func DropReason(ctx context.Context, g gitx.Runner, p patch.Patch, newRef, newTag string) string {
	for _, sha := range p.UpstreamSHAs {
		if g.IsAncestor(ctx, sha, newRef) {
			return "upstream commit " + short(sha) + " is in " + newTag
		}
		if hit := grepLog(ctx, g, newRef, "", "cherry picked from commit "+sha); hit != "" {
			return "upstream commit " + short(sha) + " was backported to " + newTag + " as " + short(hit)
		}
	}
	if p.PR > 0 && (p.Kind == patch.KindBackport || p.UpstreamSt == patch.UpstreamPending) {
		if hit := grepLog(ctx, g, newRef, "--merges", fmt.Sprintf("^Merge pull request #%d ", p.PR)); hit != "" {
			return fmt.Sprintf("PR #%d merged upstream (merge %s)", p.PR, short(hit))
		}
		for _, pat := range []string{
			fmt.Sprintf("Automated cherry pick of #%d", p.PR), // k/k hack/cherry_pick_pull.sh
			fmt.Sprintf("cherry-pick of #%d\\b", p.PR),        // Prow cherrypicker plugin
		} {
			if hit := grepLog(ctx, g, newRef, "", pat); hit != "" {
				return fmt.Sprintf("PR #%d backported to %s (%s)", p.PR, newTag, short(hit))
			}
		}
	}
	if p.Kind == patch.KindDrop {
		if ok, err := patch.VersionAtLeast(newTag, p.DropWhen); err == nil && ok {
			return "Drop-when " + p.DropWhen + " satisfied by " + newTag
		}
	}
	return ""
}

func grepLog(ctx context.Context, g gitx.Runner, ref, extra, pattern string) string {
	args := []string{"log", "-1", "--format=%H", "-E", "--grep=" + pattern}
	if extra != "" {
		args = append(args, extra)
	}
	out, err := g.Git(ctx, append(args, ref)...)
	if err != nil {
		return ""
	}
	return out
}

func short(sha string) string { return sha[:min(12, len(sha))] }

func writeBaseBump(ctx context.Context, g gitx.Runner, o Options) error {
	if err := os.WriteFile(filepath.Join(g.Dir, BaseFile), []byte(o.Ref+"\n"), 0o644); err != nil {
		return err
	}
	if _, err := g.Git(ctx, "add", BaseFile); err != nil {
		return err
	}
	msg := fmt.Sprintf("UPSTREAM: <carry>: forksync: base on %s\n\n%s: %s\n%s: true\n",
		o.Ref, patch.TrailerOwner, o.BumpOwner, patch.TrailerBaseBump)
	_, err := g.Git(ctx, "commit", "--allow-empty", "-q", "-m", msg)
	return err
}

// supersede ends the sync branch with a merge of the old head that keeps the
// branch's tree. The sync PR then contains its base, so it merges without
// conflicts, and the trailer tells StackOf that the old head's history is no
// longer the stack.
func supersede(ctx context.Context, g gitx.Runner, oldHead, ref string) error {
	msg := fmt.Sprintf("forksync: supersede %s with the %s stack\n\n%s: %s\n",
		short(oldHead), ref, patch.TrailerSupersedes, oldHead)
	_, err := g.Git(ctx, "merge", "-q", "-s", "ours", "--no-ff", "-m", msg, oldHead)
	return err
}

// rangeDiff compares each carried patch with its re-applied copy.
func rangeDiff(ctx context.Context, g gitx.Runner, pairs [][2]string) string {
	var out []string
	for _, p := range pairs {
		if s, err := g.Git(ctx, "range-diff", "--no-color", p[0]+"^!", p[1]+"^!"); err == nil && s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n")
}

func Report(r *Result, o Options) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Upstream sync: `%s` → `%s`\n\n", r.OldBase, r.NewBase)
	b.WriteString("Land with **Create a merge commit**. Squash or rebase would drop the upstream history this branch brings in.\n\n")
	fmt.Fprintf(&b, "| | count |\n|---|---|\n| carried forward | %d |\n| dropped | %d |\n| **conflicts** | **%d** |\n\n",
		len(r.Kept), len(r.Dropped), len(r.Conflicts))

	if len(r.Conflicts) > 0 {
		b.WriteString("### Conflicts (patch skipped; owner must re-apply on this branch)\n\n")
		b.WriteString("Re-apply with `git cherry-pick <sha>`, resolve, commit, push. The bot will not overwrite this branch once it has your commits.\n\n")
		for _, c := range r.Conflicts {
			owner := c.Patch.Owner
			if owner == "" {
				owner = "_unowned — candidate for deletion_"
			}
			fmt.Fprintf(&b, "- `%s` %s — %s\n  - files: `%s`\n",
				short(c.Patch.SHA), c.Patch.Subject, owner, strings.Join(c.Files, "`, `"))
		}
		b.WriteString("\n")
	}
	if len(r.Dropped) > 0 {
		b.WriteString("### Dropped\n\n")
		for _, d := range r.Dropped {
			fmt.Fprintf(&b, "- `%s` %s — %s\n", short(d.Patch.SHA), d.Patch.Subject, d.Reason)
		}
		b.WriteString("\n")
	}
	b.WriteString("### Carried forward\n\n")
	for _, p := range r.Kept {
		fmt.Fprintf(&b, "- `%s` %s (%s, %s)\n", short(p.SHA), p.Subject, kindLabel(p), ownerOr(p.Owner))
	}
	if r.RangeDiff != "" {
		fmt.Fprintf(&b, "\n<details><summary>range-diff</summary>\n\n```\n%s\n```\n\n</details>\n", r.RangeDiff)
	}
	return b.String()
}

func kindLabel(p patch.Patch) string {
	if p.Kind == patch.KindCarry && p.UpstreamSt != patch.UpstreamUnset {
		return "carry/" + p.UpstreamSt.String()
	}
	return p.Kind.String()
}

func ownerOr(s string) string {
	if s == "" {
		return "unowned"
	}
	return s
}
