// Package patch models the carried patch stack: every downstream commit is
// classified by its subject prefix and annotated via git trailers.
package patch

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/helayoty/forksync/internal/gitx"
)

type Kind int

const (
	KindUnknown  Kind = iota
	KindBackport      // UPSTREAM: <PR#>:   dropped once PR is in the target ref
	KindCarry         // UPSTREAM: <carry>: permanent divergence
	KindDrop          // UPSTREAM: <drop>:  temporary; Drop-when: vX.Y.Z
)

func (k Kind) String() string {
	switch k {
	case KindBackport:
		return "backport"
	case KindCarry:
		return "carry"
	case KindDrop:
		return "drop"
	}
	return "unknown"
}

const (
	TrailerOwner    = "Downstream-Owner"
	TrailerDropWhen = "Drop-when"
	TrailerUpstream = "Upstream"      // <carry> only: PR URL/number (pending) or "none: <reason>" (permanent)
	TrailerBaseBump = "Forksync-Base" // marks the generated UPSTREAM_BASE bump commit
)

var (
	subjectRe      = regexp.MustCompile(`^UPSTREAM: (<carry>|<drop>|[0-9]+): \S`)
	pickedRe       = regexp.MustCompile(`\(cherry picked from commit ([0-9a-f]{7,40})\)`)
	trailerRe      = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*): (.+)$`)
	semverTagRe    = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)
	upstreamPRRe   = regexp.MustCompile(`^(?:https?://github\.com/[^/\s]+/[^/\s]+/pull/|#)?(\d+)$`)
	upstreamNoneRe = regexp.MustCompile(`^none:\s*\S`)
)

// UpstreamStatus classifies a <carry> by its Upstream: trailer.
type UpstreamStatus int

const (
	UpstreamUnset     UpstreamStatus = iota
	UpstreamPending                  // PR open or merged upstream; dropped when merged into the target
	UpstreamPermanent                // "none: <reason>"
)

func (s UpstreamStatus) String() string {
	switch s {
	case UpstreamPending:
		return "pending"
	case UpstreamPermanent:
		return "permanent"
	}
	return "unset"
}

type Patch struct {
	SHA          string
	Subject      string
	Body         string
	Kind         Kind
	PR           int // backport PR number, or the pending upstream PR for a <carry>
	Owner        string
	DropWhen     string
	Upstream     string
	UpstreamSt   UpstreamStatus
	UpstreamSHAs []string
	IsBaseBump   bool
}

func Parse(sha, message string) Patch {
	message = strings.ReplaceAll(message, "\r\n", "\n")
	subject, body, _ := strings.Cut(message, "\n")
	p := Patch{SHA: sha, Subject: strings.TrimSpace(subject), Body: body}

	if m := subjectRe.FindStringSubmatch(p.Subject); m != nil {
		switch m[1] {
		case "<carry>":
			p.Kind = KindCarry
		case "<drop>":
			p.Kind = KindDrop
		default:
			p.Kind = KindBackport
			p.PR, _ = strconv.Atoi(m[1])
		}
	}
	for _, m := range pickedRe.FindAllStringSubmatch(message, -1) {
		p.UpstreamSHAs = append(p.UpstreamSHAs, m[1])
	}
	for _, line := range strings.Split(body, "\n") {
		m := trailerRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		switch m[1] {
		case TrailerOwner:
			p.Owner = m[2]
		case TrailerDropWhen:
			p.DropWhen = m[2]
		case TrailerUpstream:
			p.Upstream = strings.TrimSpace(m[2])
			if pm := upstreamPRRe.FindStringSubmatch(p.Upstream); pm != nil {
				p.UpstreamSt = UpstreamPending
				if p.Kind == KindCarry {
					p.PR, _ = strconv.Atoi(pm[1])
				}
			} else if upstreamNoneRe.MatchString(p.Upstream) {
				p.UpstreamSt = UpstreamPermanent
			}
		case TrailerBaseBump:
			p.IsBaseBump = true
		}
	}
	return p
}

// List returns the patch stack base..head, oldest first.
func List(ctx context.Context, g gitx.Runner, base, head string) ([]Patch, error) {
	out, err := g.Git(ctx, "log", "--reverse", "--no-merges", "--format=%H%x00%B%x1e", base+".."+head)
	if err != nil {
		return nil, err
	}
	var ps []Patch
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimSpace(rec)
		if rec == "" {
			continue
		}
		sha, msg, _ := strings.Cut(rec, "\x00")
		ps = append(ps, Parse(sha, msg))
	}
	return ps, nil
}

// Lint returns one error string per violated rule for the patch.
func Lint(p Patch) []string {
	var errs []string
	switch p.Kind {
	case KindUnknown:
		errs = append(errs, "subject must start with 'UPSTREAM: <PR#>: ', 'UPSTREAM: <carry>: ' or 'UPSTREAM: <drop>: '")
	case KindBackport:
		if p.PR <= 0 {
			errs = append(errs, "backport PR number must be positive")
		}
	case KindCarry:
		if p.IsBaseBump {
			break
		}
		if p.Owner == "" {
			errs = append(errs, "<carry> requires a '"+TrailerOwner+":' trailer")
		}
		if p.UpstreamSt == UpstreamUnset {
			errs = append(errs, "<carry> requires '"+TrailerUpstream+": <PR URL or #N>' (pending) or '"+TrailerUpstream+": none: <reason>' (permanent)")
		}
	case KindDrop:
		if p.Owner == "" {
			errs = append(errs, "<drop> requires a '"+TrailerOwner+":' trailer")
		}
		if !semverTagRe.MatchString(p.DropWhen) {
			errs = append(errs, "<drop> requires a '"+TrailerDropWhen+": vX.Y.Z' trailer")
		}
	}
	return errs
}

// VersionAtLeast reports tag >= min, comparing only MAJOR.MINOR.PATCH.
// Pre-release suffixes are ignored; the sync lane only targets final tags.
func VersionAtLeast(tag, min string) (bool, error) {
	a, err := semver3(tag)
	if err != nil {
		return false, err
	}
	b, err := semver3(min)
	if err != nil {
		return false, err
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i], nil
		}
	}
	return true, nil
}

func semver3(s string) ([3]int, error) {
	var v [3]int
	m := semverTagRe.FindStringSubmatch(s)
	if m == nil {
		return v, fmt.Errorf("not a vX.Y.Z tag: %q", s)
	}
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, nil
}
