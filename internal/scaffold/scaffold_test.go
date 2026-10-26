package scaffold

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

const sha = "0123456789abcdef0123456789abcdef01234567"

func repo(t *testing.T) gitx.Runner {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@x")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}, {"tag", "v1.0.0"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(string(out))
		}
	}
	return gitx.Runner{Dir: dir}
}

func TestRun_ModesAndCommits(t *testing.T) {
	for _, tc := range []struct {
		ado        string
		wantADO    bool
		wantQueued bool
	}{
		{ADONone, false, false},
		{ADOTrigger, true, false},
		{ADOQueue, true, true},
	} {
		t.Run("ado="+tc.ado, func(t *testing.T) {
			g := repo(t)
			o := Options{UpstreamRepo: "kubernetes-sigs/kueue", Base: "v1.0.0", Owner: "@me",
				ForksyncRepo: "acme/forksync", ForksyncSHA: sha, ForksyncVersion: "v1.2.3", ADO: tc.ado, Commit: true}
			written, err := Run(context.Background(), g, o)
			if err != nil {
				t.Fatal(err)
			}
			_, hasADO := os.Stat(filepath.Join(g.Dir, "azure-pipelines.yml"))
			if (hasADO == nil) != tc.wantADO {
				t.Fatalf("azure-pipelines.yml present=%v want %v", hasADO == nil, tc.wantADO)
			}
			rel, _ := os.ReadFile(filepath.Join(g.Dir, ".github/workflows/release.yml"))
			if got := strings.Contains(string(rel), "ado-org"); got != tc.wantQueued {
				t.Fatalf("release.yml queues ADO=%v want %v", got, tc.wantQueued)
			}
			if tc.ado == ADOTrigger {
				ado, _ := os.ReadFile(filepath.Join(g.Dir, "azure-pipelines.yml"))
				if !strings.Contains(string(ado), "fork.*") {
					t.Fatal("trigger mode must have a tag trigger")
				}
			}
			if tc.ado == ADOQueue {
				ado, _ := os.ReadFile(filepath.Join(g.Dir, "azure-pipelines.yml"))
				if !strings.Contains(string(ado), "trigger: none") {
					t.Fatal("queue mode must not self-trigger")
				}
			}

			// no placeholder survives; sha pin and prow detection applied
			for _, w := range written {
				b, _ := os.ReadFile(filepath.Join(g.Dir, w))
				for _, ph := range []string{"FORKSYNC_OWNER", "FORKSYNC_SHA", "FORKSYNC_VERSION", "UPSTREAM_REPO", "TAG_GLOB", "BACKPORT_STYLE"} {
					if strings.Contains(string(b), ph) {
						t.Errorf("%s still contains %s", w, ph)
					}
				}
			}
			cp, _ := os.ReadFile(filepath.Join(g.Dir, ".github/workflows/cherry-pick.yml"))
			if !strings.Contains(string(cp), "backport-style: prow") || !strings.Contains(string(cp), "@"+sha) {
				t.Errorf("cherry-pick.yml:\n%s", cp)
			}

			// exactly two commits: CI carry (lint-clean) then base bump
			ps, err := patch.List(context.Background(), g, "v1.0.0", "HEAD")
			if err != nil || len(ps) != 2 {
				t.Fatalf("stack = %+v %v", ps, err)
			}
			if ps[0].IsBaseBump || !ps[1].IsBaseBump {
				t.Fatalf("order wrong: %+v", ps)
			}
			if errs := patch.Lint(ps[0]); len(errs) != 0 {
				t.Fatalf("CI commit fails lint: %v", errs)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	if err := (Options{ForksyncSHA: sha, ADO: "both"}).Validate(); err == nil {
		t.Error("bad ado mode accepted")
	}
	if err := (Options{ForksyncSHA: "v1", ADO: ADONone}).Validate(); err == nil {
		t.Error("short sha accepted")
	}
	if err := (Options{ForksyncSHA: sha, ADO: ADOQueue, ForksyncVersion: sha}).Validate(); err == nil {
		t.Error("ado with sha-only version accepted")
	}
}
