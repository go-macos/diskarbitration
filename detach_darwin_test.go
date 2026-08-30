// Copyright (c) 2026, the go-macos authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

//go:build darwin

package diskarbitration

import (
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The disappearance path, end to end.
//
// It is the one behaviour no amount of reading can verify: a disk has to
// actually go away. So this test MAKES ONE — a small disk image of its own, in
// the test's temporary directory — attaches it, watches it appear, detaches it
// and watches it disappear.
//
// ⛔ IT NEVER TOUCHES A DEVICE IT DID NOT CREATE. The BSD name it acts on comes
// from the attach it performed a moment earlier, the image is attached with
// -nomount so no volume of the machine's is involved at all, and the teardown
// detaches by that same name. Nothing here calls this package's own Unmount or
// Eject: the write path is exercised against refusals elsewhere, and a suite
// that ejected media to prove it could would eventually eject the wrong thing.
//
// hdiutil is used to attach and detach precisely BECAUSE it is not the code
// under test. The point is to observe DiskArbitration reporting a change this
// package did not make.

func TestLiveDiskAppearsAndDisappears(t *testing.T) {
	if _, err := exec.LookPath("hdiutil"); err != nil {
		t.Skip("hdiutil is not available")
	}

	img := filepath.Join(t.TempDir(), "diskarbitration-test.dmg")
	if out, err := exec.Command("hdiutil", "create",
		"-size", "8m", "-fs", "HFS+", "-volname", "DATest", "-ov", img).CombinedOutput(); err != nil {
		t.Skipf("cannot create a test image: %v\n%s", err, out)
	}

	s := liveSession(t)

	var mu sync.Mutex
	appeared := map[string]bool{}
	disappeared := map[string]bool{}
	w, err := s.Watch(func(e Event) {
		if e.BSDName == "" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch e.Kind {
		case Appeared:
			appeared[e.BSDName] = true
		case Disappeared:
			disappeared[e.BSDName] = true
		}
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Stop()

	// Let the replay of what is already present drain, then forget it: only
	// what happens AFTER this point is the subject.
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	appeared = map[string]bool{}
	mu.Unlock()

	// -nomount: the image's volume is never mounted, so nothing of this
	// machine's is disturbed and nothing appears in /Volumes.
	out, err := exec.Command("hdiutil", "attach", "-nomount", "-noverify", img).CombinedOutput()
	if err != nil {
		t.Skipf("cannot attach the test image: %v\n%s", err, out)
	}
	dev := strings.Fields(strings.Split(string(out), "\n")[0])[0] // /dev/diskN
	bsd := strings.TrimPrefix(dev, "/dev/")
	t.Logf("attached %s", dev)

	detached := false
	detach := func() {
		if detached {
			return
		}
		detached = true
		if out, err := exec.Command("hdiutil", "detach", dev, "-force").CombinedOutput(); err != nil {
			t.Logf("detach %s: %v\n%s", dev, err, out)
		}
	}
	defer detach()

	if !ValidName(bsd) {
		t.Fatalf("hdiutil reported %q, which is not a BSD disk name", dev)
	}

	// It appeared, and this package can describe it as a disk image.
	waitFor(t, &mu, func() bool { return appeared[bsd] }, "appearance of "+bsd)
	d, err := s.Describe(bsd)
	if err != nil {
		t.Fatalf("Describe(%s): %v", bsd, err)
	}
	t.Logf("described: %s", d)
	if !d.IsDiskImage() {
		t.Errorf("IsDiskImage() = false for an attached image (model=%q protocol=%q)", d.Model, d.Protocol)
	}
	if !d.Whole || !d.Ejectable || !d.Removable {
		t.Errorf("an attached whole image should be whole, ejectable and removable: %+v", d)
	}
	if d.Mounted() {
		t.Errorf("attached with -nomount but reports a mount point %q", d.VolumePath)
	}

	// And now it goes away. This is the only way to reach the disappearance
	// callback at all.
	detach()
	waitFor(t, &mu, func() bool { return disappeared[bsd] }, "disappearance of "+bsd)

	// The description goes with it.
	if _, err := s.Describe(bsd); err == nil {
		t.Errorf("Describe(%s) still succeeds after the image was detached", bsd)
	}
}

// waitFor polls cond under mu until it holds or the test gives up.
func waitFor(t *testing.T, mu *sync.Mutex, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ok := cond()
		mu.Unlock()
		if ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the %s", what)
}
