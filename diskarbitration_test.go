// Copyright (c) 2026, the go-macos authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

package diskarbitration

import (
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// The whole of this file runs on every platform and every architecture. It
// never touches DiskArbitration: the seams are swapped for fakes, so the
// portable half — the name grammar, the ordering, the description typing, the
// status vocabulary and the session guards — is exercised identically on macOS,
// on a Linux runner and under qemu on an architecture Apple never shipped.
//
// Nothing here unmounts or ejects anything. There is no code path from this
// file to a real disk.

// ---------------------------------------------------------------------------
// Seam harness.
// ---------------------------------------------------------------------------

// fakeSeams replaces every seam for the duration of a test and restores the
// real ones afterwards, so a failing test cannot leave the package rewired for
// the next one.
type fakeSeams struct {
	open     func() (handle, error)
	closed   []handle
	describe func(handle, string) (map[string]any, error)
	unmount  func(handle, string, uint32) error
	eject    func(handle, string) error
	watch    func(handle, func(EventKind, string, map[string]any)) (func(), error)
}

func installSeams(t *testing.T, f *fakeSeams) *fakeSeams {
	t.Helper()
	oldOpen, oldClose := sessionOpen, sessionClose
	oldDesc, oldUn, oldEj, oldW := diskDescribe, diskUnmount, diskEject, diskWatch
	t.Cleanup(func() {
		sessionOpen, sessionClose = oldOpen, oldClose
		diskDescribe, diskUnmount, diskEject, diskWatch = oldDesc, oldUn, oldEj, oldW
	})

	if f.open == nil {
		f.open = func() (handle, error) { return 7, nil }
	}
	if f.describe == nil {
		f.describe = func(handle, string) (map[string]any, error) { return map[string]any{}, nil }
	}
	if f.unmount == nil {
		f.unmount = func(handle, string, uint32) error { return nil }
	}
	if f.eject == nil {
		f.eject = func(handle, string) error { return nil }
	}
	if f.watch == nil {
		f.watch = func(handle, func(EventKind, string, map[string]any)) (func(), error) {
			return func() {}, nil
		}
	}
	sessionOpen = f.open
	sessionClose = func(h handle) { f.closed = append(f.closed, h) }
	diskDescribe = f.describe
	diskUnmount = f.unmount
	diskEject = f.eject
	diskWatch = f.watch
	return f
}

// fakeEntry is the minimum fs.DirEntry a /dev listing needs.
type fakeEntry struct{ name string }

func (e fakeEntry) Name() string               { return e.name }
func (e fakeEntry) IsDir() bool                { return false }
func (e fakeEntry) Type() fs.FileMode          { return fs.ModeDevice }
func (e fakeEntry) Info() (fs.FileInfo, error) { return nil, nil }

// installDev points the /dev scan at a fixed list of names.
func installDev(t *testing.T, names []string, err error) {
	t.Helper()
	old := readDirFn
	t.Cleanup(func() { readDirFn = old })
	readDirFn = func(string) ([]fs.DirEntry, error) {
		if err != nil {
			return nil, err
		}
		out := make([]fs.DirEntry, len(names))
		for i, n := range names {
			out[i] = fakeEntry{n}
		}
		return out, nil
	}
}

// openFake opens a session over the installed fakes.
func openFake(t *testing.T) *Session {
	t.Helper()
	s, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// ---------------------------------------------------------------------------
// BSD names.
// ---------------------------------------------------------------------------

func TestParseBSDName(t *testing.T) {
	cases := []struct {
		name string
		want []int
		ok   bool
	}{
		{"disk0", []int{0}, true},
		{"disk10", []int{10}, true},
		{"disk0s1", []int{0, 1}, true},
		{"disk3s1s1", []int{3, 1, 1}, true},
		{"disk12s34s56s78", []int{12, 34, 56, 78}, true},

		{"", nil, false},
		{"disk", nil, false},
		{"disks1", nil, false},
		{"rdisk0", nil, false},
		{"sda1", nil, false},
		{"disk0s", nil, false},
		{"disk0x1", nil, false},
		{"disk0s1x", nil, false},
		{"disk0ss1", nil, false},
		{"disk1\x00s9", nil, false},
		{"Disk0", nil, false},
		{"disk-1", nil, false},
		// An integer wider than the platform's int: strconv refuses it and
		// so must this, rather than wrapping into a plausible unit number.
		{"disk99999999999999999999999", nil, false},
	}
	for _, c := range cases {
		got, ok := parseBSDName(c.name)
		if ok != c.ok {
			t.Errorf("parseBSDName(%q) ok = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if ok && !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseBSDName(%q) = %v, want %v", c.name, got, c.want)
		}
		if ValidName(c.name) != c.ok {
			t.Errorf("ValidName(%q) = %v, want %v", c.name, !c.ok, c.ok)
		}
	}
}

func TestLessBSDName(t *testing.T) {
	cases := []struct {
		a, b []int
		want bool
	}{
		{[]int{0}, []int{1}, true},
		{[]int{2}, []int{10}, true}, // the whole point: not lexical
		{[]int{10}, []int{2}, false},
		{[]int{3}, []int{3, 1}, true},
		{[]int{3, 1}, []int{3}, false},
		{[]int{3, 1}, []int{3, 1, 1}, true},
		{[]int{3, 2}, []int{3, 1, 1}, false},
		{[]int{3, 1}, []int{3, 1}, false},
	}
	for _, c := range cases {
		if got := lessBSDName(c.a, c.b); got != c.want {
			t.Errorf("lessBSDName(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestSessionDisks(t *testing.T) {
	installSeams(t, &fakeSeams{})
	installDev(t, []string{
		"disk10", "rdisk0", "console", "disk2s1", "disk0", "null",
		"disk3s1s1", "disk2", "disk0s1", "disk3s1", "tty", "disk3",
	}, nil)

	s := openFake(t)
	defer func() { _ = s.Close() }()

	got, err := s.Disks()
	if err != nil {
		t.Fatalf("Disks: %v", err)
	}
	want := []string{"disk0", "disk0s1", "disk2", "disk2s1", "disk3", "disk3s1", "disk3s1s1", "disk10"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Disks() = %v\nwant %v", got, want)
	}
}

func TestSessionDisksEmpty(t *testing.T) {
	installSeams(t, &fakeSeams{})
	installDev(t, []string{"console", "null"}, nil)
	s := openFake(t)
	defer func() { _ = s.Close() }()

	got, err := s.Disks()
	if err != nil {
		t.Fatalf("Disks: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Disks() = %v, want empty", got)
	}
}

func TestSessionDisksReadError(t *testing.T) {
	installSeams(t, &fakeSeams{})
	boom := errors.New("boom")
	installDev(t, nil, boom)
	s := openFake(t)
	defer func() { _ = s.Close() }()

	_, err := s.Disks()
	if !errors.Is(err, boom) {
		t.Fatalf("Disks err = %v, want it to wrap %v", err, boom)
	}
	if !strings.Contains(err.Error(), devDir) {
		t.Errorf("Disks err = %q, want it to name %q", err, devDir)
	}
}

// ---------------------------------------------------------------------------
// Return.
// ---------------------------------------------------------------------------

func TestReturnString(t *testing.T) {
	cases := []struct {
		r    Return
		want string
	}{
		{ReturnSuccess, "success"},
		{ReturnError, "error"},
		{ReturnBusy, "busy"},
		{ReturnBadArgument, "badArgument"},
		{ReturnExclusiveAccess, "exclusiveAccess"},
		{ReturnNoResources, "noResources"},
		{ReturnNotFound, "notFound"},
		{ReturnNotMounted, "notMounted"},
		{ReturnNotPermitted, "notPermitted"},
		{ReturnNotPrivileged, "notPrivileged"},
		{ReturnNotReady, "notReady"},
		{ReturnNotWritable, "notWritable"},
		{ReturnUnsupported, "unsupported"},
		{0xF8DA00FF, "Return(0xF8DA00FF)"},
		// unix_err(EBUSY): the value a real refusal carries.
		{0x0000C010, "EBUSY"},
		{0x0000C001, "EPERM"},
		{0x0000C00D, "EACCES"},
		{0x0000C016, "EINVAL"},
		{0x0000C01E, "EROFS"},
		{0x0000C0FF, "errno 255"},
	}
	for _, c := range cases {
		if got := c.r.String(); got != c.want {
			t.Errorf("Return(0x%08X).String() = %q, want %q", uint32(c.r), got, c.want)
		}
	}
}

func TestReturnErrno(t *testing.T) {
	if e, ok := Return(0x0000C010).Errno(); !ok || e != 16 {
		t.Errorf("Errno of unix_err(EBUSY) = %d, %v; want 16, true", e, ok)
	}
	if e, ok := Return(0x0000C000).Errno(); !ok || e != 0 {
		t.Errorf("Errno of unix_err(0) = %d, %v; want 0, true", e, ok)
	}
	for _, r := range []Return{ReturnSuccess, ReturnBusy, ReturnUnsupported, 0x00008010} {
		if e, ok := r.Errno(); ok {
			t.Errorf("Return(0x%08X).Errno() = %d, true; want false", uint32(r), e)
		}
	}
}

func TestReturnAdvice(t *testing.T) {
	cases := []struct {
		r    Return
		want string // a distinctive fragment, or "" for no advice
	}{
		{ReturnBusy, "still has the volume open"},
		{ReturnNotPrivileged, "refused this to an unprivileged process"},
		{ReturnNotPermitted, "refused this to an unprivileged process"},
		{ReturnNotMounted, "not mounted"},
		{ReturnExclusiveAccess, "exclusively"},
		{ReturnSuccess, ""},
		{ReturnNotWritable, ""},
		{0x0000C010, "still has the volume open"},         // EBUSY
		{0x0000C001, "refused this to an unprivileged"},   // EPERM
		{0x0000C00D, "refused this to an unprivileged"},   // EACCES
		{0x0000C016, "rejected the request as malformed"}, // EINVAL
		{0x0000C01E, "read-only"},                         // EROFS
		{0x0000C005, ""},                                  // EIO: nothing useful to say
	}
	for _, c := range cases {
		got := c.r.Advice()
		if c.want == "" {
			if got != "" {
				t.Errorf("Return(0x%08X).Advice() = %q, want empty", uint32(c.r), got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("Return(0x%08X).Advice() = %q, want it to contain %q", uint32(c.r), got, c.want)
		}
	}
}

func TestDiskError(t *testing.T) {
	if err := newDiskError("unmount", "disk4s1", uint32(ReturnSuccess), ""); err != nil {
		t.Fatalf("a successful status must not be an error, got %v", err)
	}

	err := newDiskError("unmount", "disk4s1", 0x0000C010, "")
	var de *DiskError
	if !errors.As(err, &de) {
		t.Fatalf("want a *DiskError, got %T", err)
	}
	if de.Op != "unmount" || de.Disk != "disk4s1" || de.Status != 0x0000C010 {
		t.Errorf("unexpected fields: %+v", de)
	}
	if want := "diskarbitration: unmount disk4s1: EBUSY"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err, want)
	}
	if !strings.Contains(de.Advice(), "still has the volume open") {
		t.Errorf("Advice() = %q", de.Advice())
	}

	withMsg := newDiskError("eject", "disk4", uint32(ReturnBusy), "the volume is in use")
	if want := "diskarbitration: eject disk4: the volume is in use (busy)"; withMsg.Error() != want {
		t.Errorf("Error() = %q, want %q", withMsg, want)
	}
}

// ---------------------------------------------------------------------------
// Description.
// ---------------------------------------------------------------------------

// liveDisk3s5 is a description dictionary read verbatim off a live machine
// (macOS 26.6.2, arm64) with cmd/dalist -l, so the typing below is checked
// against what macOS really sends rather than against what a header suggests.
func liveDisk3s5() map[string]any {
	return map[string]any{
		"DAAppearanceTime":        8.09617871764205e+08,
		"DABusName":               "AppleANS3CGv2Controller",
		"DABusPath":               "IODeviceTree:/arm-io@10F00000/ans@9600000/iop-ans-nub/AppleANS3CGv2Controller",
		"DADeviceInternal":        true,
		"DADeviceModel":           "APPLE SSD AP4096Z",
		"DADevicePath":            "IOService:/AppleARMPE/arm-io@10F00000/…/NS_01@1",
		"DADeviceProtocol":        "Apple Fabric",
		"DADeviceRevision":        "2973.120",
		"DADeviceUnit":            int64(1),
		"DADeviceVendor":          "",
		"DAMediaBSDMajor":         int64(1),
		"DAMediaBSDMinor":         int64(18),
		"DAMediaBSDName":          "disk3s5",
		"DAMediaBlockSize":        int64(4096),
		"DAMediaContent":          "41504653-0000-11AA-AA11-00306543ECAC",
		"DAMediaEjectable":        false,
		"DAMediaEncrypted":        true,
		"DAMediaEncryptionDetail": int64(2),
		"DAMediaIcon":             "{\n    CFBundleIdentifier = \"com.apple.iokit.IOStorageFamily\";\n}",
		"DAMediaKind":             "IOMedia",
		"DAMediaLeaf":             true,
		"DAMediaName":             "Data",
		"DAMediaPath":             "IOService:/AppleARMPE/…/Data@5",
		"DAMediaRemovable":        false,
		"DAMediaSize":             int64(3996329328640),
		"DAMediaUUID":             "8999B3B6-F758-42AF-AFF5-8E5EF79CD14C",
		"DAMediaWhole":            false,
		"DAMediaWritable":         true,
		"DAVolumeKind":            "apfs",
		"DAVolumeMountable":       true,
		"DAVolumeName":            "Data",
		"DAVolumeNetwork":         false,
		"DAVolumePath":            "/System/Volumes/Data",
		"DAVolumeType":            "APFS (Encrypted)",
		"DAVolumeUUID":            "8999B3B6-F758-42AF-AFF5-8E5EF79CD14C",
	}
}

func TestParseDescriptionLive(t *testing.T) {
	d := parseDescription(liveDisk3s5())

	if d.BSDName != "disk3s5" || d.MediaName != "Data" {
		t.Errorf("names: %q / %q", d.BSDName, d.MediaName)
	}
	if d.MediaSize != 3996329328640 || d.BlockSize != 4096 {
		t.Errorf("sizes: %d / %d", d.MediaSize, d.BlockSize)
	}
	if d.Whole || !d.Leaf || d.Removable || d.Ejectable || !d.Writable || !d.Encrypted {
		t.Errorf("media flags: %+v", d)
	}
	if !d.Internal || d.Protocol != "Apple Fabric" || d.Model != "APPLE SSD AP4096Z" {
		t.Errorf("device: %+v", d)
	}
	if d.Vendor != "" || d.Revision != "2973.120" {
		t.Errorf("device strings: %q / %q", d.Vendor, d.Revision)
	}
	if d.VolumePath != "/System/Volumes/Data" || d.VolumeName != "Data" || d.VolumeKind != "apfs" {
		t.Errorf("volume: %+v", d)
	}
	if !d.VolumeMountable || d.VolumeNetwork {
		t.Errorf("volume flags: %+v", d)
	}
	if d.VolumeUUID != "8999B3B6-F758-42AF-AFF5-8E5EF79CD14C" {
		t.Errorf("uuid: %q", d.VolumeUUID)
	}
	if d.BusName != "AppleANS3CGv2Controller" || !strings.HasPrefix(d.BusPath, "IODeviceTree:") {
		t.Errorf("bus: %+v", d)
	}
	if !strings.HasPrefix(d.MediaPath, "IOService:") || !strings.HasPrefix(d.DevicePath, "IOService:") {
		t.Errorf("paths: %+v", d)
	}
	if d.Content != "41504653-0000-11AA-AA11-00306543ECAC" {
		t.Errorf("content: %q", d.Content)
	}
	if !d.Mounted() {
		t.Error("Mounted() = false for a volume with a mount point")
	}
	if d.IsDiskImage() {
		t.Error("IsDiskImage() = true for an internal SSD")
	}
	if d.Device() != "/dev/disk3s5" {
		t.Errorf("Device() = %q", d.Device())
	}
	if len(d.Raw) != len(liveDisk3s5()) {
		t.Errorf("Raw dropped keys: %d of %d", len(d.Raw), len(liveDisk3s5()))
	}
}

func TestParseDescriptionEmpty(t *testing.T) {
	d := parseDescription(map[string]any{})
	if d.Mounted() || d.IsDiskImage() || d.Device() != "" {
		t.Errorf("an empty description should be entirely zero: %+v", d)
	}
	if d.String() != "(no device)" {
		t.Errorf("String() = %q", d.String())
	}
}

func TestParseDescriptionWrongTypes(t *testing.T) {
	// Every typed field fed a value of the wrong CoreFoundation type. None
	// of it may panic: DAMediaIcon really does arrive as a dictionary, and a
	// future key could arrive as anything at all.
	d := parseDescription(map[string]any{
		KeyMediaBSDName:   int64(7),
		KeyMediaSize:      "not a number",
		KeyMediaWhole:     "yes",
		KeyVolumePath:     []byte{1, 2, 3},
		KeyDeviceInternal: int64(1),
		KeyMediaBlockSize: []byte{4},
	})
	if d.BSDName != "" || d.MediaSize != 0 || d.Whole || d.VolumePath != "" || d.Internal || d.BlockSize != 0 {
		t.Errorf("wrongly-typed values must degrade to the zero value: %+v", d)
	}
}

func TestDictInt(t *testing.T) {
	m := map[string]any{
		"i64":     int64(5),
		"int":     7,
		"float":   9.9,
		"string":  "11",
		"missing": nil,
	}
	for k, want := range map[string]int64{"i64": 5, "int": 7, "float": 9, "string": 0, "absent": 0} {
		if got := dictInt(m, k); got != want {
			t.Errorf("dictInt(%q) = %d, want %d", k, got, want)
		}
	}
}

func TestDescriptionIsDiskImage(t *testing.T) {
	// Measured: current macOS marks an attached image with the MODEL, older
	// releases with the protocol. Both must be recognised.
	cases := []struct {
		model, protocol string
		want            bool
	}{
		{ModelDiskImage, ProtocolVirtualInterface, true}, // macOS 26.6.2, measured
		{"", ModelDiskImage, true},                       // the historical spelling
		{"", ProtocolVirtualInterface, true},
		{"APPLE SSD AP4096Z", "Apple Fabric", false},
		{"", "", false},
	}
	for _, c := range cases {
		d := &Description{Model: c.model, Protocol: c.protocol}
		if got := d.IsDiskImage(); got != c.want {
			t.Errorf("IsDiskImage(model=%q protocol=%q) = %v, want %v", c.model, c.protocol, got, c.want)
		}
	}
}

func TestDescriptionString(t *testing.T) {
	full := &Description{
		BSDName: "disk4s1", MediaSize: 33513472, VolumeName: "DAScratch",
		VolumeKind: "hfs", VolumePath: "/Volumes/DAScratch",
		Removable: true, Ejectable: true, Writable: true, Protocol: ProtocolVirtualInterface,
	}
	want := `disk4s1 33.5 MB "DAScratch" [hfs] on /Volumes/DAScratch (removable, ejectable, Virtual Interface)`
	if got := full.String(); got != want {
		t.Errorf("String() =\n  %s\nwant\n  %s", got, want)
	}

	whole := &Description{BSDName: "disk0", MediaSize: 4002222325760, Whole: true, Writable: true, Encrypted: true}
	if got, want := whole.String(), "disk0 4.0 TB (whole, encrypted)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	// "read-only" appears only when macOS said the media is not writable —
	// not merely because the field is false.
	ro := &Description{BSDName: "disk3s1s1", Raw: map[string]any{KeyMediaWritable: false}}
	if got, want := ro.String(), "disk3s1s1 (read-only)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	silent := &Description{BSDName: "disk3s1s1"}
	if got, want := silent.String(), "disk3s1s1"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1000, "1.0 kB"},
		{33513472, "33.5 MB"},
		{4002222325760, "4.0 TB"},
		// Decimal units, deliberately: macOS calls a 4002222325760-byte
		// disk 4 TB, and a binding that said 3.6 TiB would disagree with
		// every other window on the machine.
		{5_000_000_000_000_000, "5.0 PB"},
		{9_000_000_000_000_000_000, "9000.0 PB"},
	}
	for _, c := range cases {
		if got := humanSize(c.n); got != c.want {
			t.Errorf("humanSize(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Events and options.
// ---------------------------------------------------------------------------

func TestEventKindString(t *testing.T) {
	for k, want := range map[EventKind]string{
		Appeared: "appeared", Disappeared: "disappeared", EventKind(9): "EventKind(9)",
	} {
		if got := k.String(); got != want {
			t.Errorf("EventKind(%d).String() = %q, want %q", int(k), got, want)
		}
	}
}

func TestEventString(t *testing.T) {
	cases := []struct {
		e    Event
		want string
	}{
		{Event{Kind: Appeared, BSDName: "disk4"}, "appeared disk4"},
		{Event{Kind: Disappeared, BSDName: "disk4s1"}, "disappeared disk4s1"},
		{Event{Kind: Appeared}, "appeared (no device)"},
		{Event{Kind: Appeared, BSDName: "disk4s1", Description: &Description{VolumeName: "DAScratch"}},
			`appeared disk4s1 "DAScratch"`},
		{Event{Kind: Appeared, BSDName: "disk4", Description: &Description{}}, "appeared disk4"},
	}
	for _, c := range cases {
		if got := c.e.String(); got != c.want {
			t.Errorf("Event.String() = %q, want %q", got, c.want)
		}
	}
}

func TestUnmountOptionsString(t *testing.T) {
	for o, want := range map[UnmountOptions]string{
		UnmountDefault:              "default",
		UnmountWhole:                "whole",
		UnmountForce:                "force",
		UnmountWhole | UnmountForce: "whole|force",
	} {
		if got := o.String(); got != want {
			t.Errorf("UnmountOptions(0x%X).String() = %q, want %q", uint32(o), got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Session.
// ---------------------------------------------------------------------------

func TestOpenFailure(t *testing.T) {
	boom := errors.New("no daemon")
	installSeams(t, &fakeSeams{open: func() (handle, error) { return 0, boom }})
	if _, err := Open(); !errors.Is(err, boom) {
		t.Fatalf("Open err = %v, want %v", err, boom)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	f := installSeams(t, &fakeSeams{})
	s := openFake(t)
	for i := 0; i < 3; i++ {
		if err := s.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i, err)
		}
	}
	if len(f.closed) != 1 {
		t.Errorf("the platform seam was closed %d times, want exactly 1", len(f.closed))
	}
}

func TestUseAfterClose(t *testing.T) {
	installSeams(t, &fakeSeams{})
	installDev(t, []string{"disk0"}, nil)
	s := openFake(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Disks(); !errors.Is(err, ErrClosed) {
		t.Errorf("Disks after Close = %v, want ErrClosed", err)
	}
	if _, err := s.Describe("disk0"); !errors.Is(err, ErrClosed) {
		t.Errorf("Describe after Close = %v, want ErrClosed", err)
	}
	if _, err := s.DescribeAll(); !errors.Is(err, ErrClosed) {
		t.Errorf("DescribeAll after Close = %v, want ErrClosed", err)
	}
	if _, err := s.Mounts(); !errors.Is(err, ErrClosed) {
		t.Errorf("Mounts after Close = %v, want ErrClosed", err)
	}
	if err := s.Unmount("disk0", UnmountDefault); !errors.Is(err, ErrClosed) {
		t.Errorf("Unmount after Close = %v, want ErrClosed", err)
	}
	if err := s.Eject("disk0"); !errors.Is(err, ErrClosed) {
		t.Errorf("Eject after Close = %v, want ErrClosed", err)
	}
	if _, err := s.Watch(func(Event) {}); !errors.Is(err, ErrClosed) {
		t.Errorf("Watch after Close = %v, want ErrClosed", err)
	}
}

func TestDescribe(t *testing.T) {
	var asked []string
	installSeams(t, &fakeSeams{
		describe: func(_ handle, bsd string) (map[string]any, error) {
			asked = append(asked, bsd)
			return liveDisk3s5(), nil
		},
	})
	s := openFake(t)
	defer func() { _ = s.Close() }()

	// The /dev/ prefix is accepted, because that is the form a caller has
	// after reading a mount table.
	for _, name := range []string{"disk3s5", "/dev/disk3s5"} {
		d, err := s.Describe(name)
		if err != nil {
			t.Fatalf("Describe(%q): %v", name, err)
		}
		if d.VolumePath != "/System/Volumes/Data" {
			t.Errorf("Describe(%q) mount point = %q", name, d.VolumePath)
		}
	}
	if !reflect.DeepEqual(asked, []string{"disk3s5", "disk3s5"}) {
		t.Errorf("the platform seam was asked for %v", asked)
	}
}

func TestDescribeFillsMissingBSDName(t *testing.T) {
	// Some virtual devices come back with no DAMediaBSDName. The name the
	// caller asked about is the right answer, not "".
	installSeams(t, &fakeSeams{
		describe: func(handle, string) (map[string]any, error) {
			return map[string]any{KeyVolumeName: "somewhere"}, nil
		},
	})
	s := openFake(t)
	defer func() { _ = s.Close() }()

	d, err := s.Describe("disk9")
	if err != nil {
		t.Fatal(err)
	}
	if d.BSDName != "disk9" {
		t.Errorf("BSDName = %q, want disk9", d.BSDName)
	}
}

func TestDescribeBadName(t *testing.T) {
	installSeams(t, &fakeSeams{
		describe: func(handle, string) (map[string]any, error) {
			t.Error("the platform seam must not be reached for a bad name")
			return nil, nil
		},
	})
	s := openFake(t)
	defer func() { _ = s.Close() }()

	for _, name := range []string{"", "sda1", "/dev/sda1", "disk1\x00s9", "../etc/passwd"} {
		if _, err := s.Describe(name); !errors.Is(err, ErrBadName) {
			t.Errorf("Describe(%q) = %v, want ErrBadName", name, err)
		}
		if err := s.Unmount(name, UnmountDefault); !errors.Is(err, ErrBadName) {
			t.Errorf("Unmount(%q) = %v, want ErrBadName", name, err)
		}
		if err := s.Eject(name); !errors.Is(err, ErrBadName) {
			t.Errorf("Eject(%q) = %v, want ErrBadName", name, err)
		}
	}
}

func TestDescribeError(t *testing.T) {
	installSeams(t, &fakeSeams{
		describe: func(handle, string) (map[string]any, error) { return nil, ErrNoDescription },
	})
	s := openFake(t)
	defer func() { _ = s.Close() }()

	if _, err := s.Describe("disk9"); !errors.Is(err, ErrNoDescription) {
		t.Errorf("Describe = %v, want ErrNoDescription", err)
	}
}

func TestDescribeAllSkipsDisksThatWentAway(t *testing.T) {
	installDev(t, []string{"disk0", "disk1", "disk2", "disk3"}, nil)
	installSeams(t, &fakeSeams{
		describe: func(_ handle, bsd string) (map[string]any, error) {
			switch bsd {
			case "disk1":
				return nil, fmt.Errorf("%w: %s", ErrNoDescription, bsd)
			case "disk2":
				return nil, fmt.Errorf("%w: %s", ErrNoDisk, bsd)
			}
			return map[string]any{KeyMediaBSDName: bsd, KeyVolumePath: "/mnt/" + bsd}, nil
		},
	})
	s := openFake(t)
	defer func() { _ = s.Close() }()

	all, err := s.DescribeAll()
	if err != nil {
		t.Fatalf("DescribeAll: %v", err)
	}
	var got []string
	for _, d := range all {
		got = append(got, d.BSDName)
	}
	if !reflect.DeepEqual(got, []string{"disk0", "disk3"}) {
		t.Errorf("DescribeAll = %v, want the two that answered", got)
	}
}

func TestDescribeAllPropagatesRealErrors(t *testing.T) {
	installDev(t, []string{"disk0"}, nil)
	boom := errors.New("the daemon died")
	installSeams(t, &fakeSeams{
		describe: func(handle, string) (map[string]any, error) { return nil, boom },
	})
	s := openFake(t)
	defer func() { _ = s.Close() }()

	if _, err := s.DescribeAll(); !errors.Is(err, boom) {
		t.Fatalf("DescribeAll = %v, want %v", err, boom)
	}
	if _, err := s.Mounts(); !errors.Is(err, boom) {
		t.Fatalf("Mounts = %v, want %v", err, boom)
	}
}

func TestDescribeAllListError(t *testing.T) {
	boom := errors.New("no /dev")
	installDev(t, nil, boom)
	installSeams(t, &fakeSeams{})
	s := openFake(t)
	defer func() { _ = s.Close() }()

	if _, err := s.DescribeAll(); !errors.Is(err, boom) {
		t.Fatalf("DescribeAll = %v, want %v", err, boom)
	}
	if _, err := s.Mounts(); !errors.Is(err, boom) {
		t.Fatalf("Mounts = %v, want %v", err, boom)
	}
}

func TestMounts(t *testing.T) {
	installDev(t, []string{"disk0", "disk0s1", "disk0s2"}, nil)
	installSeams(t, &fakeSeams{
		describe: func(_ handle, bsd string) (map[string]any, error) {
			m := map[string]any{KeyMediaBSDName: bsd}
			if bsd == "disk0s2" {
				m[KeyVolumePath] = "/Volumes/Thing"
			}
			return m, nil
		},
	})
	s := openFake(t)
	defer func() { _ = s.Close() }()

	mounted, err := s.Mounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(mounted) != 1 || mounted[0].BSDName != "disk0s2" {
		t.Fatalf("Mounts = %v, want just disk0s2", mounted)
	}
}

func TestUnmountAndEject(t *testing.T) {
	var gotOpts uint32
	var ejected string
	installSeams(t, &fakeSeams{
		unmount: func(_ handle, bsd string, opts uint32) error {
			if bsd != "disk4s1" {
				t.Errorf("unmount asked for %q", bsd)
			}
			gotOpts = opts
			return nil
		},
		eject: func(_ handle, bsd string) error { ejected = bsd; return nil },
	})
	s := openFake(t)
	defer func() { _ = s.Close() }()

	if err := s.Unmount("/dev/disk4s1", UnmountWhole|UnmountForce); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if gotOpts != uint32(UnmountWhole|UnmountForce) {
		t.Errorf("options reached the seam as 0x%X", gotOpts)
	}
	if err := s.Eject("/dev/disk4"); err != nil {
		t.Fatalf("Eject: %v", err)
	}
	if ejected != "disk4" {
		t.Errorf("eject asked for %q, want disk4", ejected)
	}
}

func TestUnmountAndEjectErrors(t *testing.T) {
	busy := newDiskError("unmount", "disk4s1", 0x0000C010, "")
	installSeams(t, &fakeSeams{
		unmount: func(handle, string, uint32) error { return busy },
		eject:   func(handle, string) error { return busy },
	})
	s := openFake(t)
	defer func() { _ = s.Close() }()

	if err := s.Unmount("disk4s1", UnmountDefault); !errors.Is(err, busy) {
		t.Errorf("Unmount = %v", err)
	}
	if err := s.Eject("disk4"); !errors.Is(err, busy) {
		t.Errorf("Eject = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Watching.
// ---------------------------------------------------------------------------

func TestWatch(t *testing.T) {
	var emit func(EventKind, string, map[string]any)
	stopped := 0
	installSeams(t, &fakeSeams{
		watch: func(_ handle, e func(EventKind, string, map[string]any)) (func(), error) {
			emit = e
			return func() { stopped++ }, nil
		},
	})
	s := openFake(t)
	defer func() { _ = s.Close() }()

	var mu sync.Mutex
	var got []Event
	w, err := s.Watch(func(e Event) { mu.Lock(); got = append(got, e); mu.Unlock() })
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	emit(Appeared, "disk4s1", map[string]any{KeyVolumeName: "DAScratch", KeyVolumePath: "/Volumes/DAScratch"})
	// A disappearance has no description at all: the media is gone.
	emit(Disappeared, "disk4s1", nil)
	// A device with no BSD name in its dictionary still gets the one the
	// callback reported.
	emit(Appeared, "disk9", map[string]any{})

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
	if got[0].Kind != Appeared || got[0].Description == nil || got[0].Description.VolumeName != "DAScratch" {
		t.Errorf("event 0 = %+v", got[0])
	}
	if got[0].Description.BSDName != "disk4s1" {
		t.Errorf("event 0 BSDName = %q", got[0].Description.BSDName)
	}
	if got[1].Kind != Disappeared || got[1].Description != nil {
		t.Errorf("event 1 = %+v", got[1])
	}
	if got[2].Description.BSDName != "disk9" {
		t.Errorf("event 2 BSDName = %q", got[2].Description.BSDName)
	}

	// Stop is idempotent, and Close does not stop an already-stopped watch a
	// second time.
	w.Stop()
	w.Stop()
	if stopped != 1 {
		t.Errorf("the registration was stopped %d times, want 1", stopped)
	}
}

func TestWatchStoppedByClose(t *testing.T) {
	stopped := 0
	installSeams(t, &fakeSeams{
		watch: func(handle, func(EventKind, string, map[string]any)) (func(), error) {
			return func() { stopped++ }, nil
		},
	})
	s := openFake(t)
	if _, err := s.Watch(func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Watch(func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if stopped != 2 {
		t.Errorf("Close stopped %d watches, want 2", stopped)
	}
}

func TestWatchNilCallback(t *testing.T) {
	installSeams(t, &fakeSeams{})
	s := openFake(t)
	defer func() { _ = s.Close() }()
	if _, err := s.Watch(nil); err == nil {
		t.Fatal("Watch(nil) must be refused")
	}
}

func TestWatchSeamError(t *testing.T) {
	boom := errors.New("cannot register")
	installSeams(t, &fakeSeams{
		watch: func(handle, func(EventKind, string, map[string]any)) (func(), error) {
			return nil, boom
		},
	})
	s := openFake(t)
	defer func() { _ = s.Close() }()
	if _, err := s.Watch(func(Event) {}); !errors.Is(err, boom) {
		t.Fatalf("Watch = %v, want %v", err, boom)
	}
}

// TestSessionIsConcurrencySafe drives every method from many goroutines at once
// against the fakes. It exists for the race detector, not for its assertions:
// a Session is documented as safe from any goroutine and that claim needs a
// lane that would notice if it stopped being true.
func TestSessionIsConcurrencySafe(t *testing.T) {
	installDev(t, []string{"disk0", "disk0s1"}, nil)
	installSeams(t, &fakeSeams{})
	s := openFake(t)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = s.Disks()
				_, _ = s.Describe("disk0")
				_, _ = s.DescribeAll()
				_ = s.Unmount("disk0s1", UnmountDefault)
				_ = s.Eject("disk0")
				if w, err := s.Watch(func(Event) {}); err == nil {
					w.Stop()
				}
			}
		}()
	}
	time.Sleep(time.Millisecond)
	_ = s.Close()
	wg.Wait()
}
