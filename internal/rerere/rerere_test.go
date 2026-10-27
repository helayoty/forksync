package rerere

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helayoty/forksync/internal/gitx"
	"github.com/helayoty/forksync/internal/patch"
	fsync "github.com/helayoty/forksync/internal/sync"
)

func run(t *testing.T, g gitx.Runner, args ...string) string {
	t.Helper()
	out, err := g.Git(context.Background(), args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func write(t *testing.T, g gitx.Runner, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(g.Dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, g gitx.Runner, msg string) string {
	t.Helper()
	run(t, g, "add", "-A")
	run(t, g, "commit", "-q", "-m", msg)
	return run(t, g, "rev-parse", "HEAD")
}

// Scenario: bot sync skipped a conflicting carry; alice re-applied it by hand
// on the sync branch. Train must record a resolution such that the next sync
// (onto a further tag with the same context) applies cleanly, and the cache
// must round-trip through the orphan branch to a second clone.
func TestTrainPushPull(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "alice")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "a@example.com")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")

	up := gitx.Runner{Dir: filepath.Join(root, "up")}
	os.MkdirAll(up.Dir, 0o755)
	run(t, up, "init", "-q", "-b", "master")
	write(t, up, "f.go", "a\nb\nc\n")
	commit(t, up, "init")
	run(t, up, "tag", "v1.0.0")
	write(t, up, "f.go", "a\nB-up\nc\n")
	commit(t, up, "churn")
	run(t, up, "tag", "v1.1.0")
	write(t, up, "g.go", "unrelated\n")
	commit(t, up, "more")
	run(t, up, "tag", "v1.2.0")

	origin := filepath.Join(root, "origin.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", origin).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	fork := gitx.Runner{Dir: filepath.Join(root, "fork")}
	if out, err := exec.Command("git", "clone", "-q", up.Dir, fork.Dir).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	run(t, fork, "remote", "set-url", "origin", origin)
	run(t, fork, "remote", "add", "upstream", up.Dir)
	run(t, fork, "fetch", "-q", "--tags", "upstream")
	run(t, fork, "checkout", "-qb", "main", "v1.0.0")
	write(t, fork, fsync.BaseFile, "v1.0.0\n")
	commit(t, fork, "UPSTREAM: <carry>: forksync: base on v1.0.0\n\nDownstream-Owner: @bot\nForksync-Base: true")
	write(t, fork, "f.go", "a\nB-fork\nc\n")
	patchSHA := commit(t, fork, "UPSTREAM: <carry>: tweak\n\nDownstream-Owner: @alice\nUpstream: none: ours")

	// bot sync → conflict, branch sync/v1.1.0 has only the base bump
	t.Setenv("GIT_AUTHOR_NAME", fsync.BotName)
	t.Setenv("GIT_COMMITTER_NAME", fsync.BotName)
	res, err := fsync.Run(ctx, fork, fsync.Options{Remote: "upstream", Ref: "v1.1.0", Head: "main", Branch: "sync/v1.1.0", BumpOwner: "@bot", ContinueOnConflict: true})
	if err != fsync.ErrConflicts || len(res.Conflicts) != 1 {
		t.Fatalf("expected one conflict, got %v %+v", err, res)
	}
	// the bot's rr-cache now has a preimage; throw it away to simulate a
	// different machine (alice's laptop) doing the fix with no rerere at all
	os.RemoveAll(filepath.Join(fork.Dir, ".git", "rr-cache"))
	run(t, fork, "config", "rerere.enabled", "false")

	// alice re-applies by hand (no rerere involved)
	t.Setenv("GIT_AUTHOR_NAME", "alice")
	t.Setenv("GIT_COMMITTER_NAME", "alice")
	_, _ = fork.Git(ctx, "cherry-pick", patchSHA)
	write(t, fork, "f.go", "a\nB-fork\nc\n")
	run(t, fork, "add", "f.go")
	run(t, fork, "-c", "core.editor=true", "cherry-pick", "--continue")
	run(t, fork, "push", "-q", "origin", "main", "sync/v1.1.0")

	// CI: train from alice's commit, push cache
	stack, err := patch.List(ctx, fork, "v1.0.0", "main")
	if err != nil {
		t.Fatal(err)
	}
	learned, err := Train(ctx, fork, stack, "v1.1.0", "sync/v1.1.0")
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	if len(learned) != 1 || learned[0].Patch.SHA != patchSHA {
		t.Fatalf("learned = %+v", learned)
	}
	if pushed, err := Push(ctx, fork, "origin", DefaultBranch); err != nil || !pushed {
		t.Fatalf("push: %v %v", pushed, err)
	}
	if pushed, _ := Push(ctx, fork, "origin", DefaultBranch); pushed {
		t.Fatal("second push should be a no-op")
	}

	// fresh clone (next CI run): pull cache, sync main onto v1.2.0 → no conflict
	ci := gitx.Runner{Dir: filepath.Join(root, "ci")}
	if out, err := exec.Command("git", "clone", "-q", "-b", "main", origin, ci.Dir).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	run(t, ci, "remote", "add", "upstream", up.Dir)
	run(t, ci, "fetch", "-q", "--tags", "upstream")
	if ok, err := Pull(ctx, ci, "origin", DefaultBranch); err != nil || !ok {
		t.Fatalf("pull: %v %v", ok, err)
	}
	t.Setenv("GIT_AUTHOR_NAME", fsync.BotName)
	t.Setenv("GIT_COMMITTER_NAME", fsync.BotName)
	res, err = fsync.Run(ctx, ci, fsync.Options{Remote: "upstream", Ref: "v1.2.0", Head: "main", Branch: "sync/v1.2.0", BumpOwner: "@bot", ContinueOnConflict: true})
	if err != nil {
		t.Fatalf("sync after pull: %v conflicts=%+v", err, res.Conflicts)
	}
	if got := run(t, ci, "show", "HEAD~1:f.go"); got != "a\nB-fork\nc" {
		t.Fatalf("replayed resolution wrong: %q", got)
	}
}
