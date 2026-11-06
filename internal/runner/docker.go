// Package runner builds a root filesystem for a Tumbleweed snapshot from its
// history repository and runs test commands in it, using docker or podman.
//
// Every snapshot becomes an image: zypper installs the requested packages
// into an empty directory from the snapshot's own repository, and that
// directory is copied into a FROM scratch image. Each test then runs in a
// throwaway container, so the package-level search gets a pristine copy of
// the good system for free.
package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/officialasishkumar/tw-bisect/internal/bisect"
)

// BasePackages is what every snapshot image gets: enough to run a shell,
// query rpm and install more packages from a history repository.
var BasePackages = []string{"bash", "coreutils", "grep", "rpm", "zypper", "openSUSE-release", "ca-certificates-mozilla"}

// Docker drives a docker-compatible CLI.
type Docker struct {
	Bin       string    // docker or podman
	Platform  string    // e.g. linux/amd64; empty uses the host platform
	BaseImage string    // image whose zypper builds the snapshot roots
	CacheVol  string    // named volume mounted at /var/cache/zypp in test containers
	Log       io.Writer // progress and command output when non-nil
}

// New picks docker or podman, and amd64 emulation on other hosts because the
// snapshot archive only holds x86_64 packages.
func New(bin string) (*Docker, error) {
	if bin == "" {
		for _, c := range []string{"docker", "podman"} {
			if _, err := exec.LookPath(c); err == nil {
				bin = c
				break
			}
		}
	}
	if bin == "" {
		return nil, errors.New("neither docker nor podman found in PATH")
	}
	d := &Docker{Bin: bin, BaseImage: "registry.opensuse.org/opensuse/tumbleweed:latest", CacheVol: "tw-bisect-zypp-cache"}
	if runtime.GOARCH != "amd64" {
		d.Platform = "linux/amd64"
	}
	return d, nil
}

func (d *Docker) logf(format string, args ...any) {
	if d.Log != nil {
		fmt.Fprintf(d.Log, format, args...)
	}
}

// ImageTag names the image for a snapshot with a given extra package set.
func (d *Docker) ImageTag(snapshot string, pkgs []string) string {
	sorted := append([]string(nil), pkgs...)
	sort.Strings(sorted)
	h := sha256.Sum256([]byte(strings.Join(sorted, " ")))
	return "tw-bisect/snap:" + snapshot + "-" + hex.EncodeToString(h[:])[:8]
}

// HasImage reports whether the tag exists locally.
func (d *Docker) HasImage(ctx context.Context, tag string) bool {
	return exec.CommandContext(ctx, d.Bin, "image", "inspect", tag).Run() == nil
}

const dockerfile = `# syntax=docker/dockerfile:1
FROM %s AS build
ARG REPO
ARG PKGS
RUN --mount=type=cache,target=/snap/var/cache/zypp,sharing=locked \
    set -e; \
    zypper -n --root /snap addrepo --keep-packages --refresh "$REPO" tw-oss; \
    zypper -n --root /snap --gpg-auto-import-keys refresh; \
    zypper -n --root /snap install --no-recommends $PKGS
FROM scratch
COPY --from=build /snap /
ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
LABEL org.opensuse.tumbleweed.snapshot=%s
CMD ["/bin/bash"]
`

// BuildSnapshot builds (or reuses) the image for snapshot with BasePackages
// plus extra, installed from repoURL. It returns the image tag.
func (d *Docker) BuildSnapshot(ctx context.Context, snapshot, repoURL string, extra []string) (string, error) {
	tag := d.ImageTag(snapshot, extra)
	if d.HasImage(ctx, tag) {
		d.logf("[build] %s: image cached\n", snapshot)
		return tag, nil
	}
	pkgs := append(append([]string(nil), BasePackages...), extra...)
	dir, err := os.MkdirTemp("", "tw-bisect-ctx-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	args := []string{"build", "--build-arg", "REPO=" + repoURL, "--build-arg", "PKGS=" + strings.Join(pkgs, " "), "-t", tag, "-f", "-"}
	if d.Platform != "" {
		args = append(args, "--platform", d.Platform)
	}
	args = append(args, dir)
	start := time.Now()
	d.logf("[build] %s: installing %d packages from %s\n", snapshot, len(pkgs), repoURL)
	cmd := exec.CommandContext(ctx, d.Bin, args...)
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	cmd.Stdin = strings.NewReader(fmt.Sprintf(dockerfile, d.BaseImage, snapshot))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("building %s: %w\n%s", snapshot, err, tail(out, 40))
	}
	d.logf("[build] %s: done in %s\n", snapshot, time.Since(start).Round(time.Second))
	return tag, nil
}

// Exec runs script with bash inside a fresh container of tag and returns
// the combined output and exit code. A non-zero exit is not an error.
func (d *Docker) Exec(ctx context.Context, tag string, env map[string]string, script string, interactive bool) (int, string, error) {
	args := []string{"run", "--rm"}
	if interactive {
		args = append(args, "-it")
	}
	if d.Platform != "" {
		args = append(args, "--platform", d.Platform)
	}
	if d.CacheVol != "" {
		args = append(args, "-v", d.CacheVol+":/var/cache/zypp")
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+env[k])
	}
	args = append(args, tag, "bash", "-c", script)
	cmd := exec.CommandContext(ctx, d.Bin, args...)
	if interactive {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		err := cmd.Run()
		return exitCode(err), "", nil
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if _, isExit := errors.AsType[*exec.ExitError](err); !isExit {
			return -1, string(out), err
		}
	}
	return exitCode(err), string(out), nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode()
	}
	return -1
}

const testScript = `bash -c "$TW_CMD"; rc=$?; echo "TWBISECT-EXIT $rc"`

var exitRe = regexp.MustCompile(`(?m)^TWBISECT-EXIT (\d+)\s*$`)

// Test runs the user's command in a snapshot image and maps its exit status
// the way git bisect run does: 0 good, 125 skip, 1-127 bad, 128+ abort.
func (d *Docker) Test(ctx context.Context, tag, command string) (bisect.Verdict, string, error) {
	_, out, err := d.Exec(ctx, tag, map[string]string{"TW_CMD": command}, testScript, false)
	if err != nil {
		return bisect.Skip, out, err
	}
	return verdictFrom(out)
}

func verdictFrom(out string) (bisect.Verdict, string, error) {
	m := exitRe.FindStringSubmatch(out)
	if m == nil {
		return bisect.Skip, out, fmt.Errorf("container did not run the test:\n%s", tail([]byte(out), 20))
	}
	rc, _ := strconv.Atoi(m[1])
	switch {
	case rc == 0:
		return bisect.Good, out, nil
	case rc == 125:
		return bisect.Skip, out, nil
	case rc >= 128:
		return bisect.Skip, out, fmt.Errorf("test command exited with %d, which aborts the bisect", rc)
	default:
		return bisect.Bad, out, nil
	}
}

// Installed is one package in a container's rpm database.
type Installed struct {
	Name   string
	EVR    string
	Source string // source package name, from %{SOURCERPM}
}

const listScript = `rpm -qa --qf '%{NAME}\t%{EVR}\t%{SOURCERPM}\n' | sort`

// Installed lists the packages in a snapshot image.
func (d *Docker) Installed(ctx context.Context, tag string) (map[string]Installed, error) {
	rc, out, err := d.Exec(ctx, tag, nil, listScript, false)
	if err != nil {
		return nil, err
	}
	if rc != 0 {
		return nil, fmt.Errorf("rpm -qa failed (%d):\n%s", rc, tail([]byte(out), 10))
	}
	return parseInstalled(out), nil
}

var srcRe = regexp.MustCompile(`^(.*)-[^-]+-[^-]+\.(?:no)?src\.rpm$`)

func parseInstalled(out string) map[string]Installed {
	res := map[string]Installed{}
	for line := range strings.Lines(out) {
		f := strings.Split(strings.TrimRight(line, "\n"), "\t")
		if len(f) < 2 || f[0] == "" {
			continue
		}
		p := Installed{Name: f[0], EVR: f[1]}
		if len(f) > 2 {
			if m := srcRe.FindStringSubmatch(f[2]); m != nil {
				p.Source = m[1]
			}
		}
		res[p.Name] = p
	}
	return res
}

const applyScript = `set -u
zypper -n addrepo --keep-packages --refresh "$TW_BADREPO" tw-bad >/dev/null 2>&1 || { echo "TWBISECT-SKIP addrepo failed"; exit 0; }
zypper -n --gpg-auto-import-keys refresh tw-bad >/dev/null 2>&1 || { echo "TWBISECT-SKIP refresh failed"; exit 0; }
rpm -qa --qf '%{NAME}\t%{EVR}\t%{SOURCERPM}\n' | sort > /tmp/tw-before
zypper -n install --no-recommends --from tw-bad $TW_PKGS > /tmp/tw-zypper.log 2>&1
zrc=$?
case $zrc in 0|102|103|107) ;; *) echo "TWBISECT-SKIP zypper install exited $zrc"; tail -20 /tmp/tw-zypper.log; exit 0;; esac
rpm -qa --qf '%{NAME}\t%{EVR}\t%{SOURCERPM}\n' | sort > /tmp/tw-after
echo TWBISECT-BEFORE; cat /tmp/tw-before; echo TWBISECT-AFTER; cat /tmp/tw-after; echo TWBISECT-TEST
bash -c "$TW_CMD"; rc=$?; echo "TWBISECT-EXIT $rc"`

// Change is one package that an apply step altered.
type Change struct {
	Name, OldEVR, NewEVR, Source string
}

// ApplyAndTest starts from the good snapshot image, upgrades pkgs (and
// whatever the solver requires) from badRepoURL, runs the command, and
// reports what actually changed. A failed install is a Skip, not an error.
func (d *Docker) ApplyAndTest(ctx context.Context, goodTag, badRepoURL string, pkgs []string, command string) ([]Change, bisect.Verdict, string, error) {
	env := map[string]string{"TW_BADREPO": badRepoURL, "TW_PKGS": strings.Join(pkgs, " "), "TW_CMD": command}
	_, out, err := d.Exec(ctx, goodTag, env, applyScript, false)
	if err != nil {
		return nil, bisect.Skip, out, err
	}
	prefix := out
	if t := strings.Index(out, "TWBISECT-TEST\n"); t >= 0 {
		prefix = out[:t]
	}
	if i := strings.Index(prefix, "TWBISECT-SKIP"); i >= 0 {
		d.logf("[apply] skipped: %s\n", strings.TrimSpace(firstLine(prefix[i:])))
		return nil, bisect.Skip, out, nil
	}
	changes, rest, err := parseApply(out)
	if err != nil {
		return nil, bisect.Skip, out, err
	}
	v, _, err := verdictFrom(rest)
	return changes, v, out, err
}

func parseApply(out string) ([]Change, string, error) {
	b := strings.Index(out, "TWBISECT-BEFORE\n")
	a := strings.Index(out, "TWBISECT-AFTER\n")
	t := strings.Index(out, "TWBISECT-TEST\n")
	if b < 0 || a < b || t < a {
		return nil, out, fmt.Errorf("could not parse apply output:\n%s", tail([]byte(out), 20))
	}
	before := parseInstalled(out[b+len("TWBISECT-BEFORE\n") : a])
	after := parseInstalled(out[a+len("TWBISECT-AFTER\n") : t])
	var changes []Change
	for name, np := range after {
		op, had := before[name]
		if !had {
			changes = append(changes, Change{Name: name, NewEVR: np.EVR, Source: np.Source})
		} else if op.EVR != np.EVR {
			changes = append(changes, Change{Name: name, OldEVR: op.EVR, NewEVR: np.EVR, Source: np.Source})
		}
	}
	for name, op := range before {
		if _, still := after[name]; !still {
			changes = append(changes, Change{Name: name, OldEVR: op.EVR, Source: op.Source})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Name < changes[j].Name })
	return changes, out[t:], nil
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

func tail(b []byte, n int) string {
	lines := bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return string(bytes.Join(lines, []byte("\n")))
}
