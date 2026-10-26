package sync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helayoty/forksync/internal/gitx"
	"github.com/helayoty/forksync/internal/patch"
)

// fixture builds a synthetic upstream with a Tide-style history and a fork
// whose stack exercises each drop signal:
//
//	upstream master:  init ─ merge #100 (fix api) ─ refactor sched ─[v1.38.0]─ feature ─ master
//	upstream release-1.38: v1.38.0 ─ "Automated cherry pick of #300" (-x <master feature sha>) ─[v1.38.1]
//	fork main @ v1.37.0 + stack
type fixture struct {
	t        *testing.T
	up, fork gitx.Runner
	ctx      context.Context
	shas     map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, ctx: context.Background(), shas: map[string]string{}}
	f.up = gitx.Runner{Dir: filepath.Join(root, "upstream")}
	f.fork = gitx.Runner{Dir: filepath.Join(root, "fork")}
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "tester")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.com")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")

	must(t, os.MkdirAll(f.up.Dir, 0o755))
	f.git(f.up, "init", "-q", "-b", "master")
	f.write(f.up, "sched.go", "a\nb\nc\n")
	f.write(f.up, "api.go", "x\n")
	f.commit(f.up, "init")
	f.git(f.up, "tag", "v1.37.0")

	// PR #100 merged via Tide
	f.git(f.up, "checkout", "-qb", "pr100")
	f.write(f.up, "api.go", "x\nfix100\n")
	f.shas["pr100"] = f.commit(f.up, "fix api")
	f.git(f.up, "checkout", "-q", "master")
	f.git(f.up, "merge", "-q", "--no-ff", "pr100", "-m", "Merge pull request #100 from someone/fix")

	f.write(f.up, "sched.go", "a\nB-upstream\nc\n")
	f.commit(f.up, "refactor sched")
	f.git(f.up, "tag", "v1.38.0")

	// master moves on: PR #300 lands after the minor was cut
	f.write(f.up, "feature.go", "feature\n")
	f.shas["pr300"] = f.commit(f.up, "add feature")
	// release branch backport of #300 with -x provenance, k/k title style
	f.git(f.up, "checkout", "-qb", "release-1.38", "v1.38.0")
	f.git(f.up, "cherry-pick", "-x", f.shas["pr300"])
	f.git(f.up, "commit", "-q", "--amend", "-m",
		"Automated cherry pick of #300: add feature\n\n(cherry picked from commit "+f.shas["pr300"]+")")
	f.git(f.up, "tag", "v1.38.1")
	f.git(f.up, "checkout", "-q", "master")

	// fork
	out, err := exec.Command("git", "clone", "-q", f.up.Dir, f.fork.Dir).CombinedOutput()
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	f.git(f.fork, "remote", "add", "upstream", f.up.Dir)
	f.git(f.fork, "fetch", "-q", "--tags", "upstream")
	f.git(f.fork, "checkout", "-qb", "main", "v1.37.0")
	f.write(f.fork, BaseFile, "v1.37.0\n")
	f.commit(f.fork, "UPSTREAM: <carry>: forksync: base on v1.37.0\n\nDownstream-Owner: @bot\nForksync-Base: true")
	return f
}

func (f *fixture) git(g gitx.Runner, args ...string) string {
	f.t.Helper()
	out, err := g.Git(f.ctx, args...)
	if err != nil {
		f.t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func (f *fixture) write(g gitx.Runner, name, body string) {
	f.t.Helper()
	must(f.t, os.WriteFile(filepath.Join(g.Dir, name), []byte(body), 0o644))
}

func (f *fixture) commit(g gitx.Runner, msg string) string {
	f.t.Helper()
	f.git(g, "add", "-A")
	f.git(g, "commit", "-q", "--allow-empty", "-m", msg)
	return f.git(g, "rev-parse", "HEAD")
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) sync(ref string) (*Result, error) {
	return Run(f.ctx, f.fork, Options{
		Remote: "upstream", Ref: ref, Head: "main", Branch: "sync/" + ref,
		BumpOwner: "@bot", ContinueOnConflict: true,
	})
}

func names(ds []Dropped) []string {
	var s []string
	for _, d := range ds {
		s = append(s, d.Patch.Subject)
	}
	return s
}

func TestSync_DropSignals(t *testing.T) {
	f := newFixture(t)

	// 1. backport with -x: upstream sha reachable from v1.38.0
	f.write(f.fork, "api.go", "x\nfix100\n")
	f.commit(f.fork, "UPSTREAM: 100: fix api\n\n(cherry picked from commit "+f.shas["pr100"]+")\nDownstream-Owner: @a")
	// 2. <drop> satisfied by tag
	f.write(f.fork, "hack.txt", "hack\n")
	f.commit(f.fork, "UPSTREAM: <drop>: temp hack\n\nDownstream-Owner: @a\nDrop-when: v1.38.0")
	// 3. absorbed: same change as upstream "refactor sched" but no provenance at all
	f.write(f.fork, "sched.go", "a\nB-upstream\nc\n")
	f.commit(f.fork, "UPSTREAM: <carry>: same as upstream refactor\n\nDownstream-Owner: @a\nUpstream: none: already landed")
	// 4. survives
	f.write(f.fork, "mine.go", "mine\n")
	f.commit(f.fork, "UPSTREAM: <carry>: downstream feature\n\nDownstream-Owner: @a\nUpstream: none: product specific")

	res, err := f.sync("v1.38.0")
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	got := names(res.Dropped)
	want := []string{"UPSTREAM: 100: fix api", "UPSTREAM: <drop>: temp hack", "UPSTREAM: <carry>: same as upstream refactor"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("dropped = %v, want %v", got, want)
	}
	if len(res.Kept) != 1 || res.Kept[0].Subject != "UPSTREAM: <carry>: downstream feature" {
		t.Fatalf("kept = %+v", res.Kept)
	}
	if !strings.Contains(res.Dropped[2].Reason, "absorbed upstream") {
		t.Fatalf("absorbed reason = %q", res.Dropped[2].Reason)
	}
	if base := strings.TrimSpace(f.git(f.fork, "show", "HEAD:"+BaseFile)); base != "v1.38.0" {
		t.Fatalf("UPSTREAM_BASE = %q", base)
	}
}

func TestSync_PatchReleaseBackportDrops(t *testing.T) {
	f := newFixture(t)
	f.git(f.fork, "checkout", "-q", "-B", "main", "v1.38.0")
	f.write(f.fork, BaseFile, "v1.38.0\n")
	f.commit(f.fork, "UPSTREAM: <carry>: forksync: base on v1.38.0\n\nDownstream-Owner: @bot\nForksync-Base: true")

	// urgent-lane pick of #300 from MASTER (sha not on release-1.38), -x provenance
	f.git(f.fork, "cherry-pick", "-x", f.shas["pr300"])
	msg := f.git(f.fork, "log", "-1", "--format=%B")
	f.git(f.fork, "commit", "-q", "--amend", "-m", "UPSTREAM: 300: "+msg+"\nDownstream-Owner: @a")
	// a pending <carry> whose upstream PR is #100 — it's in v1.38.0 and therefore v1.38.1
	f.write(f.fork, "api.go", "x\nfix100\nextra\n")
	f.commit(f.fork, "UPSTREAM: <carry>: api tweak awaiting upstream\n\nDownstream-Owner: @a\nUpstream: https://github.com/k/k/pull/100")

	res, err := f.sync("v1.38.1")
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.Dropped) != 2 {
		t.Fatalf("dropped = %v (want 2)", names(res.Dropped))
	}
	// #300: master sha is NOT an ancestor of v1.38.1; must be caught via the
	// backport's "(cherry picked from commit <sha>)" body (signal 2).
	if r := res.Dropped[0].Reason; !strings.Contains(r, "was backported to v1.38.1") {
		t.Fatalf("pr300 reason = %q", r)
	}
	if r := res.Dropped[1].Reason; !strings.Contains(r, "PR #100 merged upstream") {
		t.Fatalf("pending carry reason = %q", r)
	}
}

func TestSync_PRNumberFallbackWithoutProvenance(t *testing.T) {
	f := newFixture(t)
	f.git(f.fork, "checkout", "-q", "-B", "main", "v1.38.0")
	f.write(f.fork, BaseFile, "v1.38.0\n")
	f.commit(f.fork, "UPSTREAM: <carry>: forksync: base on v1.38.0\n\nDownstream-Owner: @bot\nForksync-Base: true")
	// hand-written backport, no -x line, content differs slightly → not "absorbed"
	f.write(f.fork, "feature.go", "feature\nhand-ported\n")
	f.commit(f.fork, "UPSTREAM: 300: add feature\n\nDownstream-Owner: @a")

	res, err := f.sync("v1.38.1")
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.Dropped) != 1 || !strings.Contains(res.Dropped[0].Reason, "PR #300 backported to v1.38.1") {
		t.Fatalf("dropped = %+v", res.Dropped)
	}
}

func TestSync_ConflictIsReportedAndSkipped(t *testing.T) {
	f := newFixture(t)
	f.write(f.fork, "sched.go", "a\nB-fork\nc\n")
	f.commit(f.fork, "UPSTREAM: <carry>: tweak sched\n\nDownstream-Owner: @alice\nUpstream: none: ours")
	f.write(f.fork, "mine.go", "mine\n")
	f.commit(f.fork, "UPSTREAM: <carry>: later patch\n\nDownstream-Owner: @bob\nUpstream: none: ours")

	res, err := f.sync("v1.38.0")
	if err != ErrConflicts {
		t.Fatalf("err = %v, want ErrConflicts", err)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0].Patch.Owner != "@alice" || res.Conflicts[0].Files[0] != "sched.go" {
		t.Fatalf("conflicts = %+v", res.Conflicts)
	}
	if len(res.Kept) != 1 || res.Kept[0].Owner != "@bob" {
		t.Fatalf("later patch must still land: kept = %+v", res.Kept)
	}
	if st := f.git(f.fork, "status", "--porcelain"); st != "" {
		t.Fatalf("tree left dirty:\n%s", st)
	}
}

func TestHumanCommits(t *testing.T) {
	f := newFixture(t)
	f.git(f.fork, "checkout", "-qb", "sync/x", "v1.38.0")
	t.Setenv("GIT_AUTHOR_NAME", BotName)
	f.commit(f.fork, "UPSTREAM: <carry>: bot")
	shas, err := HumanCommits(f.ctx, f.fork, "v1.38.0", "sync/x")
	if err != nil || len(shas) != 0 {
		t.Fatalf("bot-only: %v %v", shas, err)
	}
	t.Setenv("GIT_AUTHOR_NAME", "alice")
	f.commit(f.fork, "UPSTREAM: <carry>: human fix")
	shas, err = HumanCommits(f.ctx, f.fork, "v1.38.0", "sync/x")
	if err != nil || len(shas) != 1 {
		t.Fatalf("with human: %v %v", shas, err)
	}
}

func TestLint_UpstreamTrailer(t *testing.T) {
	cases := map[string]int{
		"UPSTREAM: <carry>: x\n\nDownstream-Owner: @a":                                           1,
		"UPSTREAM: <carry>: x\n\nDownstream-Owner: @a\nUpstream: none: reason":                   0,
		"UPSTREAM: <carry>: x\n\nDownstream-Owner: @a\nUpstream: https://github.com/o/r/pull/12": 0,
		"UPSTREAM: <carry>: x\n\nDownstream-Owner: @a\nUpstream: #12":                            0,
		"UPSTREAM: <carry>: x\n\nDownstream-Owner: @a\nUpstream: maybe later":                    1,
	}
	for msg, want := range cases {
		if got := len(patch.Lint(patch.Parse("deadbeef", msg))); got != want {
			t.Errorf("%q: %d errors, want %d", msg, got, want)
		}
	}
	p := patch.Parse("x", "UPSTREAM: <carry>: x\n\nUpstream: #12")
	if p.PR != 12 || p.UpstreamSt != patch.UpstreamPending {
		t.Errorf("pending carry parse: %+v", p)
	}
}
