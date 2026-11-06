// tw-bisect finds the openSUSE Tumbleweed snapshot, and then the package,
// that broke something.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/officialasishkumar/tw-bisect/internal/bisect"
	"github.com/officialasishkumar/tw-bisect/internal/history"
	"github.com/officialasishkumar/tw-bisect/internal/runner"
)

var version = "dev"

const usage = `tw-bisect: git bisect for your operating system

Usage:
  tw-bisect snapshots                      list the snapshots still in the archive
  tw-bisect diff <good> <bad> [flags]      packages that changed between two snapshots
  tw-bisect run [flags] -- <command>       find the snapshot, then the package, that broke <command>
  tw-bisect shell <snapshot> [flags]       open a shell in a snapshot
  tw-bisect version

The command given to "run" is executed with bash inside each snapshot.
Exit 0 means good, 125 means this snapshot cannot be tested, anything
else means bad, as with "git bisect run".

Examples:
  tw-bisect run --good 20261013 --bad 20261105 -- 'curl --version | grep -q nghttp2/1.70'
  tw-bisect run --good 20261020 --bad 20261105 --pkgs python3,python3-requests -- python3 -c 'import requests'
  tw-bisect diff 20261104 20261105 --changelog
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tw-bisect:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "snapshots":
		return cmdSnapshots(ctx, args[1:])
	case "diff":
		return cmdDiff(ctx, args[1:])
	case "run":
		return cmdRun(ctx, args[1:])
	case "shell":
		return cmdShell(ctx, args[1:])
	case "version":
		fmt.Println("tw-bisect", version)
		return nil
	}
	return fmt.Errorf("unknown command %q (try --help)", args[0])
}

func cmdSnapshots(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("snapshots", flag.ContinueOnError)
	rt := runtimeFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c := history.NewClient()
	snaps, err := c.Snapshots(ctx)
	if err != nil {
		return err
	}
	d, _ := rt.docker()
	for _, s := range snaps {
		mark := ""
		if d != nil && d.HasImage(ctx, d.ImageTag(s, nil)) {
			mark = "  (image built)"
		}
		fmt.Printf("%s%s\n", s, mark)
	}
	fmt.Fprintf(os.Stderr, "%d snapshots in %s/history/ (the archive keeps the last twenty, x86_64 only)\n", len(snaps), c.BaseURL)
	return nil
}

func cmdDiff(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	changelog := fs.Bool("changelog", false, "print changelog excerpts from Changes.<bad>.txt")
	only := fs.String("only", "", "comma-separated package names to restrict the output to")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New("usage: tw-bisect diff <good> <bad>")
	}
	good, bad := pos[0], pos[1]
	c := history.NewClient()
	gp, err := c.Packages(ctx, good)
	if err != nil {
		return err
	}
	bp, err := c.Packages(ctx, bad)
	if err != nil {
		return err
	}
	changes := history.Diff(gp, bp)
	if *only != "" {
		keep := map[string]bool{}
		for _, n := range splitList(*only) {
			keep[n] = true
		}
		var f []history.Change
		for _, ch := range changes {
			if keep[ch.Name] {
				f = append(f, ch)
			}
		}
		changes = f
	}
	var cl *history.Changes
	if *changelog {
		if cl, err = c.Changes(ctx, bad); err != nil {
			fmt.Fprintf(os.Stderr, "no changelog for %s: %v\n", bad, err)
		}
	}
	counts := map[string]int{}
	for _, ch := range changes {
		counts[ch.Kind()]++
		fmt.Printf("%-8s %s  %s\n", ch.Kind(), ch.Name, versions(ch))
		if cl != nil {
			if d, ok := cl.ForPackage(ch.Name); ok {
				fmt.Println(indent(d, "         | "))
			}
		}
	}
	fmt.Fprintf(os.Stderr, "%d packages differ between %s and %s: %d updated, %d added, %d removed\n",
		len(changes), good, bad, counts["updated"], counts["added"], counts["removed"])
	return nil
}

func versions(ch history.Change) string {
	switch ch.Kind() {
	case "added":
		return ch.New.EVR()
	case "removed":
		return ch.Old.EVR() + " -> (gone)"
	}
	return ch.Old.EVR() + " -> " + ch.New.EVR()
}

type rtFlags struct {
	runtime, platform, base, cache *string
}

func runtimeFlags(fs *flag.FlagSet) *rtFlags {
	return &rtFlags{
		runtime:  fs.String("runtime", "", "docker or podman (default: whichever is found)"),
		platform: fs.String("platform", "", "container platform (default: linux/amd64 on non-x86 hosts)"),
		base:     fs.String("base-image", "", "image whose zypper builds snapshot roots (default: registry.opensuse.org/opensuse/tumbleweed:latest)"),
		cache:    fs.String("cache-volume", "", "named volume for downloaded packages (default: tw-bisect-zypp-cache)"),
	}
}

func (r *rtFlags) docker() (*runner.Docker, error) {
	d, err := runner.New(*r.runtime)
	if err != nil {
		return nil, err
	}
	if *r.platform != "" {
		d.Platform = *r.platform
	}
	if *r.base != "" {
		d.BaseImage = *r.base
	}
	if *r.cache != "" {
		d.CacheVol = *r.cache
	}
	return d, nil
}

// parseInterspersed accepts flags before and after positional arguments.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func splitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func cmdShell(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("shell", flag.ContinueOnError)
	rt := runtimeFlags(fs)
	pkgs := fs.String("pkgs", "", "comma-separated extra packages to install")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || !history.IsSnapshot(pos[0]) {
		return errors.New("usage: tw-bisect shell <snapshot>")
	}
	d, err := rt.docker()
	if err != nil {
		return err
	}
	d.Log = os.Stderr
	c := history.NewClient()
	tag, err := d.BuildSnapshot(ctx, pos[0], c.RepoURL(pos[0]), splitList(*pkgs))
	if err != nil {
		return err
	}
	_, _, err = d.Exec(ctx, tag, nil, "exec bash", true)
	return err
}

// Report is the machine-readable result of a run.
type Report struct {
	Good            string          `json:"good"`
	Bad             string          `json:"bad"`
	Command         string          `json:"command"`
	Packages        []string        `json:"extra_packages,omitempty"`
	LastGood        string          `json:"last_good_snapshot"`
	FirstBad        string          `json:"first_bad_snapshot"`
	SkippedSnaps    []string        `json:"skipped_snapshots,omitempty"`
	SnapshotSteps   []bisect.Step   `json:"snapshot_steps"`
	InstalledDiff   []runner.Change `json:"installed_packages_changed"`
	Culprits        []CulpritReport `json:"culprits"`
	Inseparable     bool            `json:"inseparable,omitempty"`
	Interaction     bool            `json:"interaction,omitempty"`
	Unresolved      bool            `json:"unresolved,omitempty"`
	PackageSteps    []bisect.Step   `json:"package_steps"`
	Closure         []string        `json:"closure,omitempty"`
	Elapsed         string          `json:"elapsed"`
	ChangesFileUsed string          `json:"changes_file,omitempty"`
}

// CulpritReport describes one package found responsible.
type CulpritReport struct {
	Name      string `json:"name"`
	OldEVR    string `json:"old,omitempty"`
	NewEVR    string `json:"new,omitempty"`
	Source    string `json:"source,omitempty"`
	Changelog string `json:"changelog,omitempty"`
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	rt := runtimeFlags(fs)
	good := fs.String("good", "", "a snapshot where the command succeeds (YYYYMMDD)")
	bad := fs.String("bad", "", "a snapshot where the command fails (default: newest in the archive)")
	pkgs := fs.String("pkgs", "", "comma-separated packages to install in every snapshot before testing")
	jsonOut := fs.String("json", "", "write a JSON report to this file")
	noVerify := fs.Bool("no-verify", false, "do not re-check that good passes and bad fails")
	snapOnly := fs.Bool("snapshots-only", false, "stop after finding the first bad snapshot")
	quiet := fs.Bool("quiet", false, "hide container build and test logs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cmdArgs := fs.Args()
	if len(cmdArgs) == 0 {
		return errors.New("usage: tw-bisect run --good <snapshot> [--bad <snapshot>] -- <command>")
	}
	command := strings.Join(cmdArgs, " ")
	if len(cmdArgs) > 1 {
		command = shellJoin(cmdArgs)
	}
	if !history.IsSnapshot(*good) {
		return errors.New("--good must be a snapshot id such as 20261013")
	}
	start := time.Now()
	logw := io.Writer(os.Stderr)
	if *quiet {
		logw = io.Discard
	}
	c := history.NewClient()
	snaps, err := c.Snapshots(ctx)
	if err != nil {
		return err
	}
	if *bad == "" {
		*bad = snaps[len(snaps)-1]
	}
	gi, bi := index(snaps, *good), index(snaps, *bad)
	if gi < 0 || bi < 0 {
		return fmt.Errorf("the archive only holds %s to %s (run \"tw-bisect snapshots\"); got good=%s bad=%s", snaps[0], snaps[len(snaps)-1], *good, *bad)
	}
	if gi >= bi {
		return fmt.Errorf("good snapshot %s must be older than bad snapshot %s", *good, *bad)
	}
	d, err := rt.docker()
	if err != nil {
		return err
	}
	d.Log = logw
	extra := splitList(*pkgs)
	rep := &Report{Good: *good, Bad: *bad, Command: command, Packages: extra}
	status := func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
	status("tw-bisect %s: %s is good, %s is bad, %d snapshots in between; platform %s via %s",
		version, *good, *bad, bi-gi-1, orHost(d.Platform), d.Bin)
	status("test command: %s", command)

	build := func(s string) (string, error) { return d.BuildSnapshot(ctx, s, c.RepoURL(s), extra) }
	test := func(s string) (bisect.Verdict, error) {
		tag, err := build(s)
		if err != nil {
			status("[test ] %s: cannot build, skipping (%v)", s, firstLine(err.Error()))
			return bisect.Skip, nil
		}
		v, out, err := d.Test(ctx, tag, command)
		if err != nil {
			return v, err
		}
		status("[test ] %s → %s", s, v)
		if !*quiet && v == bisect.Bad {
			fmt.Fprint(logw, indent(lastLines(out, 5), "         "))
			fmt.Fprintln(logw)
		}
		return v, nil
	}
	if !*noVerify {
		for _, want := range []struct {
			s string
			v bisect.Verdict
		}{{*good, bisect.Good}, {*bad, bisect.Bad}} {
			v, err := test(want.s)
			if err != nil {
				return err
			}
			if v != want.v {
				return fmt.Errorf("precondition failed: %s is supposed to be %s but the test says %s", want.s, want.v, v)
			}
		}
	}
	sres, err := bisect.Snapshots(ctx, snaps[gi:bi+1], func(ctx context.Context, s string) (bisect.Verdict, error) { return test(s) })
	if err != nil {
		return err
	}
	rep.LastGood, rep.FirstBad, rep.SkippedSnaps, rep.SnapshotSteps = sres.LastGood, sres.FirstBad, sres.Skipped, sres.Steps
	status("")
	status("first bad snapshot: %s (last good: %s)", sres.FirstBad, sres.LastGood)
	if len(sres.Skipped) > 0 {
		status("could not test: %s", strings.Join(sres.Skipped, ", "))
	}
	if *snapOnly {
		return finish(rep, *jsonOut, start)
	}

	goodTag, err := build(sres.LastGood)
	if err != nil {
		return err
	}
	badTag, err := build(sres.FirstBad)
	if err != nil {
		return err
	}
	gl, err := d.Installed(ctx, goodTag)
	if err != nil {
		return err
	}
	bl, err := d.Installed(ctx, badTag)
	if err != nil {
		return err
	}
	rep.InstalledDiff = installedDiff(gl, bl)
	var candidates []string
	for _, ch := range rep.InstalledDiff {
		if ch.NewEVR != "" {
			candidates = append(candidates, ch.Name)
		}
	}
	status("%d of %d installed packages differ between %s and %s", len(rep.InstalledDiff), len(bl), sres.LastGood, sres.FirstBad)
	if len(candidates) == 0 {
		status("nothing to bisect: the installed package set did not change. The difference is outside the packages in this image; add the ones your test needs with --pkgs.")
		return finish(rep, *jsonOut, start)
	}
	step := 0
	apply := func(ctx context.Context, pkgs []string) ([]string, bisect.Verdict, error) {
		step++
		status("[pkgs ] step %d: upgrading %d of %d candidates from %s (%s)", step, len(pkgs), len(candidates), sres.FirstBad, summarize(pkgs))
		changes, v, out, err := d.ApplyAndTest(ctx, goodTag, c.RepoURL(sres.FirstBad), pkgs, command)
		if err != nil {
			return nil, v, err
		}
		var names []string
		for _, ch := range changes {
			names = append(names, ch.Name)
		}
		if v != bisect.Skip {
			status("[pkgs ]   %d packages changed → %s", len(changes), v)
		}
		if !*quiet && v == bisect.Bad {
			fmt.Fprint(logw, indent(lastLines(afterTest(out), 5), "         "))
			fmt.Fprintln(logw)
		}
		return names, v, nil
	}
	pres, err := bisect.Packages(ctx, candidates, apply, bisect.Options{Confirm: true})
	if err != nil {
		return err
	}
	rep.PackageSteps, rep.Inseparable, rep.Interaction, rep.Unresolved, rep.Closure = pres.Steps, pres.Inseparable, pres.Interaction, pres.Unresolved, pres.Closure
	byName := map[string]runner.Change{}
	for _, ch := range rep.InstalledDiff {
		byName[ch.Name] = ch
	}
	cl, clErr := c.Changes(ctx, sres.FirstBad)
	if clErr == nil {
		rep.ChangesFileUsed = c.BaseURL + "/tumbleweed/iso/Changes." + sres.FirstBad + ".txt"
	}
	for _, name := range pres.Culprits {
		cr := CulpritReport{Name: name, OldEVR: byName[name].OldEVR, NewEVR: byName[name].NewEVR, Source: byName[name].Source}
		if cl != nil {
			if text, ok := cl.ForPackage(name, cr.Source); ok {
				cr.Changelog = text
			}
		}
		rep.Culprits = append(rep.Culprits, cr)
	}
	status("")
	printVerdict(os.Stdout, rep)
	return finish(rep, *jsonOut, start)
}

func printVerdict(w io.Writer, rep *Report) {
	switch {
	case rep.Unresolved:
		fmt.Fprintf(w, "could not narrow it down: no subset of the %d changed packages could be installed on its own.\n", len(rep.Culprits))
	case rep.Interaction:
		fmt.Fprintf(w, "no single package reproduces it; the failure needs more than one of these %d upgrades together:\n", len(rep.Culprits))
	case rep.Inseparable:
		fmt.Fprintf(w, "these %d packages only upgrade together, and together they break the test:\n", len(rep.Culprits))
	case len(rep.Culprits) == 1:
		fmt.Fprintln(w, "first bad package:")
	default:
		fmt.Fprintln(w, "first bad packages:")
	}
	for _, cr := range rep.Culprits {
		if cr.OldEVR == "" {
			fmt.Fprintf(w, "  %s  (new in %s) %s\n", cr.Name, rep.FirstBad, cr.NewEVR)
		} else {
			fmt.Fprintf(w, "  %s  %s -> %s\n", cr.Name, cr.OldEVR, cr.NewEVR)
		}
		if cr.Changelog != "" {
			fmt.Fprintf(w, "%s\n", indent(trimLines(cr.Changelog, 12), "    | "))
		}
	}
	if extra := minusAll(rep.Closure, culpritNames(rep)); len(extra) > 0 {
		fmt.Fprintf(w, "  pulled in with them: %s\n", strings.Join(extra, ", "))
	}
	fmt.Fprintf(w, "\nsnapshot range: %s (good) -> %s (bad); repositories: https://download.opensuse.org/history/%s/\n", rep.LastGood, rep.FirstBad, rep.FirstBad)
	if rep.ChangesFileUsed != "" {
		fmt.Fprintf(w, "changelog: %s\n", rep.ChangesFileUsed)
	}
}

func finish(rep *Report, jsonPath string, start time.Time) error {
	rep.Elapsed = time.Since(start).Round(time.Second).String()
	fmt.Fprintf(os.Stderr, "done in %s\n", rep.Elapsed)
	if jsonPath == "" {
		return nil
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(jsonPath, append(b, '\n'), 0o644)
}

func installedDiff(old, new map[string]runner.Installed) []runner.Change {
	var out []runner.Change
	for name, np := range new {
		op, had := old[name]
		if !had {
			out = append(out, runner.Change{Name: name, NewEVR: np.EVR, Source: np.Source})
		} else if op.EVR != np.EVR {
			out = append(out, runner.Change{Name: name, OldEVR: op.EVR, NewEVR: np.EVR, Source: np.Source})
		}
	}
	for name, op := range old {
		if _, still := new[name]; !still {
			out = append(out, runner.Change{Name: name, OldEVR: op.EVR, Source: op.Source})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func culpritNames(rep *Report) []string {
	var out []string
	for _, c := range rep.Culprits {
		out = append(out, c.Name)
	}
	return out
}

func minusAll(a, b []string) []string {
	in := map[string]bool{}
	for _, s := range b {
		in[s] = true
	}
	var out []string
	for _, s := range a {
		if !in[s] {
			out = append(out, s)
		}
	}
	return out
}

func index(items []string, s string) int {
	for i, it := range items {
		if it == s {
			return i
		}
	}
	return -1
}

func orHost(p string) string {
	if p == "" {
		return "host"
	}
	return p
}

func summarize(pkgs []string) string {
	if len(pkgs) <= 4 {
		return strings.Join(pkgs, ", ")
	}
	return strings.Join(pkgs[:3], ", ") + fmt.Sprintf(", … +%d", len(pkgs)-3)
}

func shellJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\$`|&;<>()*?[]{}~#") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		q[i] = a
	}
	return strings.Join(q, " ")
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func trimLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = append(lines[:n], fmt.Sprintf("… (%d more lines)", len(lines)-n))
	}
	return strings.Join(lines, "\n")
}

func lastLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	lines := strings.Split(s, "\n")
	var keep []string
	for _, l := range lines {
		if !strings.HasPrefix(l, "TWBISECT-") {
			keep = append(keep, l)
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return strings.Join(keep, "\n")
}

func afterTest(out string) string {
	if _, after, ok := strings.Cut(out, "TWBISECT-TEST\n"); ok {
		return after
	}
	return out
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}
