# go-macos/diskarbitration

[![ci](https://github.com/go-macos/diskarbitration/actions/workflows/ci.yml/badge.svg)](https://github.com/go-macos/diskarbitration/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-macos/diskarbitration.svg)](https://pkg.go.dev/github.com/go-macos/diskarbitration)
[![License](https://img.shields.io/badge/license-BSD--3--Clause-blue.svg)](LICENSE)

**Apple's DiskArbitration framework from pure Go, `CGO_ENABLED=0`: which block
devices exist, which volume is mounted where, and how to take one away
politely.** No cgo, no `hdiutil`, no `diskutil`, no plist parsing — it reaches
the framework through [purego](https://github.com/ebitengine/purego), and its
run loop through [`go-macos/objc`](https://github.com/go-macos/objc).

```go
s, err := diskarbitration.Open()
if err != nil {
	return err
}
defer s.Close()

for _, d := range must(s.Mounts()) {
	fmt.Println(d)
}
// disk3s1s1 4.0 TB "Macintosh HD" [apfs] on / (read-only, Apple Fabric)
// disk3s5   4.0 TB "Data"         [apfs] on /System/Volumes/Data (encrypted, Apple Fabric)
```

## Why

A program that attaches a disk image needs three answers macOS will not give it
from the filesystem alone:

- **Which `/dev` node did my image become?**
- **Where did the system mount it?**
- **How do I detach it without corrupting it?**

Today those answers come from running `hdiutil` and `diskutil` and parsing their
output — a subprocess, a plist parse, and a text format Apple never promised to
keep stable. DiskArbitration is the service those tools themselves talk to.

## The boundary

**This package decodes no filesystem format.** Not APFS, not HFS+, not FAT, not
a partition map. It asks a macOS daemon what that daemon already believes and
reports the answer. `Description.VolumeKind` is a label macOS handed over
(`"apfs"`, `"hfs"`, `"msdos"`); nothing here parsed a superblock to earn it.

| | |
|---|---|
| **Filesystem decoding** | [`go-filesystems`](https://github.com/go-filesystems) — 18 drivers behind `go-filesystems/interface`, OS-independent by construction. |
| **Asking macOS** | this package. |
| **Asking Linux** | [`go-fsctl`](https://github.com/go-fsctl), its own org, for the same reason. |

Two OS-specific packages that ask their own system; one OS-independent org that
reads bytes. The moment a superblock is parsed here, that independence is gone
and one of those drivers has been forked into a macOS-only copy.

## The API

| | |
|---|---|
| `Open() (*Session, error)` | `DASessionCreate` + a CFRunLoop on a dedicated thread. |
| `(*Session) Close() error` | unregisters, unschedules, stops, releases. Idempotent. |
| `(*Session) Disks() ([]string, error)` | the BSD names, in device order. |
| `(*Session) Describe(name) (*Description, error)` | `DADiskCreateFromBSDName` + `DADiskCopyDescription`. |
| `(*Session) DescribeAll() ([]*Description, error)` | every device; skips ones that vanish mid-scan. |
| `(*Session) Mounts() ([]*Description, error)` | only the volumes that are mounted. |
| `(*Session) Unmount(name, UnmountOptions) error` | `DADiskUnmount`, made synchronous. |
| `(*Session) Eject(name) error` | `DADiskEject`, made synchronous. |
| `(*Session) Watch(func(Event)) (*Watcher, error)` | `DARegisterDiskAppeared` / `DiskDisappeared`. |
| `ValidName(string) bool` | is this a `diskN[sM[sK]]` name? |

`Description` carries the typed fields (`BSDName`, `MediaSize`, `Whole`, `Leaf`,
`Removable`, `Ejectable`, `Writable`, `VolumePath`, `VolumeName`, `VolumeKind`,
`Protocol`, …) plus `Raw`, the whole description dictionary converted to Go
values — because the typed fields are a selection, and needing a key nobody
anticipated should not mean forking the package.

## Registering a watch replays what is already there

```go
w, err := s.Watch(func(e diskarbitration.Event) { events <- e })
defer w.Stop()
```

DiskArbitration delivers an `Appeared` event for every disk **already present**
the moment the callback is registered. So a program that wants "what is here,
and then what changes" needs only `Watch` — no separate enumeration, and no
window between the two in which a disk could slip through unseen.

Callbacks run **on the run-loop thread**. Everything else the session does
asynchronously — the completion of an `Unmount`, every other event — waits
behind your handler. Send to a channel and return.

## Unmount is not eject

The media must already be unmounted before it can be ejected; eject a mounted
volume and the daemon answers busy. So detaching a disk image is two calls:

```go
if err := s.Unmount(whole, diskarbitration.UnmountWhole); err != nil {
	return err
}
return s.Eject(whole)
```

`UnmountWhole` takes down every volume of the whole disk. `UnmountForce` is not
a stronger request but a **different** one: open files are forcibly closed and
another process's unwritten data may be lost.

## Refusals say why, in the spelling macOS actually used

`DADissenterGetStatus`'s documentation says a BSD return code "is encoded with
`unix_err()`", and it means it. Measured on macOS 26.6.2, refusing to unmount a
volume with an open file on it answers `0x0000C010` — not in the `kDAReturn`
family at all. It is `unix_err(EBUSY)`.

A binding that only knows the `kDAReturn` constants therefore reports the one
failure everybody actually meets as an unrecognised number. This one decodes
both:

```
diskarbitration: unmount disk4s1: EBUSY
Advice: Something still has the volume open. Close it, or unmount with the Force option.
```

`errors.As` into `*DiskError` for `Op`, `Disk`, `Status` and the daemon's own
`Message`; `Status.Errno()` for the errno when there is one; `Status.Advice()`
for the sentence to put in front of a person.

## Enumeration reads `/dev`

DiskArbitration has no "list" call. Its own way to enumerate is the appearance
replay above — which needs a run loop, a registration, and a guess at how long
to wait before deciding the replay is over. `Disks()` scans `/dev` instead,
which is exact and immediate; the two were checked against each other on a live
machine and reported the same twenty devices.

## Every platform

macOS only, but every symbol exists everywhere and every operation reports
`ErrUnsupported` off darwin, so a consumer cross-compiles without build tags of
its own. Verified building and vetting on `windows/{amd64,arm64}`,
`linux/{amd64,arm64,riscv64,s390x,ppc64le,loong64}`, `darwin/amd64`,
`android/arm64` and `js/wasm`.

The OS-independent half — the BSD-name grammar, the ordering, the description
typing, the `DAReturn` vocabulary, the session guards — is **run**, not merely
compiled, on all six of Go's 64-bit architectures.

## Tests do not touch this machine's disks

Coverage is **100 %**, statement for statement, on darwin and off it. That is
reached through seams, not through disks: every bound C entry point is a package
variable, so a test can make `DASessionCreate` answer NULL, make an unmount go
unanswered, or hand `cfValue` a CFData, without a device being involved.

The live suite talks to the real daemon, and is read-only about the machine's
media. The two tests that exercise the write path aim at a BSD name they **first
prove is not a device**, and assert the daemon refuses. The one test that needs
a disk to actually disappear creates a small image of its own, attaches it with
`-nomount`, and detaches that — never a device it did not create.

## `cmd/dalist`

```
go run github.com/go-macos/diskarbitration/cmd/dalist@latest -l
```

Read-only: one line per device, `-l` for every description key, `-mounts` for
the mounted ones, `-watch 10s` to follow appearances and disappearances.

## Install

```
go get github.com/go-macos/diskarbitration
```

BSD-3-Clause.
