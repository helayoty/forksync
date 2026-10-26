// forksync keeps a fork as upstream + a rebased patch stack.
//
//	forksync sync        --remote upstream --ref v1.38.0 [--head main] [--report sync.md]
//	forksync lint        --base origin/main [--head HEAD]
//	forksync cherry-pick --repo kubernetes/kubernetes --pr 12345 --owner @me [--release-branch release-1.37]
//	forksync inventory   [--remote upstream]
//	forksync latest-tag  --remote upstream [--pattern 'v1.*.*']
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/helayoty/forksync/internal/cherrypick"
	"github.com/helayoty/forksync/internal/gitx"
	"github.com/helayoty/forksync/internal/patch"
	"github.com/helayoty/forksync/internal/release"
	"github.com/helayoty/forksync/internal/rerere"
	"github.com/helayoty/forksync/internal/scaffold"
	fsync "github.com/helayoty/forksync/internal/sync"
)

// version is set at release time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx := context.Background()
	g := gitx.Runner{Dir: ".", Verbose: os.Getenv("FORKSYNC_VERBOSE") != ""}
	var err error
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
		return
	case "sync":
		err = cmdSync(ctx, g, os.Args[2:])
	case "lint":
		err = cmdLint(ctx, g, os.Args[2:])
	case "cherry-pick":
		err = cmdCherryPick(ctx, g, os.Args[2:])
	case "inventory":
		err = cmdInventory(ctx, g, os.Args[2:])
	case "latest-tag":
		err = cmdLatestTag(ctx, g, os.Args[2:])
	case "next-tag":
		err = cmdNextTag(ctx, g, os.Args[2:])
	case "init":
		err = cmdInit(ctx, g, os.Args[2:])
	case "rerere":
		err = cmdRerere(ctx, g, os.Args[2:])
	case "human-commits":
		err = cmdHumanCommits(ctx, g, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "forksync:", err)
		if errors.Is(err, fsync.ErrConflicts) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: forksync <init|sync|lint|cherry-pick|inventory|latest-tag|next-tag|version> [flags]")
	os.Exit(64)
}

func cmdSync(ctx context.Context, g gitx.Runner, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	var o fsync.Options
	fs.StringVar(&o.Remote, "remote", "upstream", "upstream remote name")
	fs.StringVar(&o.Ref, "ref", "", "target upstream tag or sha (required)")
	fs.StringVar(&o.Head, "head", "main", "downstream branch carrying the patch stack")
	fs.StringVar(&o.Branch, "branch", "", "work branch (default sync/<ref>)")
	fs.StringVar(&o.BumpOwner, "owner", "@forksync-bot", "Downstream-Owner for the UPSTREAM_BASE commit")
	fs.StringVar(&o.ReportPath, "report", "", "write markdown report here")
	fs.BoolVar(&o.ContinueOnConflict, "continue", true, "skip conflicting patches and keep going")
	fs.Parse(args)
	if o.Ref == "" {
		return errors.New("--ref is required")
	}
	if o.Branch == "" {
		o.Branch = "sync/" + o.Ref
	}
	res, err := fsync.Run(ctx, g, o)
	if res != nil {
		fmt.Fprintf(os.Stderr, "kept=%d dropped=%d conflicts=%d\n", len(res.Kept), len(res.Dropped), len(res.Conflicts))
	}
	return err
}

func cmdLint(ctx context.Context, g gitx.Runner, args []string) error {
	fs := flag.NewFlagSet("lint", flag.ExitOnError)
	base := fs.String("base", "origin/main", "merge base to lint from")
	head := fs.String("head", "HEAD", "tip to lint")
	fs.Parse(args)
	mb, err := g.Git(ctx, "merge-base", *base, *head)
	if err != nil {
		return err
	}
	ps, err := patch.List(ctx, g, mb, *head)
	if err != nil {
		return err
	}
	bad := 0
	for _, p := range ps {
		for _, e := range patch.Lint(p) {
			bad++
			// GitHub Actions annotation format.
			fmt.Printf("::error title=commit %s::%s — %s\n", p.SHA[:12], p.Subject, e)
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d commit(s) violate the patch policy", bad)
	}
	fmt.Printf("ok: %d commit(s)\n", len(ps))
	return nil
}

func cmdCherryPick(ctx context.Context, g gitx.Runner, args []string) error {
	fs := flag.NewFlagSet("cherry-pick", flag.ExitOnError)
	var o cherrypick.Options
	fs.StringVar(&o.UpstreamRepo, "repo", "", "upstream owner/name (required)")
	fs.StringVar(&o.Remote, "remote", "upstream", "local remote for --repo")
	fs.IntVar(&o.PR, "pr", 0, "upstream PR number (required)")
	fs.StringVar(&o.Owner, "owner", "", "Downstream-Owner trailer")
	fs.BoolVar(&o.AllowApproved, "allow-approved", false, "accept APPROVED but unmerged PRs")
	fs.StringVar(&o.ReleaseBranch, "release-branch", "", "prefer the backport PR targeting this upstream branch")
	fs.StringVar(&o.BackportStyle, "backport-style", "k8s", `how upstream names backport PRs: "k8s" or "prow"`)
	fs.Parse(args)
	if o.UpstreamRepo == "" || o.PR == 0 {
		return errors.New("--repo and --pr are required")
	}
	shas, err := cherrypick.Run(ctx, g, o)
	for _, s := range shas {
		fmt.Println(s)
	}
	return err
}

func cmdInventory(ctx context.Context, g gitx.Runner, args []string) error {
	fs := flag.NewFlagSet("inventory", flag.ExitOnError)
	remote := fs.String("remote", "upstream", "upstream remote name")
	head := fs.String("head", "HEAD", "tip")
	fs.Parse(args)
	base, err := fsync.ReadBase(g.Dir)
	if err != nil {
		return err
	}
	ref := *remote + "/" + base
	if _, err := g.Git(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
		ref = base
	}
	ps, err := patch.List(ctx, g, ref, *head)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	fmt.Printf("base: %s\n\n| sha | kind | upstream | owner | subject |\n|---|---|---|---|---|\n", base)
	for _, p := range ps {
		if p.IsBaseBump {
			continue
		}
		kind := p.Kind.String()
		if p.Kind == patch.KindCarry {
			kind += "/" + p.UpstreamSt.String()
		}
		counts[kind]++
		fmt.Printf("| `%s` | %s | %s | %s | %s |\n", p.SHA[:12], kind, p.Upstream, p.Owner, p.Subject)
	}
	fmt.Printf("\ncarry/permanent=%d carry/pending=%d carry/unset=%d drop=%d backport=%d unknown=%d\n",
		counts["carry/permanent"], counts["carry/pending"], counts["carry/unset"], counts["drop"], counts["backport"], counts["unknown"])
	return nil
}

// cmdLatestTag prints the highest final (non-prerelease) tag matching the
// pattern on the remote, for the scheduled workflow to compare with UPSTREAM_BASE.
func cmdLatestTag(ctx context.Context, g gitx.Runner, args []string) error {
	fs := flag.NewFlagSet("latest-tag", flag.ExitOnError)
	remote := fs.String("remote", "upstream", "upstream remote name")
	pattern := fs.String("pattern", "v*", "glob for candidate tags")
	fs.Parse(args)
	out, err := g.Git(ctx, "ls-remote", "--tags", "--refs", "--sort=-v:refname", *remote, "refs/tags/"+*pattern)
	if err != nil {
		return err
	}
	final := regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	for _, line := range strings.Split(out, "\n") {
		_, ref, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		tag := filepath.Base(ref)
		if final.MatchString(tag) {
			fmt.Println(tag)
			return nil
		}
	}
	return errors.New("no final tag matched")
}

// cmdInit scaffolds a fork: UPSTREAM_BASE, caller workflows, optional ADO
// pipeline, and the base-bump commit. Run on a branch checked out at the
// upstream tag you want to start from.
func cmdInit(ctx context.Context, g gitx.Runner, args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	var o scaffold.Options
	fs.StringVar(&o.UpstreamRepo, "upstream", "", "upstream owner/name (required)")
	fs.StringVar(&o.Base, "base", "", "upstream tag the fork starts from (required)")
	fs.StringVar(&o.Owner, "owner", "", "Downstream-Owner for the base commit (required)")
	fs.StringVar(&o.ForksyncRepo, "forksync-repo", "", "owner/name hosting forksync's reusable workflows (required)")
	fs.StringVar(&o.ForksyncSHA, "forksync-sha", "", "forksync commit the callers pin to (required; full 40-char sha)")
	fs.StringVar(&o.ForksyncVersion, "forksync-version", "", "forksync release tag for the pin comment and ADO resource ref (default: the sha)")
	fs.StringVar(&o.TagGlob, "tag-glob", "", "upstream tag glob (default v*)")
	fs.StringVar(&o.BackportStyle, "backport-style", "", "k8s|prow (default: prow for kubernetes-sigs/*, else k8s)")
	fs.StringVar(&o.ADO, "ado", "", `Azure DevOps hand-off: "trigger" (ADO triggers on tag push) or "queue" (release workflow queues ADO); empty = no ADO`)
	fs.BoolVar(&o.Commit, "commit", true, "create the CI and UPSTREAM_BASE commits")
	fs.Parse(args)
	for _, kv := range [][2]string{{"--upstream", o.UpstreamRepo}, {"--base", o.Base}, {"--owner", o.Owner}, {"--forksync-repo", o.ForksyncRepo}, {"--forksync-sha", o.ForksyncSHA}} {
		if kv[1] == "" {
			return fmt.Errorf("%s is required", kv[0])
		}
	}
	if _, err := g.Git(ctx, "rev-parse", "--verify", "--quiet", o.Base+"^{commit}"); err != nil {
		return fmt.Errorf("tag %s not found locally; git fetch --tags upstream first", o.Base)
	}
	written, err := scaffold.Run(ctx, g, o)
	for _, w := range written {
		fmt.Println("wrote", w)
	}
	if err != nil {
		return err
	}
	fmt.Println("\nnext:")
	fmt.Println("  - optional secret FORK_BOT_TOKEN (GitHub App or PAT: contents+pull-requests+issues write on this repo). Without it the bot's PRs get no checks.")
	fmt.Println("  - branch protection on main requiring 'patch-policy / policy / lint'")
	if o.ADO == scaffold.ADOQueue {
		fmt.Println("  - repo vars ADO_ORG, ADO_PROJECT, ADO_PIPELINE_ID and secret ADO_PAT")
	}
	if o.ADO != scaffold.ADONone {
		fmt.Println("  - ADO: GitHub service connection 'github' with read access to " + o.ForksyncRepo + "; create the pipeline from azure-pipelines.yml")
	}
	return nil
}

// cmdRerere: pull | push | train. Shares .git/rr-cache through an orphan
// branch and learns resolutions from human commits on a sync branch.
func cmdRerere(ctx context.Context, g gitx.Runner, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: forksync rerere <pull|push|train> [flags]")
	}
	fs := flag.NewFlagSet("rerere "+args[0], flag.ExitOnError)
	remote := fs.String("remote", "origin", "remote holding the cache branch")
	branch := fs.String("branch", rerere.DefaultBranch, "cache branch name")
	upstream := fs.String("upstream", "upstream", "upstream remote (train)")
	head := fs.String("head", "main", "downstream branch the stack lives on (train)")
	syncBranch := fs.String("sync-branch", "", "sync branch with human fixes (train, required)")
	fs.Parse(args[1:])

	switch args[0] {
	case "pull":
		ok, err := rerere.Pull(ctx, g, *remote, *branch)
		if err == nil {
			fmt.Printf("rerere pull: %s\n", map[bool]string{true: "updated", false: "no cache branch yet"}[ok])
		}
		return err
	case "push":
		ok, err := rerere.Push(ctx, g, *remote, *branch)
		if err == nil {
			fmt.Printf("rerere push: %s\n", map[bool]string{true: "pushed", false: "nothing new"}[ok])
		}
		return err
	case "train":
		if *syncBranch == "" {
			return errors.New("--sync-branch is required")
		}
		base, err := fsync.ReadBase(g.Dir)
		if err != nil {
			return err
		}
		stack, err := patch.List(ctx, g, fsync.ResolveRef(ctx, g, *upstream, base), *head)
		if err != nil {
			return err
		}
		// the sync branch's base is its UPSTREAM_BASE, not main's
		newBase, err := g.Git(ctx, "show", *syncBranch+":"+fsync.BaseFile)
		if err != nil {
			return err
		}
		learned, err := rerere.Train(ctx, g, stack, fsync.ResolveRef(ctx, g, *upstream, strings.TrimSpace(newBase)), *syncBranch)
		for _, l := range learned {
			fmt.Printf("learned %s from %s (%s)\n", l.Patch.Subject, l.From[:12], strings.Join(l.Files, ", "))
		}
		if err == nil && len(learned) == 0 {
			fmt.Println("nothing to learn")
		}
		return err
	}
	return fmt.Errorf("unknown rerere subcommand %q", args[0])
}

// cmdHumanCommits exits 0 and prints shas when non-bot commits exist on
// --branch beyond --base; exits 3 when there are none. Used by the sync lane
// to decide whether it may regenerate the branch.
func cmdHumanCommits(ctx context.Context, g gitx.Runner, args []string) error {
	fs := flag.NewFlagSet("human-commits", flag.ExitOnError)
	base := fs.String("base", "", "exclusive lower bound (required)")
	branch := fs.String("branch", "", "branch to inspect (required)")
	fs.Parse(args)
	if *base == "" || *branch == "" {
		return errors.New("--base and --branch are required")
	}
	shas, err := fsync.HumanCommits(ctx, g, *base, *branch)
	if err != nil {
		return err
	}
	if len(shas) == 0 {
		os.Exit(3)
	}
	fmt.Println(strings.Join(shas, "\n"))
	return nil
}

// cmdNextTag prints the next downstream release tag for the current
// UPSTREAM_BASE, plus an OCI-safe image tag, as KEY=VALUE lines.
func cmdNextTag(ctx context.Context, g gitx.Runner, args []string) error {
	fs := flag.NewFlagSet("next-tag", flag.ExitOnError)
	remote := fs.String("remote", "origin", "remote whose tags are authoritative")
	sep := fs.String("sep", "-", `separator before "fork.N": "-" (semver pre-release) or "+" (build metadata)`)
	fs.Parse(args)
	base, err := fsync.ReadBase(g.Dir)
	if err != nil {
		return err
	}
	tag, err := release.Next(ctx, g, release.Options{Remote: *remote, Base: base, Separator: *sep})
	if err != nil {
		return err
	}
	fmt.Printf("tag=%s\nimage_tag=%s\nupstream_base=%s\n", tag, release.ImageTag(tag), base)
	return nil
}
