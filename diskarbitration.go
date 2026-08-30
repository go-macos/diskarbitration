// Copyright (c) 2026, the go-macos authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

package diskarbitration

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Framework is the DiskArbitration framework's path, opened by the darwin half
// on first use. It is exported because a caller that loads frameworks of its
// own has one list to keep, not two.
const Framework = "/System/Library/Frameworks/DiskArbitration.framework/DiskArbitration"

// Sentinel errors. They are stable and may be compared with [errors.Is].
var (
	// ErrUnsupported is returned by every operation on non-darwin platforms.
	// DiskArbitration is macOS-only; the symbols exist everywhere so
	// consumers cross-compile without build tags of their own.
	ErrUnsupported = errors.New("diskarbitration: unsupported on this platform (darwin only)")

	// ErrNoSession reports that DASessionCreate returned NULL. It is not a
	// disk-level failure: the process could not reach the arbitration daemon
	// at all, and nothing else in this package will work until it can.
	ErrNoSession = errors.New("diskarbitration: DASessionCreate returned no session")

	// ErrClosed reports use of a [Session] after [Session.Close].
	ErrClosed = errors.New("diskarbitration: session is closed")

	// ErrNoDisk reports that DADiskCreateFromBSDName returned NULL.
	//
	// It is RARE, and it does not mean "no such device". Creating a DADisk
	// reference is a local operation that does not consult the daemon:
	// measured on macOS 26.6.2, asking for "disk99" — a device that does not
	// exist — succeeds and yields a perfectly good reference. NULL here means
	// the framework could not allocate at all, or the session is unusable.
	// The absence of a device shows up as [ErrNoDescription].
	ErrNoDisk = errors.New("diskarbitration: no such disk")

	// ErrNoDescription reports that DADiskCopyDescription returned NULL,
	// which is what "there is no such device" actually looks like — see
	// [ErrNoDisk] for why the two are the other way round from what the
	// names suggest.
	//
	// It is also what a caller sees when a disk vanishes BETWEEN the
	// enumeration and the description: a removable drive being unplugged, or
	// a disk image detaching underneath. [Session.DescribeAll] skips those
	// rather than failing the whole listing.
	ErrNoDescription = errors.New("diskarbitration: disk has no description (it may have gone away)")

	// ErrBadName reports a BSD name that is not of the form macOS uses for a
	// block device: diskN, diskNsM, or diskNsMsK for an APFS volume inside a
	// container. It is checked BEFORE the name reaches C, because the name
	// crosses as a NUL-terminated string and an embedded NUL would silently
	// truncate it into a different, existing device.
	ErrBadName = errors.New("diskarbitration: not a BSD disk name")
)

// ---------------------------------------------------------------------------
// DAReturn.
// ---------------------------------------------------------------------------

// Return is a DAReturn: the status DiskArbitration reports for an operation it
// refused. The numeric values are Apple's, pinned here so a mis-ordered
// constant cannot turn "busy" into "not permitted" silently.
type Return uint32

// The DAReturn values. kDAReturnSuccess is zero; the rest are err_local |
// err_local_diskarbitration | n, which is the 0xF8DA00nn family.
const (
	ReturnSuccess         Return = 0
	ReturnError           Return = 0xF8DA0001
	ReturnBusy            Return = 0xF8DA0002
	ReturnBadArgument     Return = 0xF8DA0003
	ReturnExclusiveAccess Return = 0xF8DA0004
	ReturnNoResources     Return = 0xF8DA0005
	ReturnNotFound        Return = 0xF8DA0006
	ReturnNotMounted      Return = 0xF8DA0007
	ReturnNotPermitted    Return = 0xF8DA0008
	ReturnNotPrivileged   Return = 0xF8DA0009
	ReturnNotReady        Return = 0xF8DA000A
	ReturnNotWritable     Return = 0xF8DA000B
	ReturnUnsupported     Return = 0xF8DA000C
)

// The Mach error encoding DiskArbitration wraps a BSD errno in.
//
// DADissenterGetStatus's own documentation says it: "A BSD return code, if
// applicable, is encoded with unix_err()." And it IS applicable, routinely —
// measured on macOS 26.6.2, refusing to unmount a volume with an open file on
// it answers 0x0000C010, which is not in the kDAReturn family at all. It is
// unix_err(EBUSY).
//
// A binding that only knows the kDAReturn constants therefore reports the one
// failure everybody actually meets as an unrecognised number, and
// [Return.Advice] — the sentence that tells a person to close their files —
// never fires. That is why the errno half is decoded here rather than left to
// the caller.
//
// From <mach/error.h>: unix_err(e) = err_kern | err_sub(3) | e, and
// err_sub(3) = 3<<14 = 0xC000, with the code in the low 14 bits.
const (
	unixErrBase Return = 0x0000C000
	unixErrMask Return = 0x00003FFF
)

// errnoNames names the BSD errors a disk operation can be refused with. It is
// deliberately NOT syscall.Errno: this file compiles and is TESTED on every
// platform, and syscall.Errno means a Windows error code on Windows, where the
// same number would be given a different and wrong name.
var errnoNames = map[int]string{
	1:  "EPERM",
	2:  "ENOENT",
	5:  "EIO",
	6:  "ENXIO",
	9:  "EBADF",
	11: "EDEADLK",
	12: "ENOMEM",
	13: "EACCES",
	14: "EFAULT",
	15: "ENOTBLK",
	16: "EBUSY",
	17: "EEXIST",
	18: "EXDEV",
	19: "ENODEV",
	20: "ENOTDIR",
	21: "EISDIR",
	22: "EINVAL",
	28: "ENOSPC",
	30: "EROFS",
	35: "EAGAIN",
	45: "ENOTSUP",
	82: "EPWROFF",
	83: "EDEVERR",
}

// Errno reports the BSD errno the status carries, when it carries one. A
// kDAReturn constant is not an errno and answers false.
func (r Return) Errno() (int, bool) {
	if r&^unixErrMask != unixErrBase {
		return 0, false
	}
	return int(r & unixErrMask), true
}

// String names the status the way Apple's own constant does, or the errno it
// wraps.
func (r Return) String() string {
	if e, ok := r.Errno(); ok {
		if name, known := errnoNames[e]; known {
			return name
		}
		return fmt.Sprintf("errno %d", e)
	}
	switch r {
	case ReturnSuccess:
		return "success"
	case ReturnError:
		return "error"
	case ReturnBusy:
		return "busy"
	case ReturnBadArgument:
		return "badArgument"
	case ReturnExclusiveAccess:
		return "exclusiveAccess"
	case ReturnNoResources:
		return "noResources"
	case ReturnNotFound:
		return "notFound"
	case ReturnNotMounted:
		return "notMounted"
	case ReturnNotPermitted:
		return "notPermitted"
	case ReturnNotPrivileged:
		return "notPrivileged"
	case ReturnNotReady:
		return "notReady"
	case ReturnNotWritable:
		return "notWritable"
	case ReturnUnsupported:
		return "unsupported"
	}
	return fmt.Sprintf("Return(0x%08X)", uint32(r))
}

// Advice is the sentence to show a person, or "" when the status speaks for
// itself. It exists because the statuses that actually happen — a busy volume
// above all — are fixable by the person at the keyboard, and none of them says
// so.
//
// It answers for the errno spellings as well as the kDAReturn ones, because the
// daemon uses both for the same situation and a caller should not have to know
// which it got today.
func (r Return) Advice() string {
	if e, ok := r.Errno(); ok {
		switch e {
		case 16: // EBUSY
			return ReturnBusy.Advice()
		case 1, 13: // EPERM, EACCES
			return ReturnNotPrivileged.Advice()
		case 22: // EINVAL
			return "macOS rejected the request as malformed: the volume is probably not mounted, or the device is not the one you meant."
		case 30: // EROFS
			return "The filesystem is read-only."
		}
		return ""
	}
	switch r {
	case ReturnBusy:
		return "Something still has the volume open. Close it, or unmount with the Force option."
	case ReturnNotPrivileged, ReturnNotPermitted:
		return "macOS refused this to an unprivileged process; the volume may belong to another user or be protected by the system."
	case ReturnNotMounted:
		return "The volume is not mounted, so there is nothing to unmount."
	case ReturnExclusiveAccess:
		return "Another process holds the whole device exclusively (a running disk-image or virtualisation tool, typically)."
	}
	return ""
}

// DiskError is a failure DiskArbitration reported about one disk. It carries
// the dissenter's status and, when the daemon supplied one, its own sentence.
type DiskError struct {
	// Op is the operation that failed: "unmount" or "eject".
	Op string
	// Disk is the BSD name it was asked about.
	Disk string
	// Status is the DAReturn the dissenter carried.
	Status Return
	// Message is the daemon's own status string, or "" when it gave none.
	Message string
}

// Error renders the failure, preferring the daemon's own words when it had any.
func (e *DiskError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("diskarbitration: %s %s: %s (%s)", e.Op, e.Disk, e.Message, e.Status)
	}
	return fmt.Sprintf("diskarbitration: %s %s: %s", e.Op, e.Disk, e.Status)
}

// Advice forwards [Return.Advice] for the status that was reported.
func (e *DiskError) Advice() string { return e.Status.Advice() }

// newDiskError builds a [DiskError], and returns nil for a successful status so
// a caller can hand the raw dissenter values straight through.
//
// The daemon often supplies no status string at all — it did not, for the busy
// unmount this package was measured against — so when it is silent and the
// status is a known one, that status's own name is used rather than leaving the
// caller with a bare hexadecimal number.
func newDiskError(op, disk string, status uint32, msg string) error {
	if Return(status) == ReturnSuccess {
		return nil
	}
	return &DiskError{Op: op, Disk: disk, Status: Return(status), Message: msg}
}

// ---------------------------------------------------------------------------
// Description keys.
// ---------------------------------------------------------------------------

// The DADiskDescription keys, as the strings DiskArbitration actually uses.
//
// Apple documents these as kDADiskDescription*Key, each an exported CFStringRef
// VARIABLE. Reading one means dereferencing an exported data pointer, which is
// exactly what go vet's unsafeptr check rejects — and the value it holds is the
// literal spelled here. CoreFoundation dictionaries hash and compare CFStrings
// by content, so a key built from the literal finds the same entry. This is the
// same trade github.com/go-macos/objc makes for kCFRunLoopDefaultMode, and the
// values below were read back off a live description dictionary rather than
// transcribed from a header.
const (
	KeyVolumePath      = "DAVolumePath"
	KeyVolumeName      = "DAVolumeName"
	KeyVolumeKind      = "DAVolumeKind"
	KeyVolumeMountable = "DAVolumeMountable"
	KeyVolumeNetwork   = "DAVolumeNetwork"
	KeyVolumeUUID      = "DAVolumeUUID"

	KeyMediaBSDName   = "DAMediaBSDName"
	KeyMediaName      = "DAMediaName"
	KeyMediaSize      = "DAMediaSize"
	KeyMediaBlockSize = "DAMediaBlockSize"
	KeyMediaWhole     = "DAMediaWhole"
	KeyMediaLeaf      = "DAMediaLeaf"
	KeyMediaRemovable = "DAMediaRemovable"
	KeyMediaEjectable = "DAMediaEjectable"
	KeyMediaWritable  = "DAMediaWritable"
	KeyMediaEncrypted = "DAMediaEncrypted"
	KeyMediaContent   = "DAMediaContent"
	KeyMediaPath      = "DAMediaPath"

	KeyDeviceProtocol = "DADeviceProtocol"
	KeyDeviceModel    = "DADeviceModel"
	KeyDeviceVendor   = "DADeviceVendor"
	KeyDeviceRevision = "DADeviceRevision"
	KeyDeviceInternal = "DADeviceInternal"
	KeyDevicePath     = "DADevicePath"

	KeyBusName = "DABusName"
	KeyBusPath = "DABusPath"

	KeyAppearanceTime = "DAAppearanceTime"
)

// How macOS marks a device that is an attached disk image rather than hardware.
// This is the answer to "which of these is my DMG": a caller that attached an
// image and wants its /dev node keeps the descriptions [Description.IsDiskImage]
// accepts.
//
// The two constants are BOTH needed, and which one carries the mark has moved.
// Measured on macOS 26.0 (arm64) against a freshly attached image: the protocol
// is "Virtual Interface" and it is the MODEL that says "Disk Image". Older
// releases — and much code written against them — put "Disk Image" in the
// protocol. Neither is checked alone here, because a binding that picked the
// wrong one would answer "no disk images attached" on half the fleet and give
// no hint why.
const (
	// ModelDiskImage is the DADeviceModel of an attached image.
	ModelDiskImage = "Disk Image"
	// ProtocolVirtualInterface is the DADeviceProtocol of an attached image
	// on current macOS.
	ProtocolVirtualInterface = "Virtual Interface"
)

// ---------------------------------------------------------------------------
// Description.
// ---------------------------------------------------------------------------

// Description is what DiskArbitration says about one disk: a typed view of the
// description dictionary. Absent keys are the zero value — macOS omits a key
// rather than reporting an empty one, so "" for [Description.VolumeName] means
// "no volume here", not "a volume with no name".
//
// Nothing in it was computed by reading the device. It is the daemon's opinion,
// which is the only opinion that matches what the rest of macOS will do.
type Description struct {
	// BSDName is the device node's name without /dev, e.g. "disk3s1s1".
	BSDName string
	// MediaName is the IOKit media name: a product string for a whole disk,
	// a partition label for a slice.
	MediaName string
	// MediaSize is the media's size in bytes.
	MediaSize int64
	// BlockSize is the media's block size in bytes.
	BlockSize int64
	// Whole reports a whole disk (diskN) rather than one of its slices.
	Whole bool
	// Leaf reports media with no further partition scheme below it.
	Leaf bool
	// Removable reports media that can be removed from its drive.
	Removable bool
	// Ejectable reports media macOS can eject — the precondition for
	// [Session.Eject] meaning anything.
	Ejectable bool
	// Writable reports media that is not write-protected.
	Writable bool
	// Encrypted reports media macOS considers encrypted.
	Encrypted bool
	// Content is the partition's type hint: a GPT type GUID, or a scheme
	// name such as "GUID_partition_scheme" on a whole disk.
	Content string
	// MediaPath is the IOKit registry path of the media object.
	MediaPath string

	// VolumePath is the mount point, or "" when nothing is mounted. It is
	// THE answer to "where did my image end up".
	VolumePath string
	// VolumeName is the volume's name as the Finder shows it.
	VolumeName string
	// VolumeKind is the filesystem macOS believes is there ("apfs", "hfs",
	// "msdos", …).
	//
	// It is a LABEL, not a decode: no superblock was parsed to produce it.
	// Reading the filesystem is github.com/go-filesystems' job.
	VolumeKind string
	// VolumeMountable reports a volume macOS knows how to mount.
	VolumeMountable bool
	// VolumeNetwork reports a network volume rather than local media.
	VolumeNetwork bool
	// VolumeUUID is the volume's UUID, or "".
	VolumeUUID string

	// Protocol is the transport: "USB", "Apple Fabric", "PCI-Express", or
	// [ProtocolDiskImage] for an attached image.
	Protocol string
	// Model, Vendor and Revision are the device's identification strings.
	Model    string
	Vendor   string
	Revision string
	// Internal reports a device built into the machine.
	Internal bool
	// DevicePath is the IOKit registry path of the device.
	DevicePath string
	// BusName and BusPath identify the bus the device hangs off.
	BusName string
	BusPath string

	// Raw is the whole description dictionary, converted to Go values
	// (string, bool, int64, []byte). It is kept because the typed fields
	// above are a selection, and a caller that needs a key nobody
	// anticipated should not have to fork this package to reach it.
	Raw map[string]any
}

// Mounted reports whether the volume is mounted somewhere.
func (d *Description) Mounted() bool { return d.VolumePath != "" }

// IsDiskImage reports whether the device is backed by a disk image rather than
// hardware. See [ModelDiskImage] for why it accepts three spellings.
func (d *Description) IsDiskImage() bool {
	return d.Model == ModelDiskImage ||
		d.Protocol == ModelDiskImage ||
		d.Protocol == ProtocolVirtualInterface
}

// Device is the full path of the block device node, e.g. "/dev/disk3s1s1". It
// is "" for a description with no BSD name, which is what a network volume
// looks like.
func (d *Description) Device() string {
	if d.BSDName == "" {
		return ""
	}
	return "/dev/" + d.BSDName
}

// String renders one line: the node, what is on it and where it is mounted.
func (d *Description) String() string {
	var b strings.Builder
	if d.BSDName == "" {
		b.WriteString("(no device)")
	} else {
		b.WriteString(d.BSDName)
	}
	if d.MediaSize > 0 {
		fmt.Fprintf(&b, " %s", humanSize(d.MediaSize))
	}
	if d.VolumeName != "" {
		fmt.Fprintf(&b, " %q", d.VolumeName)
	}
	if d.VolumeKind != "" {
		fmt.Fprintf(&b, " [%s]", d.VolumeKind)
	}
	if d.VolumePath != "" {
		fmt.Fprintf(&b, " on %s", d.VolumePath)
	}
	var flags []string
	if d.Whole {
		flags = append(flags, "whole")
	}
	if d.Removable {
		flags = append(flags, "removable")
	}
	if d.Ejectable {
		flags = append(flags, "ejectable")
	}
	if d.Encrypted {
		flags = append(flags, "encrypted")
	}
	// "read-only" is reported only when macOS actually said so. Writable is
	// false for a description that simply has no media key at all, and
	// labelling that read-only would invent a fact about a device nobody
	// described.
	if _, said := d.Raw[KeyMediaWritable]; said && !d.Writable {
		flags = append(flags, "read-only")
	}
	if d.Protocol != "" {
		flags = append(flags, d.Protocol)
	}
	if len(flags) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(flags, ", "))
	}
	return b.String()
}

// humanSize renders a byte count in the decimal units macOS itself uses (a
// 4 TB disk is 4,002,222,325,760 bytes and Apple calls it 4 TB, not 3.64 TiB).
func humanSize(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTP"[exp])
}

// parseDescription types the converted description dictionary. It is the whole
// of the mapping from macOS's vocabulary to this package's, and it is portable
// on purpose: every branch of it is reachable from a test on any machine.
func parseDescription(raw map[string]any) *Description {
	d := &Description{
		BSDName:   dictString(raw, KeyMediaBSDName),
		MediaName: dictString(raw, KeyMediaName),
		MediaSize: dictInt(raw, KeyMediaSize),
		BlockSize: dictInt(raw, KeyMediaBlockSize),
		Whole:     dictBool(raw, KeyMediaWhole),
		Leaf:      dictBool(raw, KeyMediaLeaf),
		Removable: dictBool(raw, KeyMediaRemovable),
		Ejectable: dictBool(raw, KeyMediaEjectable),
		Writable:  dictBool(raw, KeyMediaWritable),
		Encrypted: dictBool(raw, KeyMediaEncrypted),
		Content:   dictString(raw, KeyMediaContent),
		MediaPath: dictString(raw, KeyMediaPath),

		VolumePath:      dictString(raw, KeyVolumePath),
		VolumeName:      dictString(raw, KeyVolumeName),
		VolumeKind:      dictString(raw, KeyVolumeKind),
		VolumeMountable: dictBool(raw, KeyVolumeMountable),
		VolumeNetwork:   dictBool(raw, KeyVolumeNetwork),
		VolumeUUID:      dictString(raw, KeyVolumeUUID),

		Protocol:   dictString(raw, KeyDeviceProtocol),
		Model:      dictString(raw, KeyDeviceModel),
		Vendor:     dictString(raw, KeyDeviceVendor),
		Revision:   dictString(raw, KeyDeviceRevision),
		Internal:   dictBool(raw, KeyDeviceInternal),
		DevicePath: dictString(raw, KeyDevicePath),
		BusName:    dictString(raw, KeyBusName),
		BusPath:    dictString(raw, KeyBusPath),

		Raw: raw,
	}
	return d
}

// dictString reads a string key, answering "" for an absent key AND for a key
// whose value is of another type. The second case is not paranoia: the
// dictionary is built by converting CoreFoundation objects, and a key that
// arrives as a CFDictionary (DAMediaIcon does) or a CFData must not be able to
// panic a caller who asked for a string.
func dictString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// dictBool reads a boolean key; a missing or differently-typed value is false.
func dictBool(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}

// dictInt reads an integer key. CFNumbers are converted to int64, but a value
// that arrived as some other numeric shape is still accepted rather than
// silently dropped.
func dictInt(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	}
	return 0
}

// ---------------------------------------------------------------------------
// Events.
// ---------------------------------------------------------------------------

// EventKind is what happened to a disk.
type EventKind int

const (
	// Appeared means a disk became known to DiskArbitration. Registering a
	// watch replays one of these for every disk ALREADY present, so a
	// watcher never has to enumerate separately to catch up.
	Appeared EventKind = iota + 1
	// Disappeared means a disk went away. Its description can no longer be
	// copied, so [Event.Description] is nil and only [Event.BSDName]
	// identifies it.
	Disappeared
)

// String names the kind.
func (k EventKind) String() string {
	switch k {
	case Appeared:
		return "appeared"
	case Disappeared:
		return "disappeared"
	}
	return fmt.Sprintf("EventKind(%d)", int(k))
}

// Event is one disk appearing or disappearing.
type Event struct {
	// Kind is [Appeared] or [Disappeared].
	Kind EventKind
	// BSDName is the device node's name, or "" for a disk that has none —
	// which a mounted network volume does, and DiskArbitration reports it
	// alongside the real ones.
	BSDName string
	// Description is the disk's description at the moment of the event, or
	// nil for a [Disappeared] event (there is nothing left to describe).
	Description *Description
}

// String renders the event for a log line.
func (e Event) String() string {
	name := e.BSDName
	if name == "" {
		name = "(no device)"
	}
	if e.Description != nil && e.Description.VolumeName != "" {
		return fmt.Sprintf("%s %s %q", e.Kind, name, e.Description.VolumeName)
	}
	return fmt.Sprintf("%s %s", e.Kind, name)
}

// ---------------------------------------------------------------------------
// Options.
// ---------------------------------------------------------------------------

// UnmountOptions is a DADiskUnmountOptions bit set.
type UnmountOptions uint32

const (
	// UnmountDefault unmounts the one volume, and fails with [ReturnBusy] if
	// anything still has it open.
	UnmountDefault UnmountOptions = 0
	// UnmountWhole unmounts every volume of the whole disk the named device
	// belongs to. It is kDADiskUnmountOptionWhole, and it is what "detach
	// this image" means for an image with several partitions.
	UnmountWhole UnmountOptions = 0x00000001
	// UnmountForce unmounts even though something has the volume open.
	//
	// It is not a stronger request; it is a DIFFERENT one. Open files are
	// forcibly closed and unwritten data belonging to another process may be
	// lost. Reach for it when a person has been told what it means.
	UnmountForce UnmountOptions = 0x00080000
)

// String renders the options for an error message.
func (o UnmountOptions) String() string {
	var parts []string
	if o&UnmountWhole != 0 {
		parts = append(parts, "whole")
	}
	if o&UnmountForce != 0 {
		parts = append(parts, "force")
	}
	if len(parts) == 0 {
		return "default"
	}
	return strings.Join(parts, "|")
}

// ---------------------------------------------------------------------------
// BSD names.
// ---------------------------------------------------------------------------

// devDir is the directory scanned by [Session.Disks]. It is a variable so a
// test can point the scan at a fixture directory instead of the real one.
var devDir = "/dev"

// readDirFn is the seam [Session.Disks] reads through.
var readDirFn = os.ReadDir

// parseBSDName splits a macOS block-device name into its numbers and reports
// whether it is one at all.
//
// The grammar is diskN, then zero or more sN groups: "disk0", "disk0s2", and
// "disk3s1s1" for an APFS volume inside a container inside a partition. It is
// parsed rather than pattern-matched because [Session.Disks] must also SORT the
// result, and sorting these as strings puts disk10 before disk2.
//
// Rejecting a bad name here is a safety property, not tidiness: the name
// crosses into C as a NUL-terminated string, so an embedded NUL would truncate
// "disk1\x00s9" into "disk1" — a real device, and the wrong one.
func parseBSDName(name string) ([]int, bool) {
	rest, ok := strings.CutPrefix(name, "disk")
	if !ok {
		return nil, false
	}
	var nums []int
	for {
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 0 {
			return nil, false // no digits where a number must be
		}
		n, err := strconv.Atoi(rest[:i])
		if err != nil {
			return nil, false // an integer too large for the platform int
		}
		nums = append(nums, n)
		rest = rest[i:]
		if rest == "" {
			return nums, true
		}
		if rest[0] != 's' {
			return nil, false
		}
		rest = rest[1:]
	}
}

// ValidName reports whether name is a macOS block-device name this package will
// speak about. Use it to check a name from a configuration file or a user
// before handing it to [Session.Describe].
func ValidName(name string) bool {
	_, ok := parseBSDName(name)
	return ok
}

// lessBSDName orders two parsed names: by unit, then by each slice level, with
// the shorter name first when one is a prefix of the other (so disk3 precedes
// disk3s1 precedes disk3s1s1).
func lessBSDName(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// filterDiskNames keeps the block-device names out of a directory listing and
// returns them in device order.
//
// The rdiskN character devices are deliberately dropped. They are the same
// media reached raw, DiskArbitration describes them identically, and reporting
// both would double every list for no information.
func filterDiskNames(entries []fs.DirEntry) []string {
	type row struct {
		name string
		nums []int
	}
	var rows []row
	for _, e := range entries {
		nums, ok := parseBSDName(e.Name())
		if !ok {
			continue
		}
		rows = append(rows, row{e.Name(), nums})
	}
	sort.Slice(rows, func(i, j int) bool { return lessBSDName(rows[i].nums, rows[j].nums) })
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.name
	}
	return out
}

// ---------------------------------------------------------------------------
// Seams. Assigned in an init(): on darwin to the real DiskArbitration calls,
// elsewhere to stubs that report ErrUnsupported. Everything above them — name
// parsing, sorting, description typing, status naming, the closed-session
// guard — is shared with every platform, which is what lets all of it be tested
// on a runner with no macOS anywhere.
// ---------------------------------------------------------------------------

// handle identifies a live session to the platform half. It is an opaque token
// rather than a pointer so that nothing here holds a C reference, and so the
// darwin half can key its callback contexts by the same value.
type handle uintptr

var (
	// sessionOpen creates a session and starts its run loop.
	sessionOpen func() (handle, error)
	// sessionClose stops the run loop and releases the session.
	sessionClose func(handle)
	// diskDescribe copies one disk's description as Go values.
	diskDescribe func(h handle, bsdName string) (map[string]any, error)
	// diskUnmount unmounts and waits for the daemon's answer.
	diskUnmount func(h handle, bsdName string, opts uint32) error
	// diskEject ejects and waits for the daemon's answer.
	diskEject func(h handle, bsdName string) error
	// diskWatch registers the appearance callbacks; the returned function
	// unregisters them.
	diskWatch func(h handle, emit func(EventKind, string, map[string]any)) (func(), error)
)

// ---------------------------------------------------------------------------
// Session.
// ---------------------------------------------------------------------------

// Session is a connection to the disk arbitration daemon plus the run loop its
// asynchronous answers are delivered on. Open one, keep it, close it.
//
// Every method is safe to call from any goroutine. Closing is idempotent, and a
// method called on a closed session reports [ErrClosed] rather than reaching a
// released C object — which is the difference between an error and a crash in
// somebody else's stack frame.
type Session struct {
	mu      sync.Mutex
	h       handle
	closed  bool
	watches []func()
}

// Open connects to the disk arbitration daemon and starts the run loop its
// callbacks need. The caller must Close the result.
//
// It reports [ErrUnsupported] off darwin and [ErrNoSession] when the daemon
// cannot be reached — which is a real outcome in a sandbox that has not been
// granted the service, not a defensive branch.
func Open() (*Session, error) {
	h, err := sessionOpen()
	if err != nil {
		return nil, err
	}
	return &Session{h: h}, nil
}

// Close unregisters every watch, stops the run loop and releases the session.
// It is idempotent and always reports nil, so it may be deferred without
// ceremony; the error is in the signature because a Closer that cannot fail
// today is still a Closer.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	for _, stop := range s.watches {
		stop()
	}
	s.watches = nil
	sessionClose(s.h)
	return nil
}

// use runs fn with the session's handle under the lock, refusing a closed
// session. It is the one place the closed check lives.
func (s *Session) use(fn func(h handle) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return fn(s.h)
}

// Disks lists the BSD names of the block devices present, in device order
// (disk0, disk0s1, …, disk10). The names are what [Session.Describe],
// [Session.Unmount] and [Session.Eject] take.
//
// It enumerates by reading /dev rather than by asking DiskArbitration, and the
// reason is worth stating: DiskArbitration has no "list" call at all. The
// framework's own way to enumerate is to register an appearance callback and
// let the daemon replay one event per existing disk — which needs a run loop, a
// registration, and a guess at how long to wait before deciding the replay is
// over. The /dev scan is exact and immediate, and it was checked against the
// callback replay on a live machine: the same twenty devices, in both.
//
// [Session.Watch] still uses the replay, because a watcher wants the events
// anyway.
func (s *Session) Disks() ([]string, error) {
	var names []string
	err := s.use(func(handle) error {
		entries, err := readDirFn(devDir)
		if err != nil {
			return fmt.Errorf("diskarbitration: list %s: %w", devDir, err)
		}
		names = filterDiskNames(entries)
		return nil
	})
	return names, err
}

// Describe answers what DiskArbitration knows about one disk. The name is a BSD
// name without /dev ("disk3s1s1"); a leading /dev/ is accepted and trimmed,
// because that is the form a caller has after reading a mount table.
//
// It reports [ErrBadName] for a name that is not a block device's, and
// [ErrNoDescription] both when there is no such device and when there was one a
// moment ago — DiskArbitration does not distinguish those, and neither does
// this.
func (s *Session) Describe(name string) (*Description, error) {
	bsd := strings.TrimPrefix(name, "/dev/")
	if !ValidName(bsd) {
		return nil, fmt.Errorf("%w: %q", ErrBadName, name)
	}
	var d *Description
	err := s.use(func(h handle) error {
		raw, err := diskDescribe(h, bsd)
		if err != nil {
			return err
		}
		d = parseDescription(raw)
		// The dictionary carries the BSD name for real media but omits it
		// for some virtual devices. The name the caller asked about is
		// always the right answer, so it is filled in rather than left
		// empty for the caller to wonder about.
		if d.BSDName == "" {
			d.BSDName = bsd
		}
		return nil
	})
	return d, err
}

// DescribeAll describes every disk [Session.Disks] finds, in the same order.
//
// A disk that goes away between the listing and its description is SKIPPED, not
// reported as an error. Enumerating a set of removable devices is inherently
// racy, and a caller asking "what is here" is better served by the four disks
// that answered than by an error about the fifth that left.
func (s *Session) DescribeAll() ([]*Description, error) {
	names, err := s.Disks()
	if err != nil {
		return nil, err
	}
	out := make([]*Description, 0, len(names))
	for _, n := range names {
		d, err := s.Describe(n)
		if err != nil {
			if errors.Is(err, ErrNoDescription) || errors.Is(err, ErrNoDisk) {
				continue
			}
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// Mounts returns the descriptions of the volumes that are currently mounted,
// which is the question a caller who just attached a disk image is actually
// asking.
func (s *Session) Mounts() ([]*Description, error) {
	all, err := s.DescribeAll()
	if err != nil {
		return nil, err
	}
	out := make([]*Description, 0, len(all))
	for _, d := range all {
		if d.Mounted() {
			out = append(out, d)
		}
	}
	return out, nil
}

// Unmount unmounts the volume on the named disk and blocks until the daemon
// answers. A refusal comes back as a [*DiskError] carrying the [Return] and the
// daemon's own sentence; [ReturnBusy] is the common one and [Return.Advice]
// says what to do about it.
//
// Pass [UnmountWhole] to take down every volume of the whole disk — the right
// option before ejecting a multi-partition image — and [UnmountForce] only
// where losing another process's unwritten data is acceptable.
func (s *Session) Unmount(name string, opts UnmountOptions) error {
	bsd := strings.TrimPrefix(name, "/dev/")
	if !ValidName(bsd) {
		return fmt.Errorf("%w: %q", ErrBadName, name)
	}
	return s.use(func(h handle) error { return diskUnmount(h, bsd, uint32(opts)) })
}

// Eject ejects the named disk and blocks until the daemon answers.
//
// Ejecting is not unmounting. The media must already be unmounted — eject a
// mounted volume and the daemon answers [ReturnBusy] — so the sequence for a
// disk image or a removable drive is Unmount with [UnmountWhole], then Eject
// the whole disk.
func (s *Session) Eject(name string) error {
	bsd := strings.TrimPrefix(name, "/dev/")
	if !ValidName(bsd) {
		return fmt.Errorf("%w: %q", ErrBadName, name)
	}
	return s.use(func(h handle) error { return diskEject(h, bsd) })
}

// Watcher is a live registration made by [Session.Watch]. Stop it when done;
// [Session.Close] stops any that are left.
type Watcher struct {
	once sync.Once
	stop func()
}

// Stop unregisters the callbacks. It is idempotent.
func (w *Watcher) Stop() { w.once.Do(w.stop) }

// Watch delivers an [Event] to fn each time a disk appears or disappears.
//
// Registration REPLAYS an [Appeared] event for every disk already present, so a
// caller that wants "what is here, and then what changes" needs only this — no
// separate enumeration, and no window between the two in which a disk could
// slip through unseen.
//
// fn is called ON THE RUN-LOOP THREAD. Everything else this session does
// asynchronously — the completion of an [Session.Unmount], every other event —
// waits behind it. Send to a channel and return; do not do work there, and
// above all do not call back into this session.
func (s *Session) Watch(fn func(Event)) (*Watcher, error) {
	if fn == nil {
		return nil, errors.New("diskarbitration: Watch needs a callback")
	}
	var w *Watcher
	err := s.use(func(h handle) error {
		emit := func(kind EventKind, bsd string, raw map[string]any) {
			ev := Event{Kind: kind, BSDName: bsd}
			if raw != nil {
				ev.Description = parseDescription(raw)
				if ev.Description.BSDName == "" {
					ev.Description.BSDName = bsd
				}
			}
			fn(ev)
		}
		stop, err := diskWatch(h, emit)
		if err != nil {
			return err
		}
		w = &Watcher{stop: stop}
		s.watches = append(s.watches, w.Stop)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return w, nil
}
