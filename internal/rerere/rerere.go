// Package rerere shares git's conflict-resolution cache between CI and
// developer machines, and learns resolutions from commits humans pushed to a
// sync branch.
//
// Storage: an orphan branch (default refs/heads/forksync/rerere) whose tree is
// a verbatim copy of .git/rr-cache. No worktree is needed: push uses a
// temporary index, pull uses git archive.
package rerere

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/helayoty/forksync/internal/gitx"
	"github.com/helayoty/forksync/internal/patch"
)

const DefaultBranch = "forksync/rerere"

func cacheDir(ctx context.Context, g gitx.Runner) (string, error) {
	common, err := g.Git(ctx, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(g.Dir, common)
	}
	return filepath.Join(common, "rr-cache"), nil
}

// Pull overlays the remote cache onto the local one. Local entries win on
// collision only if they are newer, which git handles by filename hashing: a
// given conflict hash has one preimage and at most one postimage, so a plain
// overwrite is correct.
func Pull(ctx context.Context, g gitx.Runner, remote, branch string) (bool, error) {
	if _, err := g.Git(ctx, "fetch", "--no-tags", remote, "+refs/heads/"+branch+":refs/remotes/"+remote+"/"+branch); err != nil {
		if strings.Contains(err.Error(), "couldn't find remote ref") {
			return false, nil
		}
		return false, err
	}
	dir, err := cacheDir(ctx, g)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	ar := exec.CommandContext(ctx, "git", "archive", "--format=tar", remote+"/"+branch)
	ar.Dir = g.Dir
	tar := exec.CommandContext(ctx, "tar", "-x", "-C", dir)
	pipe, err := ar.StdoutPipe()
	if err != nil {
		return false, err
	}
	tar.Stdin = pipe
	if err := ar.Start(); err != nil {
		return false, err
	}
	if err := tar.Run(); err != nil {
		return false, fmt.Errorf("extract rerere cache: %w", err)
	}
	return true, ar.Wait()
}

// Push commits the local cache onto the branch and pushes it. Returns false
// when there is nothing new.
func Push(ctx context.Context, g gitx.Runner, remote, branch string) (bool, error) {
	dir, err := cacheDir(ctx, g)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(dir); err != nil {
		return false, nil
	}
	// Drop empty preimage-only entries (unresolved conflicts) so we only share resolutions.
	_, _ = g.Git(ctx, "rerere", "gc")

	idx := filepath.Join(os.TempDir(), fmt.Sprintf("forksync-rr-%d.idx", time.Now().UnixNano()))
	defer os.Remove(idx)
	env := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"--work-tree=" + dir}, args...)...)
		cmd.Dir = g.Dir
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+idx)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if _, err := env("add", "-A", "--", "."); err != nil {
		return false, err
	}
	tree, err := env("write-tree")
	if err != nil {
		return false, err
	}
	ref := "refs/heads/" + branch
	parent, _ := g.Git(ctx, "rev-parse", "--verify", "--quiet", ref)
	if parent != "" {
		if pt, _ := g.Git(ctx, "rev-parse", parent+"^{tree}"); pt == tree {
			return false, nil
		}
	}
	args := []string{"commit-tree", tree, "-m", "forksync: rerere cache " + time.Now().UTC().Format(time.RFC3339)}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	commit, err := g.Git(ctx, args...)
	if err != nil {
		return false, err
	}
	if _, err := g.Git(ctx, "update-ref", ref, commit); err != nil {
		return false, err
	}
	_, err = g.Git(ctx, "push", remote, ref+":"+ref)
	return true, err
}

// Trained describes one resolution learned from a human commit.
type Trained struct {
	Patch patch.Patch
	From  string // human commit sha
	Files []string
}

// Train learns resolutions for patches an owner re-applied by hand on branch.
//
// For every non-bot commit c on base..branch whose subject matches a patch p
// in stack, it replays p onto c's parent in a scratch worktree; if that
// conflicts, the conflicted files are replaced with c's versions and
// `git rerere` records the resolution. Nothing on branch is modified.
func Train(ctx context.Context, g gitx.Runner, stack []patch.Patch, base, branch string) ([]Trained, error) {
	bySubject := map[string]patch.Patch{}
	for _, p := range stack {
		bySubject[p.Subject] = p
	}
	out, err := g.Git(ctx, "log", "--reverse", "--format=%H%x00%an%x00%s", base+".."+branch)
	if err != nil {
		return nil, err
	}
	wt := filepath.Join(os.TempDir(), fmt.Sprintf("forksync-train-%d", time.Now().UnixNano()))
	if _, err := g.Git(ctx, "worktree", "add", "--detach", wt, branch); err != nil {
		return nil, err
	}
	defer func() {
		_, _ = g.Git(ctx, "worktree", "remove", "--force", wt)
	}()
	w := gitx.Runner{Dir: wt, Verbose: g.Verbose}
	for _, kv := range [][2]string{{"rerere.enabled", "true"}, {"rerere.autoupdate", "false"}} {
		if _, err := w.Git(ctx, "config", kv[0], kv[1]); err != nil {
			return nil, err
		}
	}

	var learned []Trained
	for _, rec := range strings.Split(out, "\n") {
		parts := strings.SplitN(rec, "\x00", 3)
		if len(parts) != 3 || parts[1] == "forksync-bot" {
			continue
		}
		c, subject := parts[0], parts[2]
		p, ok := bySubject[subject]
		if !ok {
			continue
		}
		if _, err := w.Git(ctx, "checkout", "-q", "--detach", c+"^"); err != nil {
			return learned, err
		}
		if _, err := w.Git(ctx, "cherry-pick", "--no-commit", p.SHA); err == nil {
			_, _ = w.Git(ctx, "reset", "-q", "--hard")
			continue // applied cleanly; nothing to learn
		}
		files, _ := w.Unmerged(ctx)
		if len(files) == 0 {
			_, _ = w.Git(ctx, "cherry-pick", "--abort")
			continue
		}
		for _, f := range files {
			if _, err := w.Git(ctx, "checkout", c, "--", f); err != nil {
				// deleted in the human resolution
				_, _ = w.Git(ctx, "rm", "-q", "--cached", "--", f)
				_ = os.Remove(filepath.Join(wt, f))
			}
		}
		if _, err := w.Git(ctx, "rerere"); err != nil {
			return learned, err
		}
		learned = append(learned, Trained{Patch: p, From: c, Files: files})
		_, _ = w.Git(ctx, "cherry-pick", "--abort")
		_, _ = w.Git(ctx, "reset", "-q", "--hard")
	}
	return learned, nil
}
