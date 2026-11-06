// Package bisect holds the two search algorithms tw-bisect runs: a binary
// search over an ordered list of snapshots, and a dependency-aware search
// over the set of packages that changed between two snapshots.
//
// Both are written against oracle functions so they can be tested without
// containers.
package bisect

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Verdict is the outcome of one test run.
type Verdict int

const (
	Good Verdict = iota
	Bad
	Skip // the test could not be run (install failure, exit 125)
)

func (v Verdict) String() string {
	switch v {
	case Good:
		return "good"
	case Bad:
		return "bad"
	default:
		return "skip"
	}
}

// Step records one oracle call.
type Step struct {
	Label   string   // snapshot id, or a short description of the package subset
	Verdict Verdict  //
	Tried   []string // package mode: packages requested from the bad snapshot
	Changed []string // package mode: packages that actually changed
}

// SnapshotResult is the outcome of Snapshots.
type SnapshotResult struct {
	LastGood string
	FirstBad string
	Skipped  []string // untestable snapshots between LastGood and FirstBad
	Steps    []Step
}

// Snapshots binary-searches items, where items[0] is known good and
// items[len-1] is known bad, calling test on the candidates in between.
// Snapshots that return Skip are set aside, as git bisect does, and the
// search continues on the nearest untested neighbour.
func Snapshots(ctx context.Context, items []string, test func(ctx context.Context, item string) (Verdict, error)) (SnapshotResult, error) {
	var res SnapshotResult
	if len(items) < 2 {
		return res, errors.New("need a good and a bad snapshot")
	}
	lo, hi := 0, len(items)-1
	skipped := map[int]bool{}
	for {
		mid := pickUntested(lo, hi, skipped)
		if mid < 0 {
			break
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
		v, err := test(ctx, items[mid])
		if err != nil {
			return res, fmt.Errorf("testing %s: %w", items[mid], err)
		}
		res.Steps = append(res.Steps, Step{Label: items[mid], Verdict: v})
		switch v {
		case Good:
			lo = mid
		case Bad:
			hi = mid
		default:
			skipped[mid] = true
		}
	}
	res.LastGood, res.FirstBad = items[lo], items[hi]
	for i := lo + 1; i < hi; i++ {
		res.Skipped = append(res.Skipped, items[i])
	}
	return res, nil
}

// pickUntested returns the untested index nearest the middle of (lo, hi),
// or -1 when every index strictly between them has been skipped.
func pickUntested(lo, hi int, skipped map[int]bool) int {
	mid := (lo + hi) / 2
	for d := 0; ; d++ {
		a, b := mid-d, mid+d
		if a <= lo && b >= hi {
			return -1
		}
		if a > lo && a < hi && !skipped[a] {
			return a
		}
		if b > lo && b < hi && !skipped[b] {
			return b
		}
	}
}

// Apply upgrades pkgs (and whatever the solver drags along) from the bad
// snapshot on a pristine good system, runs the test, and reports every
// package that actually changed together with the verdict.
type Apply func(ctx context.Context, pkgs []string) (changed []string, v Verdict, err error)

// PackageResult is the outcome of Packages.
type PackageResult struct {
	// Culprits is the smallest set found whose upgrade makes the test fail.
	Culprits []string
	// Closure lists everything that changed when Culprits were upgraded
	// alone (only when Confirm ran).
	Closure []string
	// Inseparable is set when dependencies made every tested subset pull in
	// the whole remaining set, so Culprits could not be split further.
	Inseparable bool
	// Interaction is set when subsets passed individually but the whole set
	// fails: the regression needs more than one of the Culprits.
	Interaction bool
	// Unresolved is set when no subset could be applied at all.
	Unresolved bool
	Steps      []Step
}

// Options tunes Packages.
type Options struct {
	// Confirm re-applies the final culprit set alone to record its closure
	// and make sure it reproduces the failure.
	Confirm bool
}

// Packages narrows candidates (packages that differ between the good and
// the bad snapshot, in a stable order) to the ones responsible for a
// failing test. The whole candidate set is assumed to fail.
//
// Unlike a plain binary search, a subset cannot always be applied in
// isolation: upgrading one package may force others along. The search
// therefore works on what actually changed, not on what was asked for.
func Packages(ctx context.Context, candidates []string, apply Apply, opt Options) (PackageResult, error) {
	var res PackageResult
	if len(candidates) == 0 {
		return res, errors.New("no candidate packages")
	}
	suspects := append([]string(nil), candidates...)
	record := func(tried, changed []string, v Verdict) {
		res.Steps = append(res.Steps, Step{Label: describe(tried), Verdict: v, Tried: tried, Changed: changed})
	}
	for len(suspects) > 1 {
		parts := 2
	finer:
		if parts > len(suspects) {
			res.Unresolved, res.Culprits = true, suspects
			return res, nil
		}
		progressed, coupled := false, false
		for _, chunk := range split(suspects, parts) {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			changed, v, err := apply(ctx, chunk)
			if err != nil {
				return res, err
			}
			record(chunk, changed, v)
			affected := intersect(suspects, union(chunk, changed))
			switch v {
			case Bad:
				if len(affected) < len(suspects) {
					suspects, progressed = affected, true
				} else {
					coupled = true
				}
			case Good:
				if len(affected) == len(suspects) {
					res.Interaction, res.Culprits = true, suspects
					return res, nil
				}
				suspects, progressed = minus(suspects, affected), true
			}
			if progressed {
				break
			}
		}
		if progressed {
			continue
		}
		if coupled {
			res.Inseparable, res.Culprits = true, suspects
			break
		}
		parts *= 2
		goto finer
	}
	res.Culprits = suspects
	if last := lastStep(res.Steps); last != nil && sameSet(last.Tried, suspects) && last.Verdict == Bad {
		// The final narrowing step already applied exactly this set alone.
		res.Closure = union(suspects, last.Changed)
		return res, nil
	}
	if opt.Confirm && !res.Inseparable {
		changed, v, err := apply(ctx, res.Culprits)
		if err != nil {
			return res, err
		}
		record(res.Culprits, changed, v)
		res.Closure = union(res.Culprits, changed)
		if v == Good {
			res.Interaction = true
		}
	}
	return res, nil
}

func lastStep(steps []Step) *Step {
	if len(steps) == 0 {
		return nil
	}
	return &steps[len(steps)-1]
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	in := map[string]bool{}
	for _, s := range a {
		in[s] = true
	}
	for _, s := range b {
		if !in[s] {
			return false
		}
	}
	return true
}

func describe(pkgs []string) string {
	if len(pkgs) <= 3 {
		return strings.Join(pkgs, ", ")
	}
	return fmt.Sprintf("%s, %s … (%d packages)", pkgs[0], pkgs[1], len(pkgs))
}

func split(items []string, parts int) [][]string {
	var out [][]string
	n := len(items)
	for i := range parts {
		a, b := i*n/parts, (i+1)*n/parts
		if b > a {
			out = append(out, items[a:b])
		}
	}
	return out
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string(nil), a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// intersect keeps the elements of order that appear in set, in order's order.
func intersect(order, set []string) []string {
	in := map[string]bool{}
	for _, s := range set {
		in[s] = true
	}
	var out []string
	for _, s := range order {
		if in[s] {
			out = append(out, s)
		}
	}
	return out
}

func minus(order, set []string) []string {
	in := map[string]bool{}
	for _, s := range set {
		in[s] = true
	}
	var out []string
	for _, s := range order {
		if !in[s] {
			out = append(out, s)
		}
	}
	return out
}
