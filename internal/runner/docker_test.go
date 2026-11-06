package runner

import (
	"reflect"
	"testing"

	"github.com/officialasishkumar/tw-bisect/internal/bisect"
)

func TestParseInstalled(t *testing.T) {
	out := "curl\t8.23.0-1.1\tcurl-8.23.0-1.1.src.rpm\nlibnghttp2-14\t1.70.0-1.1\tnghttp2-1.70.0-1.1.src.rpm\nkernel-firmware\t20261001-1.1\tkernel-firmware-20261001-1.1.nosrc.rpm\n"
	got := parseInstalled(out)
	want := map[string]Installed{
		"curl":            {"curl", "8.23.0-1.1", "curl"},
		"libnghttp2-14":   {"libnghttp2-14", "1.70.0-1.1", "nghttp2"},
		"kernel-firmware": {"kernel-firmware", "20261001-1.1", "kernel-firmware"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

func TestParseApplyAndVerdict(t *testing.T) {
	out := "TWBISECT-BEFORE\n" +
		"a\t1-1\ta-1-1.src.rpm\nb\t1-1\tb-1-1.src.rpm\ngone\t1-1\tgone-1-1.src.rpm\n" +
		"TWBISECT-AFTER\n" +
		"a\t2-1\ta-2-1.src.rpm\nb\t1-1\tb-1-1.src.rpm\nnew\t3-1\tnew-3-1.src.rpm\n" +
		"TWBISECT-TEST\nsome test output\nTWBISECT-EXIT 1\n"
	changes, rest, err := parseApply(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []Change{{"a", "1-1", "2-1", "a"}, {"gone", "1-1", "", "gone"}, {"new", "", "3-1", "new"}}
	if !reflect.DeepEqual(changes, want) {
		t.Errorf("changes = %+v", changes)
	}
	v, _, err := verdictFrom(rest)
	if err != nil || v != bisect.Bad {
		t.Errorf("verdict = %v, %v", v, err)
	}
	for rc, want := range map[string]bisect.Verdict{"0": bisect.Good, "125": bisect.Skip, "2": bisect.Bad, "127": bisect.Bad} {
		v, _, err := verdictFrom("x\nTWBISECT-EXIT " + rc + "\n")
		if err != nil || v != want {
			t.Errorf("exit %s: %v, %v", rc, v, err)
		}
	}
	if _, _, err := verdictFrom("TWBISECT-EXIT 130\n"); err == nil {
		t.Error("exit 130 should abort")
	}
	if _, _, err := verdictFrom("no marker"); err == nil {
		t.Error("missing marker should be an error")
	}
}

func TestImageTagStable(t *testing.T) {
	d := &Docker{}
	if d.ImageTag("20261105", []string{"b", "a"}) != d.ImageTag("20261105", []string{"a", "b"}) {
		t.Error("tag should not depend on package order")
	}
	if d.ImageTag("20261105", nil) == d.ImageTag("20261105", []string{"a"}) {
		t.Error("tag should depend on the package set")
	}
}
