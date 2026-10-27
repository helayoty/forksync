package cherrypick

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helayoty/forksync/internal/gitx"
	fsync "github.com/helayoty/forksync/internal/sync"
)

type repo struct {
	t *testing.T
	g gitx.Runner
}

func (r repo) git(args ...string) string {
	r.t.Helper()
	out, err := r.g.Git(context.Background(), args...)
	if err != nil {
		r.t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func (r repo) commit(file, body, msg string) string {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.g.Dir, file), []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.git("add", "-A")
	r.git("commit", "-q", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

// A squash-merged upstream backport whose second commit rewrites the first
// one's lines, as Kueue's cherry-pick robot PRs often do.
func TestCommitsToPick_SquashMergedBackport(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "tester")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.com")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")

	up := repo{t, gitx.Runner{Dir: filepath.Join(root, "up")}}
	if err := os.MkdirAll(up.g.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	up.git("init", "-q", "-b", "main")
	up.commit("f.go", "a\n", "init")
	up.git("tag", "v1.0.0")
	up.git("checkout", "-qb", "release-1.0")
	up.git("checkout", "-qb", "pr")
	c1 := up.commit("f.go", "a\nfix\n// a long comment\n", "fix")
	c2 := up.commit("f.go", "a\nfix\n// short\n", "shorten the comment")
	up.git("checkout", "-q", "release-1.0")
	squash := up.commit("f.go", "a\nfix\n// short\n", "[release-1.0] fix (#12)")
	up.git("tag", "v1.0.1")
	up.git("checkout", "-q", "main")
	up.commit("g.go", "g\n", "unrelated")
	up.git("merge", "-q", "--no-ff", "-m", "Merge pull request #13 from someone/pr", "pr")
	merged := up.git("rev-parse", "HEAD")

	fork := repo{t, gitx.Runner{Dir: filepath.Join(root, "fork")}}
	if out, err := exec.Command("git", "clone", "-q", up.g.Dir, fork.g.Dir).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	fork.git("remote", "add", "upstream", up.g.Dir)
	fork.git("fetch", "-q", "--tags", "upstream")
	fork.git("checkout", "-qb", "fork-main", "v1.0.0")
	fork.commit(fsync.BaseFile, "v1.0.0\n", "UPSTREAM: <carry>: forksync: base on v1.0.0\n\nDownstream-Owner: @bot\nForksync-Base: true")

	commits := func(shas ...string) []struct {
		OID string `json:"oid"`
	} {
		var cs []struct {
			OID string `json:"oid"`
		}
		for _, s := range shas {
			cs = append(cs, struct {
				OID string `json:"oid"`
			}{s})
		}
		return cs
	}

	pr := &prView{Number: 12, State: "MERGED", BaseRefName: "release-1.0", Commits: commits(c1, c2)}
	pr.MergeCommit.OID = squash
	got := commitsToPick(ctx, fork.g, "upstream", pr)
	if len(got) != 1 || got[0] != squash {
		t.Fatalf("squash-merged PR: picks = %v, want the squash commit", got)
	}

	// A PR landed with a merge commit keeps its own commits.
	tide := &prView{Number: 13, State: "MERGED", BaseRefName: "main", Commits: commits(c1, c2)}
	tide.MergeCommit.OID = merged
	if got := commitsToPick(ctx, fork.g, "upstream", tide); len(got) != 2 {
		t.Fatalf("merge-commit PR: picks = %v, want the PR's commits", got)
	}

	// Pick it as the urgent lane does, then sync to the patch release that contains it.
	fork.git("cherry-pick", "-x", squash)
	if err := relabelHead(ctx, fork.g, 12, "@alice"); err != nil {
		t.Fatal(err)
	}
	res, err := fsync.Run(ctx, fork.g, fsync.Options{
		Remote: "upstream", Ref: "v1.0.1", Head: "fork-main", Branch: "sync/v1.0.1",
		BumpOwner: "@bot", ContinueOnConflict: true,
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.Dropped) != 1 || !strings.Contains(res.Dropped[0].Reason, "is in v1.0.1") {
		t.Fatalf("dropped = %+v, want the pick dropped by ancestry", res.Dropped)
	}
}
