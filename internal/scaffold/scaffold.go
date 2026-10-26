// Package scaffold writes the per-fork files: UPSTREAM_BASE, the caller
// workflows, and optionally the Azure DevOps consumer pipeline.
package scaffold

import (
	"context"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/helayoty/forksync/internal/gitx"
	"github.com/helayoty/forksync/internal/patch"
	"github.com/helayoty/forksync/internal/sync"
)

//go:embed all:templates
var templates embed.FS

var releaseTagRe = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// ADO hand-off modes. Exactly one party starts the ADO build.
const (
	ADONone    = ""        // no Azure DevOps
	ADOTrigger = "trigger" // ADO triggers itself on the v*fork.* tag push
	ADOQueue   = "queue"   // the GitHub release workflow queues ADO over REST
)

type Options struct {
	UpstreamRepo    string // owner/name
	Base            string // upstream tag to start from
	Owner           string // Downstream-Owner for the base commit
	TagGlob         string
	BackportStyle   string // k8s | prow
	ForksyncRepo    string // owner/name hosting the reusable workflows
	ForksyncSHA     string // commit the callers pin to
	ForksyncVersion string // human-readable tag for the pin comment / ADO resource ref
	ADO             string // ADONone | ADOTrigger | ADOQueue
	Commit          bool
}

// Detect fills defaults from the upstream repo name.
func (o *Options) Detect() {
	if o.TagGlob == "" {
		o.TagGlob = "v*"
	}
	if o.BackportStyle == "" {
		if strings.HasPrefix(o.UpstreamRepo, "kubernetes-sigs/") {
			o.BackportStyle = "prow"
		} else {
			o.BackportStyle = "k8s"
		}
	}
	if o.ForksyncVersion == "" {
		o.ForksyncVersion = o.ForksyncSHA
	}
}

func (o Options) Validate() error {
	switch o.ADO {
	case ADONone, ADOTrigger, ADOQueue:
	default:
		return fmt.Errorf("--ado must be %q or %q", ADOTrigger, ADOQueue)
	}
	if len(o.ForksyncSHA) != 40 {
		return fmt.Errorf("--forksync-sha must be a full 40-char commit sha (got %q)", o.ForksyncSHA)
	}
	if o.ADO != ADONone && !releaseTagRe.MatchString(o.ForksyncVersion) {
		return fmt.Errorf("--ado needs --forksync-version vX.Y.Z (ADO pins the image tag and template ref to it; got %q)", o.ForksyncVersion)
	}
	return nil
}

// files maps destination path → embedded template for the chosen options.
func (o Options) files() map[string]string {
	m := map[string]string{
		".github/workflows/upstream-sync.yml": "templates/github/upstream-sync.yml",
		".github/workflows/patch-policy.yml":  "templates/github/patch-policy.yml",
		".github/workflows/cherry-pick.yml":   "templates/github/cherry-pick.yml",
	}
	switch o.ADO {
	case ADOQueue:
		m[".github/workflows/release.yml"] = "templates/github/release-ado-queue.yml"
		m["azure-pipelines.yml"] = "templates/ado/consumer-queue.yml"
	case ADOTrigger:
		m[".github/workflows/release.yml"] = "templates/github/release-github.yml"
		m["azure-pipelines.yml"] = "templates/ado/consumer-trigger.yml"
	default:
		m[".github/workflows/release.yml"] = "templates/github/release-github.yml"
	}
	return m
}

func Run(ctx context.Context, g gitx.Runner, o Options) ([]string, error) {
	o.Detect()
	if err := o.Validate(); err != nil {
		return nil, err
	}
	repl := strings.NewReplacer(
		"UPSTREAM_REPO", o.UpstreamRepo,
		"TAG_GLOB", o.TagGlob,
		"BACKPORT_STYLE", o.BackportStyle,
		"FORKSYNC_OWNER/forksync", o.ForksyncRepo,
		"FORKSYNC_SHA", o.ForksyncSHA,
		"FORKSYNC_VERSION", o.ForksyncVersion,
	)

	var written []string
	write := func(rel string, body []byte) error {
		dst := filepath.Join(g.Dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte(repl.Replace(string(body))), 0o644); err != nil {
			return err
		}
		written = append(written, rel)
		return nil
	}

	for dst, src := range o.files() {
		b, err := templates.ReadFile(src)
		if err != nil {
			return written, err
		}
		if err := write(dst, b); err != nil {
			return written, err
		}
	}

	if !o.Commit {
		return append(written, sync.BaseFile), os.WriteFile(filepath.Join(g.Dir, sync.BaseFile), []byte(o.Base+"\n"), 0o644)
	}

	// Two commits on purpose: the CI files are an ordinary <carry> that the
	// sync lane re-applies, while the base bump is regenerated on every sync
	// and must contain nothing but UPSTREAM_BASE.
	if _, err := g.Git(ctx, append([]string{"add", "--"}, written...)...); err != nil {
		return written, err
	}
	ci := fmt.Sprintf("UPSTREAM: <carry>: forksync: CI workflows\n\n%s: %s\n%s: none: fork CI, never upstream\n",
		patch.TrailerOwner, o.Owner, patch.TrailerUpstream)
	if _, err := g.Git(ctx, "commit", "-q", "-m", ci); err != nil {
		return written, err
	}

	if err := write(sync.BaseFile, []byte(o.Base+"\n")); err != nil {
		return written, err
	}
	if _, err := g.Git(ctx, "add", "--", sync.BaseFile); err != nil {
		return written, err
	}
	bump := fmt.Sprintf("UPSTREAM: <carry>: forksync: base on %s\n\n%s: %s\n%s: true\n",
		o.Base, patch.TrailerOwner, o.Owner, patch.TrailerBaseBump)
	if _, err := g.Git(ctx, "commit", "-q", "-m", bump); err != nil {
		return written, err
	}
	return written, nil
}
