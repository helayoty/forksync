// Package cherrypick pulls a merged upstream PR onto the downstream branch as
// one UPSTREAM: <PR#>: commit per upstream commit, with -x provenance.
package cherrypick

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helayoty/forksync/internal/gitx"
	"github.com/helayoty/forksync/internal/patch"
)

type Options struct {
	UpstreamRepo  string // owner/name, e.g. kubernetes/kubernetes
	Remote        string // local remote name for UpstreamRepo
	PR            int
	Owner         string // Downstream-Owner trailer
	AllowApproved bool   // accept APPROVED-but-unmerged PRs (embargo pre-staging)
	ReleaseBranch string // if set, prefer the automated cherry-pick PR targeting this branch
	BackportStyle string // "k8s" (hack/cherry_pick_pull.sh titles) or "prow" (cherrypicker plugin)
}

type prView struct {
	Number         int    `json:"number"`
	State          string `json:"state"`
	ReviewDecision string `json:"reviewDecision"`
	BaseRefName    string `json:"baseRefName"`
	MergeCommit    struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	Commits []struct {
		OID string `json:"oid"`
	} `json:"commits"`
}

func Run(ctx context.Context, g gitx.Runner, o Options) ([]string, error) {
	pr, err := view(ctx, g, o.UpstreamRepo, o.PR)
	if err != nil {
		return nil, err
	}
	switch {
	case pr.State == "MERGED":
	case o.AllowApproved && pr.State == "OPEN" && pr.ReviewDecision == "APPROVED":
	default:
		return nil, fmt.Errorf("PR #%d is %s (review=%s); refusing to carry an unmerged change", o.PR, pr.State, pr.ReviewDecision)
	}

	source := pr
	if o.ReleaseBranch != "" {
		if bp, err := findBackport(ctx, g, o.UpstreamRepo, o.PR, o.ReleaseBranch, o.BackportStyle); err == nil && bp != nil {
			source = bp
		}
	}

	if _, err := g.Git(ctx, "fetch", o.Remote, "refs/pull/"+fmt.Sprint(source.Number)+"/head"); err != nil {
		return nil, err
	}

	var picked []string
	for _, sha := range commitsToPick(ctx, g, o.Remote, source) {
		if _, err := g.Git(ctx, "cherry-pick", "-x", sha); err != nil {
			unmerged, _ := g.Unmerged(ctx)
			_, _ = g.Git(ctx, "cherry-pick", "--abort")
			return picked, fmt.Errorf("conflict picking %s (files: %s): %w", sha[:12], strings.Join(unmerged, ", "), errors.Unwrap(err))
		}
		if err := relabelHead(ctx, g, o.PR, o.Owner); err != nil {
			return picked, err
		}
		head, _ := g.HeadSHA(ctx)
		picked = append(picked, head)
	}
	return picked, nil
}

// commitsToPick returns what to cherry-pick for pr. A squash-merged PR is
// taken as its merge commit: that is the change exactly as upstream has it,
// and it is reachable from later upstream tags, so a sync drops the pick by
// ancestry. Picking the PR's own commits instead conflicts whenever a later
// commit in the PR edits an earlier one's lines, and their shas never become
// reachable upstream. Unmerged and merge-commit PRs use the PR's commits.
func commitsToPick(ctx context.Context, g gitx.Runner, remote string, pr *prView) []string {
	var shas []string
	for _, c := range pr.Commits {
		shas = append(shas, c.OID)
	}
	merged := pr.MergeCommit.OID
	if pr.State != "MERGED" || merged == "" || pr.BaseRefName == "" {
		return shas
	}
	if _, err := g.Git(ctx, "fetch", "--no-tags", remote, "refs/heads/"+pr.BaseRefName); err != nil {
		return shas
	}
	out, err := g.Git(ctx, "log", "-1", "--format=%P%x00%s", merged)
	if err != nil {
		return shas
	}
	parents, subject, _ := strings.Cut(out, "\x00")
	if len(strings.Fields(parents)) == 1 && strings.HasSuffix(subject, fmt.Sprintf("(#%d)", pr.Number)) {
		return []string{merged}
	}
	return shas
}

func view(ctx context.Context, g gitx.Runner, repo string, n int) (*prView, error) {
	out, err := g.GH(ctx, "pr", "view", fmt.Sprint(n), "--repo", repo,
		"--json", "number,state,reviewDecision,baseRefName,mergeCommit,commits")
	if err != nil {
		return nil, err
	}
	var pr prView
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// findBackport locates the automated backport PR for n on the release branch.
//
//	k8s:  kubernetes/kubernetes hack/cherry_pick_pull.sh — title "Automated cherry pick of #n ..."
//	prow: Prow cherrypicker plugin (kubernetes-sigs/*) — title "[release-x.y] <subject>",
//	      body "This is an automated cherry-pick of #n", author k8s-infra-cherrypick-robot
func findBackport(ctx context.Context, g gitx.Runner, repo string, n int, branch, style string) (*prView, error) {
	var search string
	switch style {
	case "prow":
		search = fmt.Sprintf(`author:k8s-infra-cherrypick-robot "cherry-pick of #%d" in:body`, n)
	default:
		search = fmt.Sprintf("Automated cherry pick of #%d in:title", n)
	}
	out, err := g.GH(ctx, "pr", "list", "--repo", repo, "--state", "merged", "--base", branch,
		"--search", search, "--json", "number", "--limit", "1")
	if err != nil {
		return nil, err
	}
	var hits []struct{ Number int }
	if err := json.Unmarshal([]byte(out), &hits); err != nil || len(hits) == 0 {
		return nil, err
	}
	return view(ctx, g, repo, hits[0].Number)
}

func relabelHead(ctx context.Context, g gitx.Runner, pr int, owner string) error {
	msg, err := g.Git(ctx, "log", "-1", "--format=%B", "HEAD")
	if err != nil {
		return err
	}
	subject, body, _ := strings.Cut(msg, "\n")
	prefix := fmt.Sprintf("UPSTREAM: %d: ", pr)
	if !strings.HasPrefix(subject, prefix) {
		subject = prefix + subject
	}
	body = strings.TrimRight(body, "\n")
	if owner != "" && !strings.Contains(body, patch.TrailerOwner+":") {
		body += "\n\n" + patch.TrailerOwner + ": " + owner
	}
	_, err = g.Git(ctx, "commit", "--amend", "-q", "-m", subject+"\n"+body+"\n")
	return err
}
