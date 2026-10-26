// Package gitx is a thin exec wrapper around git and gh. Rebase, cherry-pick
// and rerere have no faithful library equivalents, so shelling out is the
// honest choice for CI tooling.
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type Runner struct {
	Dir     string
	Verbose bool
}

func (r Runner) Git(ctx context.Context, args ...string) (string, error) {
	return r.run(ctx, "git", args...)
}

func (r Runner) GH(ctx context.Context, args ...string) (string, error) {
	return r.run(ctx, "gh", args...)
}

func (r Runner) run(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = r.Dir
	cmd.Env = append(os.Environ(), "GIT_EDITOR=true", "GIT_PAGER=cat")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if r.Verbose {
		fmt.Fprintf(os.Stderr, "+ %s %s\n", bin, strings.Join(args, " "))
	}
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w\n%s", bin, strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// IsAncestor reports whether a is reachable from b.
func (r Runner) IsAncestor(ctx context.Context, a, b string) bool {
	_, err := r.Git(ctx, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// Unmerged lists paths still in conflict in the index.
func (r Runner) Unmerged(ctx context.Context) ([]string, error) {
	out, err := r.Git(ctx, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

func (r Runner) HeadSHA(ctx context.Context) (string, error) {
	return r.Git(ctx, "rev-parse", "HEAD")
}
