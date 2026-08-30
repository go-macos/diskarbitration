// Copyright (c) 2026, the go-macos authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

//go:build darwin

package diskarbitration

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/go-macos/objc"
)

// The failure branches of the darwin bridge.
//
// Every C entry point this package uses is a package-level function variable —
// bound once by load(), and therefore replaceable. That is the seam: a test
// swaps daSessionCreate for one that answers NULL and reaches the branch that
// reports [ErrNoSession] without needing a machine where the daemon is
// genuinely unreachable.
//
// This is the same technique as github.com/go-macos/objc's init(), one level
// further down: there the seams are Go functions over purego, here they are the
// bound C functions themselves.
//
// ⛔ NOTHING HERE TOUCHES A REAL DISK. The operations that could are aimed at
// stubs.

// swap replaces a bound function for the duration of a test.
func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	t.Cleanup(func() { *p = old })
	*p = v
}

func TestBridgeLoaded(t *testing.T) {
	// If this fails, every other darwin test is testing nothing.
	if loadErr != nil {
		t.Fatalf("the bridge did not load: %v", loadErr)
	}
	if modeRef == 0 {
		t.Fatal("no run-loop mode string was created")
	}
}

func TestOpenReportsLoadFailure(t *testing.T) {
	boom := errors.New("no framework here")
	swap(t, &loadErr, boom)
	if _, err := realSessionOpen(); !errors.Is(err, boom) {
		t.Fatalf("realSessionOpen = %v, want %v", err, boom)
	}
}

func TestOpenReportsNoSession(t *testing.T) {
	swap(t, &daSessionCreate, func(uintptr) uintptr { return 0 })
	if _, err := realSessionOpen(); !errors.Is(err, ErrNoSession) {
		t.Fatalf("realSessionOpen = %v, want ErrNoSession", err)
	}
}

func TestSeamsRefuseAnUnknownHandle(t *testing.T) {
	// A handle that is not in the registry is a session that has been
	// closed. Every seam must answer rather than dereference it.
	const gone handle = 999999
	if _, err := realDescribe(gone, "disk0"); !errors.Is(err, ErrClosed) {
		t.Errorf("realDescribe = %v, want ErrClosed", err)
	}
	if err := realUnmount(gone, "disk0", 0); !errors.Is(err, ErrClosed) {
		t.Errorf("realUnmount = %v, want ErrClosed", err)
	}
	if err := realEject(gone, "disk0"); !errors.Is(err, ErrClosed) {
		t.Errorf("realEject = %v, want ErrClosed", err)
	}
	if _, err := realWatch(gone, func(EventKind, string, map[string]any) {}); !errors.Is(err, ErrClosed) {
		t.Errorf("realWatch = %v, want ErrClosed", err)
	}
	// Closing one is a no-op, not a crash.
	realSessionClose(gone)
}

func TestDescribeReportsNoDisk(t *testing.T) {
	s := liveSession(t)
	swap(t, &daDiskCreateFromBSD, func(uintptr, uintptr, string) uintptr { return 0 })
	if _, err := s.Describe("disk0"); !errors.Is(err, ErrNoDisk) {
		t.Fatalf("Describe = %v, want ErrNoDisk", err)
	}
}

func TestUnmountReportsNoDisk(t *testing.T) {
	s := liveSession(t)
	swap(t, &daDiskCreateFromBSD, func(uintptr, uintptr, string) uintptr { return 0 })
	if err := s.Unmount("disk0", UnmountDefault); !errors.Is(err, ErrNoDisk) {
		t.Fatalf("Unmount = %v, want ErrNoDisk", err)
	}
	if err := s.Eject("disk0"); !errors.Is(err, ErrNoDisk) {
		t.Fatalf("Eject = %v, want ErrNoDisk", err)
	}
}

func TestUnmountTimesOut(t *testing.T) {
	s := liveSession(t)
	// A daemon that never answers. The request is swallowed rather than
	// issued, so nothing is unmounted and the waiter has nothing to wait
	// for — which is exactly the wedged-filesystem case the bound exists
	// for.
	swap(t, &daDiskUnmountFn, func(uintptr, uint32, uintptr, uintptr) {})
	swap(t, &unmountTimeout, 150*time.Millisecond)

	start := time.Now()
	err := s.Unmount("disk0", UnmountDefault)
	if err == nil {
		t.Fatal("a request that is never answered must not report success")
	}
	if !strings.Contains(err.Error(), "no answer") {
		t.Errorf("err = %v, want it to say no answer arrived", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the wait took %s; the bound did not fire", d)
	}
}

func TestLateCompletionIsDiscarded(t *testing.T) {
	// The callback for an operation that has already timed out finds no
	// pending entry. It must return quietly rather than send on a channel
	// nobody is reading.
	pendMu.Lock()
	before := len(pendMap)
	pendMu.Unlock()

	onCompletion(0, 0xDEADBEEF)

	pendMu.Lock()
	after := len(pendMap)
	pendMu.Unlock()
	if after != before {
		t.Errorf("the registry changed: %d -> %d", before, after)
	}

	// And one that IS still pending is answered.
	p := &pending{op: "unmount", disk: "disk0", res: make(chan error, 1)}
	pendMu.Lock()
	pendMap[0xBEEF] = p
	pendMu.Unlock()
	onCompletion(0, 0xBEEF)
	if err := <-p.res; err != nil {
		t.Errorf("a NULL dissenter must complete successfully, got %v", err)
	}
}

func TestDeliverToAnUnknownWatch(t *testing.T) {
	// A callback that arrives between the unregistration and the daemon
	// noticing. It must not panic.
	deliver(0xDEADBEEF, Appeared, 0, true)
	deliver(0xDEADBEEF, Disappeared, 0, false)
}

func TestDissenterError(t *testing.T) {
	// A NULL dissenter is DiskArbitration's entire success signal.
	if err := dissenterError("unmount", "disk0", 0); err != nil {
		t.Fatalf("a NULL dissenter must mean success, got %v", err)
	}

	// A real dissenter, built with the framework's own constructor, so the
	// status and string are read back through the same calls a daemon
	// refusal would come through.
	da, err := purego.Dlopen(Framework, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		t.Skipf("DiskArbitration unavailable: %v", err)
	}
	sym, err := purego.Dlsym(da, "DADissenterCreate")
	if err != nil || sym == 0 {
		t.Skip("DADissenterCreate is not exported here")
	}
	var create func(alloc uintptr, status uint32, str uintptr) uintptr
	purego.RegisterFunc(&create, sym)

	msg := uintptr(objc.NSString("the volume is in use"))
	d := create(0, uint32(ReturnBusy), msg)
	if d == 0 {
		t.Fatal("DADissenterCreate returned NULL")
	}
	defer cfRelease(d)

	err = dissenterError("unmount", "disk4s1", d)
	var de *DiskError
	if !errors.As(err, &de) {
		t.Fatalf("want a *DiskError, got %T: %v", err, err)
	}
	if de.Status != ReturnBusy {
		t.Errorf("Status = %v, want busy", de.Status)
	}
	if de.Message != "the volume is in use" {
		t.Errorf("Message = %q", de.Message)
	}

	// A dissenter carrying kDAReturnSuccess is a refusal with no reason. It
	// must still be an error: reporting nil would tell the caller the volume
	// is unmounted when it is not.
	ok := create(0, uint32(ReturnSuccess), 0)
	if ok == 0 {
		t.Fatal("DADissenterCreate returned NULL")
	}
	defer cfRelease(ok)
	err = dissenterError("eject", "disk4", ok)
	if !errors.As(err, &de) {
		t.Fatalf("a dissenter with a success status must still be an error, got %v", err)
	}
	if !strings.Contains(de.Message, "gave no reason") {
		t.Errorf("Message = %q", de.Message)
	}
}

// ---------------------------------------------------------------------------
// CoreFoundation conversion.
// ---------------------------------------------------------------------------

func TestGoStringOfNull(t *testing.T) {
	if got := goString(0); got != "" {
		t.Errorf("goString(NULL) = %q, want empty", got)
	}
}

func TestGoStringFailure(t *testing.T) {
	// CFStringGetCString refusing the buffer. It cannot happen with the size
	// this package computes, so the branch is reached by making the call
	// fail rather than by finding a string that defeats it.
	swap(t, &cfStringGetCString, func(uintptr, *byte, int, uint32) bool { return false })
	if got := goString(uintptr(objc.NSString("anything"))); got != "" {
		t.Errorf("goString = %q, want empty when the conversion fails", got)
	}
}

func TestCFValueTypes(t *testing.T) {
	// Each CoreFoundation type the description dictionary can carry,
	// constructed here so the conversion is exercised even for the ones this
	// machine's disks do not happen to use.
	objc.AutoreleasePool(func() {
		if got := cfValue(0); got != nil {
			t.Errorf("cfValue(NULL) = %v, want nil", got)
		}

		if got := cfValue(uintptr(objc.NSString("hello"))); got != "hello" {
			t.Errorf("CFString -> %#v", got)
		}

		yes := objc.ClassID("NSNumber").Send(objc.Sel("numberWithBool:"), true)
		if got := cfValue(uintptr(yes)); got != true {
			t.Errorf("CFBoolean -> %#v", got)
		}

		n := objc.ClassID("NSNumber").Send(objc.Sel("numberWithLongLong:"), int64(3996329328640))
		if got := cfValue(uintptr(n)); got != int64(3996329328640) {
			t.Errorf("CFNumber(integer) -> %#v", got)
		}

		// The float case is the one that mattered: CFNumberGetValue reports
		// FALSE for the lossy read of a double as an SInt64, and a binding
		// that trusted the boolean returned zero. DAAppearanceTime is such a
		// number.
		f := objc.ClassID("NSNumber").Send(objc.Sel("numberWithDouble:"), 809617871.764205)
		got, ok := cfValue(uintptr(f)).(float64)
		if !ok || got < 809617871 || got > 809617872 {
			t.Errorf("CFNumber(double) -> %#v, want ~809617871.76", cfValue(uintptr(f)))
		}

		data := objc.ClassID("NSData").Send(objc.Sel("dataWithBytes:length:"),
			unsafe.Pointer(&[]byte{1, 2, 3}[0]), 3)
		if got, ok := cfValue(uintptr(data)).([]byte); !ok || len(got) != 3 || got[2] != 3 {
			t.Errorf("CFData -> %#v", cfValue(uintptr(data)))
		}
		empty := objc.ClassID("NSData").Send(objc.Sel("data"))
		if got, ok := cfValue(uintptr(empty)).([]byte); !ok || len(got) != 0 {
			t.Errorf("empty CFData -> %#v", cfValue(uintptr(empty)))
		}

		// A CFDate has no typed case, so it degrades to its description —
		// the same path DAMediaIcon's CFDictionary takes on every disk.
		date := objc.ClassID("NSDate").Send(objc.Sel("date"))
		if got, ok := cfValue(uintptr(date)).(string); !ok || got == "" {
			t.Errorf("CFDate -> %#v, want a non-empty description", cfValue(uintptr(date)))
		}
	})
}

// newCFUUID makes a real CFUUID.
//
// NSUUID is NOT toll-free bridged to CFUUID — a test that used one went through
// the CFCopyDescription fallback and passed while proving nothing, because that
// fallback also renders a UUID as 36 characters. The type is asserted here so
// the mistake cannot come back.
func newCFUUID(t *testing.T) uintptr {
	t.Helper()
	cf, err := purego.Dlopen(objc.CoreFoundation, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		t.Skipf("CoreFoundation unavailable: %v", err)
	}
	sym, err := purego.Dlsym(cf, "CFUUIDCreate")
	if err != nil || sym == 0 {
		t.Skip("CFUUIDCreate is not exported here")
	}
	var create func(alloc uintptr) uintptr
	purego.RegisterFunc(&create, sym)
	u := create(0)
	if u == 0 {
		t.Fatal("CFUUIDCreate returned NULL")
	}
	if cfGetTypeID(u) != cfUUIDTypeID() {
		t.Fatal("CFUUIDCreate did not return a CFUUID")
	}
	t.Cleanup(func() { cfRelease(u) })
	return u
}

func TestCFValueURLAndUUID(t *testing.T) {
	uuid := newCFUUID(t)
	objc.AutoreleasePool(func() {
		url := objc.ClassID("NSURL").Send(objc.Sel("fileURLWithPath:"), objc.NSString("/Volumes/Thing"))
		if got := cfValue(uintptr(url)); got != "/Volumes/Thing" {
			t.Errorf("CFURL -> %#v, want the POSIX path", got)
		}

		got, ok := cfValue(uuid).(string)
		if !ok || len(got) != 36 || strings.Count(got, "-") != 4 {
			t.Errorf("CFUUID -> %#v, want a 36-character UUID string", cfValue(uuid))
		}
	})
}

func TestCFValueNullResults(t *testing.T) {
	uuid := newCFUUID(t)
	objc.AutoreleasePool(func() {
		// Each of the three copy-style conversions answering NULL. None can
		// happen for a well-formed object, so each is reached by making the
		// call fail.
		url := uintptr(objc.ClassID("NSURL").Send(objc.Sel("fileURLWithPath:"), objc.NSString("/tmp")))
		func() {
			old := cfURLCopyFSPath
			defer func() { cfURLCopyFSPath = old }()
			cfURLCopyFSPath = func(uintptr, int) uintptr { return 0 }
			if got := cfValue(url); got != "" {
				t.Errorf("CFURL with no path -> %#v", got)
			}
		}()

		func() {
			old := cfUUIDCreateString
			defer func() { cfUUIDCreateString = old }()
			cfUUIDCreateString = func(uintptr, uintptr) uintptr { return 0 }
			if got := cfValue(uuid); got != "" {
				t.Errorf("CFUUID with no string -> %#v", got)
			}
		}()

		date := uintptr(objc.ClassID("NSDate").Send(objc.Sel("date")))
		func() {
			old := cfCopyDescription
			defer func() { cfCopyDescription = old }()
			cfCopyDescription = func(uintptr) uintptr { return 0 }
			if got := cfValue(date); got != "" {
				t.Errorf("an undescribable object -> %#v", got)
			}
		}()
	})
}

func TestCFDictToMap(t *testing.T) {
	objc.AutoreleasePool(func() {
		if got := cfDictToMap(uintptr(objc.ClassID("NSDictionary").Send(objc.Sel("dictionary")))); len(got) != 0 {
			t.Errorf("an empty dictionary -> %v, want an empty map", got)
		}

		d := objc.MapToDict(map[string]string{"DAVolumeName": "Thing", "DAVolumeKind": "hfs"})
		got := cfDictToMap(uintptr(d))
		if got["DAVolumeName"] != "Thing" || got["DAVolumeKind"] != "hfs" {
			t.Errorf("cfDictToMap = %v", got)
		}

		// A key that is not a CFString converts to "" and is dropped rather
		// than kept under an empty name.
		nonString := objc.ClassID("NSMutableDictionary").Send(objc.Sel("dictionary"))
		key := objc.ClassID("NSNumber").Send(objc.Sel("numberWithInt:"), 42)
		nonString.Send(objc.Sel("setObject:forKey:"), objc.NSString("v"), key)
		if got := cfDictToMap(uintptr(nonString)); len(got) != 0 {
			t.Errorf("a non-string key was kept: %v", got)
		}
	})
}

func TestGoStringWithoutTerminator(t *testing.T) {
	// The tail of goString: a conversion that fills the buffer completely
	// and leaves no NUL. CFStringGetCString cannot do that with the buffer
	// this package sizes — four bytes per UTF-16 unit plus one — so the
	// branch is reached by a stub that does. It exists so a truncated
	// conversion yields the bytes rather than a string running past them.
	swap(t, &cfStringGetCString, func(_ uintptr, buf *byte, size int, _ uint32) bool {
		for i, c := range []byte("abcde") {
			if i >= size {
				break
			}
			*(*byte)(unsafe.Add(unsafe.Pointer(buf), i)) = c
		}
		return true
	})
	swap(t, &cfStringGetLength, func(uintptr) int { return 1 }) // buffer of 5
	if got := goString(uintptr(objc.NSString("ignored"))); got != "abcde" {
		t.Errorf("goString = %q, want the whole buffer back", got)
	}
}

func TestCFDictSkipsAnEmptyKey(t *testing.T) {
	// A CFString key that is empty has no name to file the value under.
	objc.AutoreleasePool(func() {
		d := objc.ClassID("NSMutableDictionary").Send(objc.Sel("dictionary"))
		d.Send(objc.Sel("setObject:forKey:"), objc.NSString("v"), objc.NSString(""))
		d.Send(objc.Sel("setObject:forKey:"), objc.NSString("w"), objc.NSString("DAVolumeName"))
		got := cfDictToMap(uintptr(d))
		if len(got) != 1 || got["DAVolumeName"] != "w" {
			t.Errorf("cfDictToMap = %v, want just the named entry", got)
		}
	})
}

func TestBindAllFailures(t *testing.T) {
	// bindAll assigns nothing until a symbol resolves, so these calls cannot
	// disturb the real bindings — but load() is re-run afterwards anyway,
	// because a test that left this package half-bound would take the rest
	// of the suite down with it and the cause would look like anything but
	// this.
	t.Cleanup(load)

	if err := bindAll("/nonexistent/CoreFoundation", Framework); err == nil ||
		!strings.Contains(err.Error(), "load CoreFoundation") {
		t.Errorf("a missing CoreFoundation gave %v", err)
	}
	if err := bindAll(objc.CoreFoundation, "/nonexistent/DiskArbitration"); err == nil ||
		!strings.Contains(err.Error(), "load DiskArbitration") {
		t.Errorf("a missing DiskArbitration gave %v", err)
	}
	// A library that opens but exports none of the DiskArbitration symbols.
	// The DA binds come first, so the first one fails and nothing is
	// assigned at all.
	err := bindAll(objc.CoreFoundation, objc.CoreFoundation)
	if err == nil || !strings.Contains(err.Error(), "missing function DASessionCreate") {
		t.Errorf("a library with no DA symbols gave %v", err)
	}
}

func TestOpenReportsAClassRegistrationFailure(t *testing.T) {
	// The run-loop class is registered once per process and the registration
	// has already happened by now, so the failure is injected into its
	// recorded result rather than by trying to make the runtime refuse.
	boom := errors.New("class name taken")
	swap(t, &runnerClassErr, boom)
	if _, err := realSessionOpen(); !errors.Is(err, boom) {
		t.Fatalf("realSessionOpen = %v, want it to wrap %v", err, boom)
	}
}
