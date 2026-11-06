# tw-bisect

`git bisect` for openSUSE Tumbleweed: give it a command that used to work and
a command that fails now, and it finds the snapshot that broke it, then the
package inside that snapshot.

```
$ tw-bisect run --good 20261101 --bad 20261105 --pkgs curl -- 'curl --version | grep -q nghttp2/1.70'
tw-bisect: 20261101 is good, 20261105 is bad, 3 snapshots in between; platform linux/amd64 via docker
[build] 20261101: installing 8 packages from https://download.opensuse.org/history/20261101/tumbleweed/repo/oss/
[test ] 20261101 → good
[test ] 20261105 → bad
[test ] 20261103 → good
[test ] 20261104 → good

first bad snapshot: 20261105 (last good: 20261104)
5 of 83 installed packages differ between 20261104 and 20261105
[pkgs ] step 1: upgrading 2 of 5 candidates from 20261105 (libnghttp2-14, libnghttp3-9)
[pkgs ]   2 packages changed → bad
[pkgs ] step 2: upgrading 1 of 5 candidates from 20261105 (libnghttp2-14)
[pkgs ]   1 packages changed → bad

first bad package:
  libnghttp2-14  1.70.0-1.1 -> 1.71.0-1.1
    | Version update (1.70.0 -> 1.71.0)
    |
    | - Update to 1.71.0:
    |   * lib: add length check for CONTINUATION frames
    |   * lib: disallow HEADERS and PUSH_PROMISE in CONNECT streams
    |   ...
snapshot range: 20261104 (good) -> 20261105 (bad); repositories: https://download.opensuse.org/history/20261105/
changelog: https://download.opensuse.org/tumbleweed/iso/Changes.20261105.txt
done in 6m58s
```

(That run was on an Apple Silicon laptop, so every container ran under
x86_64 emulation. The test here is deliberately trivial; the point is the
mechanism, which is the same for a failing test suite.)

Tumbleweed publishes a new snapshot most days, and each one changes dozens to
hundreds of packages. When something breaks after `zypper dup`, you know the
day, not the package. tw-bisect turns the day into a package name and a
changelog entry.

## How it works

1. **Snapshots become containers.** The last twenty snapshots are kept, with
   their full repositories, at <https://download.opensuse.org/history/>.
   For each snapshot that needs testing, tw-bisect installs a small base system
   plus the packages you name into an empty directory using that snapshot's
   own repository (`zypper --root`), and turns the directory into an image.
   Builds are cached, and so are downloaded RPMs.
2. **Binary search over snapshots.** Your command runs in each candidate
   snapshot. Exit 0 is good, 125 is "cannot test this one", anything else is
   bad, the same convention as `git bisect run`.
3. **Search over packages.** Between the last good and first bad snapshot,
   tw-bisect lists the installed packages that differ. Starting from the good
   image, it upgrades half of them from the bad snapshot's repository, reruns
   the test, and keeps halving. Packages do not upgrade in isolation (zypper
   pulls dependencies along), so the search follows what actually changed in
   the rpm database rather than what it asked for. It reports the smallest set
   it can separate, says so when dependencies make a group inseparable, and
   says so when the failure needs several upgrades together.
4. **Changelog.** The culprit's entry is pulled from the snapshot's published
   `Changes.<snapshot>.txt`, so the report ends with the upstream change, bug
   numbers and all.

## Install

```
go install github.com/officialasishkumar/tw-bisect@latest
```

You need docker or podman. The archive only holds x86_64 packages, so on an
ARM machine (Apple Silicon included) tw-bisect runs the containers under
emulation automatically; it works, it is just slower.

## Commands

```
tw-bisect snapshots                       what is still in the archive
tw-bisect diff 20261104 20261105          every package that changed, whole distribution
tw-bisect diff 20261104 20261105 --changelog --only rsync,kmod
tw-bisect run --good G --bad B [--pkgs a,b] -- <command>
tw-bisect run ... --snapshots-only        stop after the snapshot search
tw-bisect run ... --json report.json      machine-readable result
tw-bisect shell 20261020 --pkgs python3   poke around inside a snapshot
```

The test command is run by bash inside the snapshot. Put the packages it needs
in `--pkgs`; every image gets bash, coreutils, grep, rpm, zypper and
CA certificates, nothing else.

## Limits

- Twenty snapshots. That is what openSUSE keeps. If the break is older,
  `tw-bisect diff` still works for any 2026 snapshot through the published
  change files, but nothing can be executed.
- Containers, not machines. Kernel, boot loader, graphics and systemd
  regressions are out of reach; libraries, interpreters, command line tools
  and anything with a test suite are in.
- One failure at a time. If two independent regressions landed in the same
  range, bisect finds one of them.

## Why not Snapper?

Snapper rolls the whole machine back, which is the right first move. It does
not tell you which of the 300 packages in yesterday's dup to report a bug
against. tw-bisect does, and it does it on a copy, so the machine you work on
stays as it is.

## License

MIT
