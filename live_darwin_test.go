// Copyright (c) 2026, the go-macos authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

//go:build darwin

package diskarbitration

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// The live suite. It talks to the REAL DiskArbitration daemon on the machine
// running the tests, because a binding that only ever meets its own fakes is a
// binding nobody has checked.
//
// ⛔ IT IS READ-ONLY ABOUT THIS MACHINE'S DISKS. Nothing here unmounts or
// ejects anything that exists. The two tests that exercise the write path aim
// at a BSD name they FIRST PROVE is not a device, and assert that the daemon
// refuses; if that name ever resolved to real media the test fails rather than
// proceeding. A test suite that unmounted the volume it was running from would
// be indefensible, and "it only does it on CI" is not a defence.

// liveSession opens a real session, or skips the test when the framework is
// unavailable (a sandbox with no access to the daemon).
func liveSession(t *testing.T) *Session {
	t.Helper()
	s, err := Open()
	if err != nil {
		t.Skipf("no DiskArbitration session on this machine: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestLiveDisks(t *testing.T) {
	s := liveSession(t)

	names, err := s.Disks()
	if err != nil {
		t.Fatalf("Disks: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("no block devices at all: a machine running this test has at least a boot disk")
	}
	t.Logf("%d devices: %v", len(names), names)

	// Every name the scan produced must be one the grammar accepts, and the
	// first must be a whole disk (the ordering puts diskN before diskNsM).
	for _, n := range names {
		if !ValidName(n) {
			t.Errorf("Disks returned %q, which ValidName rejects", n)
		}
	}
	if !strings.HasPrefix(names[0], "disk") {
		t.Errorf("first device is %q", names[0])
	}
}

func TestLiveDescribeAll(t *testing.T) {
	s := liveSession(t)

	all, err := s.DescribeAll()
	if err != nil {
		t.Fatalf("DescribeAll: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no descriptions at all")
	}

	var (
		anyWhole, anySize, anyKind, anyUUID bool
		root                                *Description
	)
	for _, d := range all {
		t.Log(d)
		if d.BSDName == "" {
			t.Errorf("a description came back with no BSD name: %+v", d)
		}
		if d.Device() != "/dev/"+d.BSDName {
			t.Errorf("Device() = %q for %q", d.Device(), d.BSDName)
		}
		if d.Whole {
			anyWhole = true
		}
		if d.MediaSize > 0 {
			anySize = true
		}
		if d.VolumeKind != "" {
			anyKind = true
		}
		if d.VolumeUUID != "" {
			anyUUID = true
		}
		if d.VolumePath == "/" {
			root = d
		}
		// Whole media and a slice of it are mutually exclusive claims; a
		// description asserting both would mean the boolean conversion has
		// gone wrong somewhere. The check is on the PARSED name, not on
		// whether the string contains an "s" — "disk" contains one.
		if nums, ok := parseBSDName(d.BSDName); ok && d.Whole && len(nums) > 1 {
			t.Errorf("%s says it is whole media but its name is a slice", d.BSDName)
		}
	}

	// These four assert that the CoreFoundation value conversion actually
	// works for each type the dictionary carries: CFBoolean, CFNumber,
	// CFString and CFUUID. Every macOS install has all four somewhere in
	// this list.
	if !anyWhole {
		t.Error("no whole disk: the CFBoolean conversion is suspect")
	}
	if !anySize {
		t.Error("no media size anywhere: the CFNumber conversion is suspect")
	}
	if !anyKind {
		t.Error("no volume kind anywhere: the CFString conversion is suspect")
	}
	if !anyUUID {
		t.Error("no volume UUID anywhere: the CFUUID conversion is suspect")
	}

	// The root volume is mounted at "/" and DAVolumePath is a CFURL, so
	// finding it proves the CFURL conversion too.
	if root == nil {
		t.Fatal("no volume is mounted at /: the CFURL conversion is suspect")
	}
	t.Logf("root volume: %s (kind=%q name=%q size=%d)", root.BSDName, root.VolumeKind, root.VolumeName, root.MediaSize)
	if !root.Mounted() || !root.VolumeMountable || root.VolumeKind == "" || root.MediaSize <= 0 {
		t.Errorf("the root volume looks wrong: %+v", root)
	}
	if root.VolumeNetwork {
		t.Error("the root volume is reported as a network volume")
	}
}

func TestLiveMounts(t *testing.T) {
	s := liveSession(t)

	mounts, err := s.Mounts()
	if err != nil {
		t.Fatalf("Mounts: %v", err)
	}
	if len(mounts) == 0 {
		t.Fatal("nothing is mounted, which cannot be true of a running machine")
	}
	var atRoot bool
	for _, d := range mounts {
		t.Logf("%s on %s", d.BSDName, d.VolumePath)
		if !d.Mounted() {
			t.Errorf("Mounts returned an unmounted volume: %+v", d)
		}
		if d.VolumePath == "/" {
			atRoot = true
		}
	}
	if !atRoot {
		t.Error("no mount at /")
	}
}

func TestLiveDescribeRaw(t *testing.T) {
	s := liveSession(t)

	names, err := s.Disks()
	if err != nil {
		t.Fatalf("Disks: %v", err)
	}
	d, err := s.Describe(names[0])
	if err != nil {
		t.Fatalf("Describe(%s): %v", names[0], err)
	}
	if len(d.Raw) < 10 {
		t.Fatalf("a real description carries dozens of keys, got %d: %v", len(d.Raw), d.Raw)
	}
	// Every key must be a DA key: a stray "" would mean goString failed on a
	// dictionary key and the entry was silently kept.
	for k := range d.Raw {
		if !strings.HasPrefix(k, "DA") {
			t.Errorf("unexpected description key %q", k)
		}
	}
	// DAMediaIcon is a CFDictionary — the type the converter has no typed
	// case for. It must degrade to its description, not vanish and not
	// panic.
	if icon, ok := d.Raw["DAMediaIcon"]; ok {
		s, isString := icon.(string)
		if !isString || s == "" {
			t.Errorf("DAMediaIcon converted to %T %v, want a non-empty string", icon, icon)
		}
	}
}

func TestLiveDescribeNoSuchDevice(t *testing.T) {
	s := liveSession(t)

	// Creating the DADisk reference SUCCEEDS for a name with no device
	// behind it — the failure surfaces as a missing description. This is the
	// measured behaviour on macOS 26.6.2 and the reason ErrNoDisk is rare.
	_, err := s.Describe("disk98s76")
	if !errors.Is(err, ErrNoDescription) {
		t.Fatalf("Describe of an absent device = %v, want ErrNoDescription", err)
	}
}

func TestLiveDescribeBadName(t *testing.T) {
	s := liveSession(t)
	for _, name := range []string{"", "sda1", "disk0\x00s9", "/etc/passwd"} {
		if _, err := s.Describe(name); !errors.Is(err, ErrBadName) {
			t.Errorf("Describe(%q) = %v, want ErrBadName", name, err)
		}
	}
}

func TestLiveWatchReplaysWhatIsAlreadyThere(t *testing.T) {
	s := liveSession(t)

	var mu sync.Mutex
	seen := map[string]*Description{}
	kinds := map[EventKind]int{}
	w, err := s.Watch(func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		kinds[e.Kind]++
		if e.Kind == Appeared {
			seen[e.BSDName] = e.Description
		}
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Stop()

	// The replay is delivered on the run loop as soon as the registration
	// lands. A second is generous for twenty devices; the assertion below is
	// about completeness, not speed.
	deadline := time.Now().Add(3 * time.Second)
	want, err := s.Disks()
	if err != nil {
		t.Fatalf("Disks: %v", err)
	}
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n > len(want) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	t.Logf("%d appearance events for %d devices in /dev", kinds[Appeared], len(want))

	// The replay covers every device the /dev scan found. It may also carry
	// devices the scan cannot see — a mounted network volume arrives with no
	// BSD name at all — which is why this is a subset check and not an
	// equality one.
	for _, name := range want {
		d, ok := seen[name]
		if !ok {
			t.Errorf("the appearance replay never mentioned %s", name)
			continue
		}
		if d == nil {
			t.Errorf("%s appeared with no description", name)
			continue
		}
		if d.BSDName != name {
			t.Errorf("%s appeared describing itself as %q", name, d.BSDName)
		}
	}
}

func TestLiveWatchStopIsSafeAfterClose(t *testing.T) {
	// Stopping a watch whose session has already been closed must not reach
	// the released session. It is the ordering a deferred Stop next to a
	// deferred Close produces, so it is the ordering that will actually
	// happen in consumers.
	s, err := Open()
	if err != nil {
		t.Skipf("no session: %v", err)
	}
	w, err := s.Watch(func(Event) {})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	w.Stop()
}

// ghostDisk is a BSD name chosen to be absent. The write-path tests below prove
// it is absent before they touch it.
const ghostDisk = "disk98s76"

// requireAbsent fails the test unless there is genuinely no such device.
func requireAbsent(t *testing.T, s *Session, name string) {
	t.Helper()
	if d, err := s.Describe(name); err == nil {
		t.Fatalf("REFUSING to continue: %s is a real device (%s)", name, d)
	}
}

func TestLiveUnmountIsRefusedForAnAbsentDevice(t *testing.T) {
	s := liveSession(t)
	requireAbsent(t, s, ghostDisk)

	err := s.Unmount(ghostDisk, UnmountDefault)
	if err == nil {
		t.Fatal("unmounting a device that does not exist must not succeed")
	}
	var de *DiskError
	if !errors.As(err, &de) {
		t.Fatalf("want a *DiskError from the dissenter, got %T: %v", err, err)
	}
	t.Logf("unmount refused: status=%v (0x%08X) message=%q", de.Status, uint32(de.Status), de.Message)
	if de.Op != "unmount" || de.Disk != ghostDisk {
		t.Errorf("DiskError names the wrong operation: %+v", de)
	}
	if de.Status == ReturnSuccess {
		t.Error("a refusal carried kDAReturnSuccess")
	}
}

func TestLiveEjectIsRefusedForAnAbsentDevice(t *testing.T) {
	s := liveSession(t)
	requireAbsent(t, s, ghostDisk)

	err := s.Eject(ghostDisk)
	if err == nil {
		t.Fatal("ejecting a device that does not exist must not succeed")
	}
	var de *DiskError
	if !errors.As(err, &de) {
		t.Fatalf("want a *DiskError from the dissenter, got %T: %v", err, err)
	}
	t.Logf("eject refused: status=%v (0x%08X) message=%q", de.Status, uint32(de.Status), de.Message)
	if de.Op != "eject" {
		t.Errorf("DiskError names the wrong operation: %+v", de)
	}
}

func TestLiveManySessions(t *testing.T) {
	// Each session owns a run loop on its own OS thread, and the run-loop
	// class is registered once per process. Opening several at once is what
	// would break if that registration were per-session, and the Objective-C
	// runtime refuses a duplicate class name loudly enough to matter.
	var sessions []*Session
	for i := 0; i < 4; i++ {
		s, err := Open()
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		sessions = append(sessions, s)
	}
	for i, s := range sessions {
		if _, err := s.Disks(); err != nil {
			t.Errorf("session #%d cannot list: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Errorf("session #%d Close: %v", i, err)
		}
	}
}

func TestLiveUseAfterClose(t *testing.T) {
	s, err := Open()
	if err != nil {
		t.Skipf("no session: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// The guard is in the portable half, but this asserts it against a
	// session whose C object really has been released — the case where being
	// wrong is a crash rather than a failed assertion.
	if _, err := s.Describe("disk0"); !errors.Is(err, ErrClosed) {
		t.Errorf("Describe after Close = %v, want ErrClosed", err)
	}
}
