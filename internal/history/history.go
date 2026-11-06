// Package history reads the public openSUSE Tumbleweed snapshot archive at
// https://download.opensuse.org/history/ and the per-snapshot change logs at
// https://download.opensuse.org/tumbleweed/iso/Changes.<snapshot>.txt.
//
// The archive keeps the last twenty snapshots (x86_64 only). Every snapshot
// directory holds a full package repository plus an rpm.list file naming
// every RPM in the snapshot, which is enough to diff two snapshots without
// downloading repository metadata.
package history

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// DefaultBaseURL is the openSUSE download redirector.
const DefaultBaseURL = "https://download.opensuse.org"

// Client fetches snapshot data, caching file bodies on disk.
type Client struct {
	BaseURL  string
	HTTP     *http.Client
	CacheDir string // "" disables caching
}

// NewClient returns a client with a 2-minute timeout and the XDG cache dir.
func NewClient() *Client {
	cache, _ := os.UserCacheDir()
	if cache != "" {
		cache = filepath.Join(cache, "tw-bisect")
	}
	return &Client{BaseURL: DefaultBaseURL, HTTP: &http.Client{Timeout: 2 * time.Minute}, CacheDir: cache}
}

var snapRe = regexp.MustCompile(`^\d{8}$`)

// IsSnapshot reports whether s looks like a snapshot id (YYYYMMDD).
func IsSnapshot(s string) bool { return snapRe.MatchString(s) }

// Snapshots returns the snapshot ids kept in the archive, oldest first.
func (c *Client) Snapshots(ctx context.Context) ([]string, error) {
	body, err := c.get(ctx, "/history/list", "list", false)
	if err != nil {
		return nil, err
	}
	var out []string
	for l := range strings.FieldsSeq(string(body)) {
		if IsSnapshot(l) {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("history list is empty")
	}
	sort.Strings(out)
	return out, nil
}

// RepoURL is the OSS repository of a snapshot, usable with zypper.
func (c *Client) RepoURL(snapshot string) string {
	return c.BaseURL + "/history/" + snapshot + "/tumbleweed/repo/oss/"
}

// Pkg is one binary RPM in a snapshot.
type Pkg struct {
	Name    string
	Version string
	Release string
	Arch    string
}

// EVR is the version-release string as rpm prints %{EVR} for epoch-less packages.
func (p Pkg) EVR() string { return p.Version + "-" + p.Release }

// ParseRPMFilename splits "name-version-release.arch.rpm". Package names may
// contain dashes, so the split works from the right. ok is false for source
// RPMs and for names that do not follow the pattern.
func ParseRPMFilename(path string) (p Pkg, ok bool) {
	base := path[strings.LastIndex(path, "/")+1:]
	if !strings.HasSuffix(base, ".rpm") {
		return p, false
	}
	base = strings.TrimSuffix(base, ".rpm")
	i := strings.LastIndex(base, ".")
	if i < 0 {
		return p, false
	}
	p.Arch, base = base[i+1:], base[:i]
	if p.Arch == "src" || p.Arch == "nosrc" {
		return p, false
	}
	j := strings.LastIndex(base, "-")
	if j <= 0 {
		return p, false
	}
	p.Release, base = base[j+1:], base[:j]
	k := strings.LastIndex(base, "-")
	if k <= 0 {
		return p, false
	}
	p.Version, p.Name = base[k+1:], base[:k]
	return p, true
}

// Packages returns the binary packages of a snapshot keyed by name, from its
// rpm.list. Debuginfo repositories and 32-bit i586/i686 builds are skipped,
// so each name maps to one package.
func (c *Client) Packages(ctx context.Context, snapshot string) (map[string]Pkg, error) {
	if !IsSnapshot(snapshot) {
		return nil, fmt.Errorf("%q is not a snapshot id", snapshot)
	}
	body, err := c.get(ctx, "/history/"+snapshot+"/rpm.list", "rpm.list."+snapshot, true)
	if err != nil {
		return nil, err
	}
	return ParseRPMList(string(body)), nil
}

// ParseRPMList parses the rpm.list format: one repository-relative RPM path per line.
func ParseRPMList(text string) map[string]Pkg {
	out := map[string]Pkg{}
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "/debug/") {
			continue
		}
		p, ok := ParseRPMFilename(line)
		if !ok || p.Arch == "i586" || p.Arch == "i686" {
			continue
		}
		out[p.Name] = p
	}
	return out
}

// Change is one package that differs between two snapshots. Old is zero for
// packages new in the later snapshot; New is zero for packages that were dropped.
type Change struct {
	Name string
	Old  Pkg
	New  Pkg
}

// Kind describes the change in one word.
func (ch Change) Kind() string {
	switch {
	case ch.Old.Name == "":
		return "added"
	case ch.New.Name == "":
		return "removed"
	default:
		return "updated"
	}
}

// Diff lists packages that differ between two snapshots, sorted by name.
func Diff(old, new map[string]Pkg) []Change {
	var out []Change
	for name, np := range new {
		op, had := old[name]
		if !had {
			out = append(out, Change{Name: name, New: np})
		} else if op.EVR() != np.EVR() {
			out = append(out, Change{Name: name, Old: op, New: np})
		}
	}
	for name, op := range old {
		if _, still := new[name]; !still {
			out = append(out, Change{Name: name, Old: op})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Changes is the content of a Changes.<snapshot>.txt file.
type Changes struct {
	Snapshot string
	// Details maps a source package name to its changelog excerpt.
	Details map[string]string
	// Order keeps the source packages in file order.
	Order []string
}

// Changes fetches and parses the change log published with a snapshot.
func (c *Client) Changes(ctx context.Context, snapshot string) (*Changes, error) {
	if !IsSnapshot(snapshot) {
		return nil, fmt.Errorf("%q is not a snapshot id", snapshot)
	}
	body, err := c.get(ctx, "/tumbleweed/iso/Changes."+snapshot+".txt", "Changes."+snapshot+".txt", true)
	if err != nil {
		return nil, err
	}
	ch := ParseChanges(string(body))
	ch.Snapshot = snapshot
	return ch, nil
}

var detailHead = regexp.MustCompile(`^==== (\S+) ====\s*$`)

// ParseChanges reads the "=== Details ===" section of a Changes file.
func ParseChanges(text string) *Changes {
	ch := &Changes{Details: map[string]string{}}
	cur := ""
	var buf strings.Builder
	flush := func() {
		if cur != "" {
			ch.Details[cur] = strings.TrimSpace(buf.String())
			ch.Order = append(ch.Order, cur)
		}
		buf.Reset()
	}
	for line := range strings.Lines(text) {
		line = strings.TrimRight(line, "\r\n")
		if m := detailHead.FindStringSubmatch(line); m != nil {
			flush()
			cur = m[1]
			continue
		}
		if cur != "" {
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
	}
	flush()
	return ch
}

// ForPackage finds the changelog for a binary package. Changes files are
// keyed by source package, so callers should pass the source name when they
// know it (rpm -q --qf %{SOURCERPM}). Without it, common openSUSE naming
// rules are tried: libnghttp2-14 -> nghttp2, python313-tornado6 -> python-tornado6.
func (ch *Changes) ForPackage(binary string, sources ...string) (string, bool) {
	for _, s := range append([]string{binary}, sources...) {
		if d, ok := ch.Details[s]; ok {
			return d, true
		}
	}
	for _, cand := range sourceGuesses(binary) {
		if d, ok := ch.Details[cand]; ok {
			return d, true
		}
	}
	return "", false
}

var (
	sonameRe = regexp.MustCompile(`[-_]?\d+(_\d+)*$`)
	pythonRe = regexp.MustCompile(`^python3\d*-`)
)

// sourceGuesses lists plausible source package names for a binary package,
// most specific first.
func sourceGuesses(binary string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, b := range []string{binary, pythonRe.ReplaceAllString(binary, "python-")} {
		add(b)
		add(sonameRe.ReplaceAllString(b, ""))
		if strings.HasPrefix(b, "lib") {
			add(strings.TrimPrefix(b, "lib"))
			add(sonameRe.ReplaceAllString(strings.TrimPrefix(b, "lib"), ""))
		}
		for _, suf := range []string{"-devel", "-doc", "-lang", "-bash-completion", "-tools"} {
			add(strings.TrimSuffix(b, suf))
		}
	}
	return out
}

func (c *Client) get(ctx context.Context, path, cacheName string, cache bool) ([]byte, error) {
	var cf string
	if cache && c.CacheDir != "" {
		cf = filepath.Join(c.CacheDir, cacheName)
		if b, err := os.ReadFile(cf); err == nil && len(b) > 0 {
			return b, nil
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tw-bisect (+https://github.com/officialasishkumar/tw-bisect)")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if cf != "" {
		if err := os.MkdirAll(c.CacheDir, 0o755); err == nil {
			_ = os.WriteFile(cf, b, 0o644)
		}
	}
	return b, nil
}
