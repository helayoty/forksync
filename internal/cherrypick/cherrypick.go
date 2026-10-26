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
	for _, c := range source.Commits {
		if _, err := g.Git(ctx, "cherry-pick", "-x", c.OID); err != nil {
			unmerged, _ := g.Unmerged(ctx)
			_, _ = g.Git(ctx, "cherry-pick", "--abort")
			return picked, fmt.Errorf("conflict picking %s (files: %s): %w", c.OID[:12], strings.Join(unmerged, ", "), errors.Unwrap(err))
		}
		if err := relabelHead(ctx, g, o.PR, o.Owner); err != nil {
			return picked, err
		}
		sha, _ := g.HeadSHA(ctx)
		picked = append(picked, sha)
	}
	return picked, nil
}

func view(ctx context.Context, g gitx.Runner, repo string, n int) (*prView, error) {
	out, err := g.GH(ctx, "pr", "view", fmt.Sprint(n), "--repo", repo,
		"--json", "number,state,reviewDecision,mergeCommit,commits")
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
