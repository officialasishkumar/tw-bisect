package bisect

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

func snaps(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("s%02d", i)
	}
	return out
}

func TestSnapshotsFindsFirstBad(t *testing.T) {
	items := snaps(20)
	for firstBad := 1; firstBad < 20; firstBad++ {
		calls := 0
		res, err := Snapshots(context.Background(), items, func(_ context.Context, s string) (Verdict, error) {
			calls++
			var i int
			fmt.Sscanf(s, "s%d", &i)
			if i >= firstBad {
				return Bad, nil
			}
			return Good, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.FirstBad != items[firstBad] || res.LastGood != items[firstBad-1] {
			t.Errorf("firstBad=%d: got last good %s, first bad %s", firstBad, res.LastGood, res.FirstBad)
		}
		if calls > 5 {
			t.Errorf("firstBad=%d: %d oracle calls for 18 candidates, want <= 5", firstBad, calls)
		}
		if len(res.Skipped) != 0 {
			t.Errorf("unexpected skipped: %v", res.Skipped)
		}
	}
}

func TestSnapshotsWithSkips(t *testing.T) {
	items := snaps(10)
	// s05 and s06 cannot be tested; the break is at s06.
	res, err := Snapshots(context.Background(), items, func(_ context.Context, s string) (Verdict, error) {
		switch s {
		case "s05", "s06":
			return Skip, nil
		case "s07", "s08":
			return Bad, nil
		}
		return Good, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.LastGood != "s04" || res.FirstBad != "s07" {
		t.Errorf("got %s..%s", res.LastGood, res.FirstBad)
	}
	if !reflect.DeepEqual(res.Skipped, []string{"s05", "s06"}) {
		t.Errorf("skipped = %v", res.Skipped)
	}
}

func TestSnapshotsAdjacent(t *testing.T) {
	res, err := Snapshots(context.Background(), []string{"a", "b"}, func(context.Context, string) (Verdict, error) {
		t.Fatal("nothing to test")
		return Skip, nil
	})
	if err != nil || res.LastGood != "a" || res.FirstBad != "b" || len(res.Steps) != 0 {
		t.Errorf("res=%+v err=%v", res, err)
	}
}

// world simulates zypper: applying a set upgrades it plus its dependency
// closure; the test fails when every member of `culprits` is upgraded.
type world struct {
	deps     map[string][]string
	culprits []string
	skipIf   func(pkgs []string) bool
	calls    int
}

func (w *world) apply(_ context.Context, pkgs []string) ([]string, Verdict, error) {
	w.calls++
	if w.skipIf != nil && w.skipIf(pkgs) {
		return nil, Skip, nil
	}
	changed := map[string]bool{}
	var stack []string
	for _, p := range pkgs {
		stack = append(stack, p)
	}
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if changed[p] {
			continue
		}
		changed[p] = true
		stack = append(stack, w.deps[p]...)
	}
	var out []string
	for p := range changed {
		out = append(out, p)
	}
	for _, c := range w.culprits {
		if !changed[c] {
			return out, Good, nil
		}
	}
	return out, Bad, nil
}

func names(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("pkg%02d", i)
	}
	return out
}

func TestPackagesSingleCulprit(t *testing.T) {
	cands := names(40)
	for _, culprit := range []string{"pkg00", "pkg17", "pkg39"} {
		w := &world{culprits: []string{culprit}}
		res, err := Packages(context.Background(), cands, w.apply, Options{Confirm: true})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(res.Culprits, []string{culprit}) || res.Inseparable || res.Interaction {
			t.Errorf("%s: %+v", culprit, res)
		}
		if w.calls > 8 {
			t.Errorf("%s: %d calls for 40 candidates", culprit, w.calls)
		}
	}
}

func TestPackagesDependencyPullsCulprit(t *testing.T) {
	// pkg30 is in the second half, but upgrading pkg01 drags it in, so the
	// first half already fails; the search must follow what changed.
	cands := names(40)
	w := &world{culprits: []string{"pkg30"}, deps: map[string][]string{"pkg01": {"pkg30"}}}
	res, err := Packages(context.Background(), cands, w.apply, Options{Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Culprits, []string{"pkg30"}) {
		t.Errorf("culprits = %v (steps %+v)", res.Culprits, res.Steps)
	}
	if !reflect.DeepEqual(res.Closure, []string{"pkg30"}) {
		t.Errorf("closure = %v", res.Closure)
	}
}

func TestPackagesInseparable(t *testing.T) {
	// Three packages that always move together; the culprit is one of them.
	cands := []string{"a", "b", "c"}
	w := &world{culprits: []string{"b"}, deps: map[string][]string{"a": {"b", "c"}, "b": {"a", "c"}, "c": {"a", "b"}}}
	res, err := Packages(context.Background(), cands, w.apply, Options{Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Inseparable || !reflect.DeepEqual(res.Culprits, cands) {
		t.Errorf("%+v", res)
	}
}

func TestPackagesInteraction(t *testing.T) {
	// Needs both pkg02 and pkg13 upgraded to fail.
	cands := names(16)
	w := &world{culprits: []string{"pkg02", "pkg13"}}
	res, err := Packages(context.Background(), cands, w.apply, Options{Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Interaction {
		t.Errorf("want Interaction, got %+v", res)
	}
}

func TestPackagesSkipsFallBackToFinerSplits(t *testing.T) {
	cands := names(8)
	w := &world{culprits: []string{"pkg05"}, skipIf: func(p []string) bool { return len(p) == 4 }}
	res, err := Packages(context.Background(), cands, w.apply, Options{Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Culprits, []string{"pkg05"}) {
		t.Errorf("culprits = %v", res.Culprits)
	}
}

func TestPackagesUnresolved(t *testing.T) {
	cands := names(4)
	w := &world{culprits: []string{"pkg01"}, skipIf: func([]string) bool { return true }}
	res, err := Packages(context.Background(), cands, w.apply, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Unresolved || len(res.Culprits) != 4 {
		t.Errorf("%+v", res)
	}
}

func TestPackagesNoRedundantConfirm(t *testing.T) {
	// Two candidates: the first step tests one of them alone, so the
	// confirmation must not repeat it.
	w := &world{culprits: []string{"a"}}
	res, err := Packages(context.Background(), []string{"a", "b"}, w.apply, Options{Confirm: true})
	if err != nil || !reflect.DeepEqual(res.Culprits, []string{"a"}) {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if w.calls != 1 || !reflect.DeepEqual(res.Closure, []string{"a"}) {
		t.Errorf("calls=%d closure=%v, want 1 call and closure [a]", w.calls, res.Closure)
	}
}

func TestPackagesOneCandidate(t *testing.T) {
	w := &world{culprits: []string{"only"}}
	res, err := Packages(context.Background(), []string{"only"}, w.apply, Options{Confirm: true})
	if err != nil || !reflect.DeepEqual(res.Culprits, []string{"only"}) || w.calls != 1 {
		t.Errorf("res=%+v err=%v calls=%d", res, err, w.calls)
	}
}
