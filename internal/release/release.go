// Package release computes downstream version tags of the form
// <UPSTREAM_BASE><sep>fork.<N>, where N increments per downstream release on
// the same upstream base and resets when the base moves.
package release

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/helayoty/forksync/internal/gitx"
)

// Separator selects how the fork counter is attached to the upstream version.
//
//	"-"  v1.38.0-fork.3   semver pre-release: sorts BEFORE v1.38.0 (Helm, go mod)
//	"+"  v1.38.0+fork.3   semver build metadata: equal to v1.38.0 for ordering;
//	                      invalid in OCI image tags, so image tags need a rewrite
type Options struct {
	Remote    string
	Base      string // UPSTREAM_BASE content
	Separator string
}

func (o Options) prefix() string { return o.Base + o.Separator + "fork." }

// Next returns the next unused tag for the base, consulting both local and
// remote tags so a stale clone cannot reuse a number.
func Next(ctx context.Context, g gitx.Runner, o Options) (string, error) {
	switch o.Separator {
	case "":
		o.Separator = "-"
	case "-", "+":
	default:
		return "", fmt.Errorf("separator must be \"-\" or \"+\" (got %q)", o.Separator)
	}
	pfx := o.prefix()
	re := regexp.MustCompile("^" + regexp.QuoteMeta(pfx) + `(\d+)$`)

	max := 0
	consider := func(tag string) {
		if m := re.FindStringSubmatch(tag); m != nil {
			if n, _ := strconv.Atoi(m[1]); n > max {
				max = n
			}
		}
	}
	local, err := g.Git(ctx, "tag", "--list", pfx+"*")
	if err != nil {
		return "", err
	}
	for _, t := range strings.Split(local, "\n") {
		consider(t)
	}
	if o.Remote != "" {
		remote, err := g.Git(ctx, "ls-remote", "--tags", "--refs", o.Remote, "refs/tags/"+pfx+"*")
		if err != nil {
			return "", err
		}
		for _, line := range strings.Split(remote, "\n") {
			if _, ref, ok := strings.Cut(line, "\t"); ok {
				consider(strings.TrimPrefix(ref, "refs/tags/"))
			}
		}
	}
	return fmt.Sprintf("%s%d", pfx, max+1), nil
}

// ImageTag rewrites a version tag into a form valid for OCI image references
// ([A-Za-z0-9_.-], no '+').
func ImageTag(v string) string {
	return strings.NewReplacer("+", "-").Replace(v)
}
