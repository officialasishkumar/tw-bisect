package history

import (
	"strings"
	"testing"
)

func TestParseRPMFilename(t *testing.T) {
	cases := []struct {
		in   string
		want Pkg
		ok   bool
	}{
		{"tumbleweed/repo/oss/x86_64/libnghttp2-14-1.71.0-1.1.x86_64.rpm", Pkg{"libnghttp2-14", "1.71.0", "1.1", "x86_64"}, true},
		{"tumbleweed/repo/oss/noarch/agama-cli-bash-completion-25+475.275dd4727-59.1.noarch.rpm", Pkg{"agama-cli-bash-completion", "25+475.275dd4727", "59.1", "noarch"}, true},
		{"tumbleweed/repo/oss/x86_64/ipxe-2.0.0+git20260914.7cd92e01d-1.1.x86_64.rpm", Pkg{"ipxe", "2.0.0+git20260914.7cd92e01d", "1.1", "x86_64"}, true},
		{"tumbleweed/repo/oss/x86_64/glibc-32bit-2.44-3.1.x86_64.rpm", Pkg{"glibc-32bit", "2.44", "3.1", "x86_64"}, true},
		{"tumbleweed/repo/src-oss/src/nghttp2-1.71.0-1.1.src.rpm", Pkg{}, false},
		{"tumbleweed/repo/oss/x86_64/README", Pkg{}, false},
		{"bogus.rpm", Pkg{}, false},
	}
	for _, c := range cases {
		got, ok := ParseRPMFilename(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseRPMFilename(%q) = %+v,%v want %+v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseRPMListAndDiff(t *testing.T) {
	oldList := `
tumbleweed/repo/oss/x86_64/curl-8.23.0-1.1.x86_64.rpm
tumbleweed/repo/oss/x86_64/libnghttp2-14-1.70.0-1.1.x86_64.rpm
tumbleweed/repo/oss/x86_64/old-thing-1-1.1.x86_64.rpm
tumbleweed/repo/debug/x86_64/curl-debuginfo-8.23.0-1.1.x86_64.rpm
tumbleweed/repo/oss/i586/curl-8.23.0-1.1.i586.rpm
`
	newList := `
tumbleweed/repo/oss/x86_64/curl-8.23.0-1.1.x86_64.rpm
tumbleweed/repo/oss/x86_64/libnghttp2-14-1.71.0-1.1.x86_64.rpm
tumbleweed/repo/oss/noarch/new-thing-2-1.1.noarch.rpm
`
	o := ParseRPMList(oldList)
	n := ParseRPMList(newList)
	if len(o) != 3 {
		t.Fatalf("old: want 3 packages (debug and i586 skipped), got %d: %v", len(o), o)
	}
	d := Diff(o, n)
	want := map[string]string{"libnghttp2-14": "updated", "new-thing": "added", "old-thing": "removed"}
	if len(d) != len(want) {
		t.Fatalf("diff = %+v", d)
	}
	for _, ch := range d {
		if want[ch.Name] != ch.Kind() {
			t.Errorf("%s: kind %s, want %s", ch.Name, ch.Kind(), want[ch.Name])
		}
	}
	if d[0].Name != "libnghttp2-14" || d[0].Old.EVR() != "1.70.0-1.1" || d[0].New.EVR() != "1.71.0-1.1" {
		t.Errorf("first change = %+v", d[0])
	}
}

func TestParseChanges(t *testing.T) {
	in := `Packages changed:
  nghttp2 (1.70.0 -> 1.71.0)
  rsync (3.5.1 -> 3.5.2)

=== Details ===

==== nghttp2 ====
Version update (1.70.0 -> 1.71.0)

- update to 1.71.0:
  * fix a thing

==== rsync ====
Version update (3.5.1 -> 3.5.2)

- Update to 3.5.2
`
	ch := ParseChanges(in)
	if len(ch.Order) != 2 || ch.Order[0] != "nghttp2" || ch.Order[1] != "rsync" {
		t.Fatalf("order = %v", ch.Order)
	}
	if !strings.Contains(ch.Details["rsync"], "Update to 3.5.2") {
		t.Errorf("rsync details = %q", ch.Details["rsync"])
	}
	if d, ok := ch.ForPackage("libnghttp2-14"); !ok || !strings.Contains(d, "1.71.0") {
		t.Errorf("ForPackage(libnghttp2-14) = %q, %v", d, ok)
	}
	if d, ok := ch.ForPackage("rsync"); !ok || !strings.Contains(d, "3.5.2") {
		t.Errorf("ForPackage(rsync) = %q, %v", d, ok)
	}
	if _, ok := ch.ForPackage("zlib"); ok {
		t.Error("ForPackage(zlib) should not match")
	}
	if d, ok := ch.ForPackage("libwhatever1", "rsync"); !ok || !strings.Contains(d, "3.5.2") {
		t.Errorf("explicit source name should win: %q, %v", d, ok)
	}
}

func TestSourceGuesses(t *testing.T) {
	has := func(bin, want string) {
		t.Helper()
		for _, g := range sourceGuesses(bin) {
			if g == want {
				return
			}
		}
		t.Errorf("sourceGuesses(%q) = %v, missing %q", bin, sourceGuesses(bin), want)
	}
	has("libnghttp2-14", "nghttp2")
	has("python313-tornado6", "python-tornado6")
	has("libstorage-ng1", "libstorage-ng")
	has("glibc-locale-base", "glibc-locale-base")
	has("qpdf-devel", "qpdf")
}
