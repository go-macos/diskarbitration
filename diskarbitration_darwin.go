// Copyright (c) 2026, the go-macos authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

//go:build darwin

package diskarbitration

import (
	"context"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/go-macos/objc"
)

// This file binds DiskArbitration and the CoreFoundation it speaks in, through
// purego (no cgo), and wires them into the seams declared in
// diskarbitration.go. DiskArbitration is a plain C API over CoreFoundation
// objects — not Objective-C message sends — so it is reached with dlsym
// directly; github.com/go-macos/objc is reused for the framework paths and,
// crucially, for the run loop.

// kCFStringEncodingUTF8, for CFStringCreateWithCString / CFStringGetCString.
const kCFStringEncodingUTF8 = 0x08000100

// kCFNumberSInt64Type is the widest integer CFNumberGetValue will fill;
// kCFNumberDoubleType is used for the CFNumbers that hold a floating value.
const (
	kCFNumberSInt64Type = 4
	kCFNumberDoubleType = 13
)

// kCFURLPOSIXPathStyle, for CFURLCopyFileSystemPath.
const kCFURLPOSIXPathStyle = 0

// runLoopMode is the name of the mode the session is scheduled in.
//
// kCFRunLoopDefaultMode is an exported CFStringRef VARIABLE, and dereferencing
// an exported data pointer is what go vet's unsafeptr check rejects — so the
// literal is spelled out and a CFString is built from it, which is what every
// other binding in this fleet does with it. CFRunLoop compares modes by string
// content, so the two are the same mode.
const runLoopMode = "kCFRunLoopDefaultMode"

// unmountTimeout bounds the wait for an unmount or eject completion callback.
//
// It exists because the daemon's answer arrives on the run loop and nothing
// guarantees it arrives at all: a wedged filesystem, or a session whose loop
// has stopped, leaves the waiter blocked forever with no error to report. A
// bound turns "hangs" into "says so".
//
// It is a var, not a const, so a test can shorten it and prove that the bound
// actually fires. A timeout nobody has ever seen work is a comment.
var unmountTimeout = 2 * time.Minute

var (
	// DiskArbitration.
	daSessionCreate      func(alloc uintptr) uintptr
	daSessionSchedule    func(session, runLoop, mode uintptr)
	daSessionUnschedule  func(session, runLoop, mode uintptr)
	daDiskCreateFromBSD  func(alloc, session uintptr, name string) uintptr
	daDiskCopyDescrip    func(disk uintptr) uintptr
	daDiskGetBSDName     func(disk uintptr) string
	daDiskUnmountFn      func(disk uintptr, options uint32, callback, context uintptr)
	daDiskEjectFn        func(disk uintptr, options uint32, callback, context uintptr)
	daRegisterAppeared   func(session, match, callback, context uintptr)
	daRegisterDisappear  func(session, match, callback, context uintptr)
	daUnregisterCallback func(session, callback, context uintptr)
	daDissenterGetStatus func(dissenter uintptr) uint32
	daDissenterGetString func(dissenter uintptr) uintptr

	// CoreFoundation.
	cfRelease           func(cf uintptr)
	cfGetTypeID         func(cf uintptr) uint
	cfCopyDescription   func(cf uintptr) uintptr
	cfStringCreate      func(alloc uintptr, s string, enc uint32) uintptr
	cfStringGetLength   func(s uintptr) int
	cfStringGetCString  func(s uintptr, buf *byte, size int, enc uint32) bool
	cfStringTypeID      func() uint
	cfBooleanGetValue   func(b uintptr) bool
	cfBooleanTypeID     func() uint
	cfNumberGetValue    func(n uintptr, theType int, valuePtr unsafe.Pointer) bool
	cfNumberIsFloat     func(n uintptr) bool
	cfNumberTypeID      func() uint
	cfURLCopyFSPath     func(u uintptr, style int) uintptr
	cfURLTypeID         func() uint
	cfUUIDCreateString  func(alloc, u uintptr) uintptr
	cfUUIDTypeID        func() uint
	cfDataGetLength     func(d uintptr) int
	cfDataGetBytePtr    func(d uintptr) *byte
	cfDataTypeID        func() uint
	cfDictGetCount      func(d uintptr) int
	cfDictGetKeysValues func(d uintptr, keys, values *uintptr)
	cfRunLoopGetCurrent func() uintptr

	// loadErr is the first symbol that could not be resolved, if any.
	loadErr error
	// modeRef is the retained CFString naming the run-loop mode.
	modeRef uintptr
)

// init resolves every symbol and points the seams at the real calls.
//
// The whole framework is opened before a single symbol is named. A function
// that names a framework must guarantee it is loaded: a caller who forgets gets
// a null symbol, a null session, and every call after it returning zero — in
// silence, because nothing in C complains. The load belongs with the code that
// knows which framework it needs, which is here.
func init() {
	load()
	sessionOpen = realSessionOpen
	sessionClose = realSessionClose
	diskDescribe = realDescribe
	diskUnmount = realUnmount
	diskEject = realEject
	diskWatch = realWatch
}

// load binds the two frameworks and, on success, builds the run-loop mode
// string.
func load() {
	loadErr = bindAll(objc.CoreFoundation, Framework)
	if loadErr == nil {
		modeRef = cfStringCreate(0, runLoopMode, kCFStringEncodingUTF8)
	}
}

// bindAll opens the two frameworks by path and binds every entry point,
// returning the first failure. A missing symbol on a supported macOS is not
// expected, and the branches exist so that such a failure is a message rather
// than a call through a nil function pointer.
//
// The paths are parameters rather than constants so that the failure branches
// can be tested — by pointing it at a path that does not exist, and at a
// library that exists but exports none of these. Nothing is assigned until a
// symbol resolves, so a failing call leaves the real bindings alone.
func bindAll(cfPath, daPath string) error {
	cf, err := purego.Dlopen(cfPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("diskarbitration: load CoreFoundation: %w", err)
	}
	da, err := purego.Dlopen(daPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("diskarbitration: load DiskArbitration: %w", err)
	}

	var firstErr error
	bind := func(fptr any, h uintptr, name string) {
		if firstErr != nil {
			return
		}
		p, err := purego.Dlsym(h, name)
		if err != nil || p == 0 {
			firstErr = fmt.Errorf("diskarbitration: missing function %s", name)
			return
		}
		purego.RegisterFunc(fptr, p)
	}

	bind(&daSessionCreate, da, "DASessionCreate")
	bind(&daSessionSchedule, da, "DASessionScheduleWithRunLoop")
	bind(&daSessionUnschedule, da, "DASessionUnscheduleFromRunLoop")
	bind(&daDiskCreateFromBSD, da, "DADiskCreateFromBSDName")
	bind(&daDiskCopyDescrip, da, "DADiskCopyDescription")
	bind(&daDiskGetBSDName, da, "DADiskGetBSDName")
	bind(&daDiskUnmountFn, da, "DADiskUnmount")
	bind(&daDiskEjectFn, da, "DADiskEject")
	bind(&daRegisterAppeared, da, "DARegisterDiskAppearedCallback")
	bind(&daRegisterDisappear, da, "DARegisterDiskDisappearedCallback")
	bind(&daUnregisterCallback, da, "DAUnregisterCallback")
	bind(&daDissenterGetStatus, da, "DADissenterGetStatus")
	bind(&daDissenterGetString, da, "DADissenterGetStatusString")

	bind(&cfRelease, cf, "CFRelease")
	bind(&cfGetTypeID, cf, "CFGetTypeID")
	bind(&cfCopyDescription, cf, "CFCopyDescription")
	bind(&cfStringCreate, cf, "CFStringCreateWithCString")
	bind(&cfStringGetLength, cf, "CFStringGetLength")
	bind(&cfStringGetCString, cf, "CFStringGetCString")
	bind(&cfStringTypeID, cf, "CFStringGetTypeID")
	bind(&cfBooleanGetValue, cf, "CFBooleanGetValue")
	bind(&cfBooleanTypeID, cf, "CFBooleanGetTypeID")
	bind(&cfNumberGetValue, cf, "CFNumberGetValue")
	bind(&cfNumberIsFloat, cf, "CFNumberIsFloatType")
	bind(&cfNumberTypeID, cf, "CFNumberGetTypeID")
	bind(&cfURLCopyFSPath, cf, "CFURLCopyFileSystemPath")
	bind(&cfURLTypeID, cf, "CFURLGetTypeID")
	bind(&cfUUIDCreateString, cf, "CFUUIDCreateString")
	bind(&cfUUIDTypeID, cf, "CFUUIDGetTypeID")
	bind(&cfDataGetLength, cf, "CFDataGetLength")
	bind(&cfDataGetBytePtr, cf, "CFDataGetBytePtr")
	bind(&cfDataTypeID, cf, "CFDataGetTypeID")
	bind(&cfDictGetCount, cf, "CFDictionaryGetCount")
	bind(&cfDictGetKeysValues, cf, "CFDictionaryGetKeysAndValues")
	bind(&cfRunLoopGetCurrent, cf, "CFRunLoopGetCurrent")

	return firstErr
}

// ---------------------------------------------------------------------------
// CoreFoundation to Go.
// ---------------------------------------------------------------------------

// goString copies a CFString's UTF-8 bytes into a Go-owned buffer.
//
// The buffer is sized at four bytes per UTF-16 unit plus a NUL, which is the
// widest UTF-8 encoding of any code point, so it never truncates. It fills a Go
// slice through CFStringGetCString rather than dereferencing the CoreFoundation-
// owned pointer CFStringGetCStringPtr may or may not return, so no uintptr ever
// holds the only reference to memory that is about to move.
func goString(s uintptr) string {
	if s == 0 {
		return ""
	}
	n := cfStringGetLength(s)*4 + 1
	buf := make([]byte, n)
	if !cfStringGetCString(s, &buf[0], n, kCFStringEncodingUTF8) {
		return ""
	}
	for i, c := range buf {
		if c == 0 {
			return string(buf[:i])
		}
	}
	return string(buf)
}

// cfValue converts one CoreFoundation value to the Go value the portable half
// expects: string, bool, int64 or []byte.
//
// A type nobody anticipated degrades to its CFCopyDescription rather than being
// dropped — DAMediaIcon is a CFDictionary and arrives here on every real disk,
// so the default branch is the normal case, not an error case.
func cfValue(v uintptr) any {
	if v == 0 {
		return nil
	}
	switch cfGetTypeID(v) {
	case cfStringTypeID():
		return goString(v)
	case cfBooleanTypeID():
		return cfBooleanGetValue(v)
	case cfNumberTypeID():
		// The float check is not decoration. CFNumberGetValue returns
		// FALSE for a lossy conversion — and reading a CFNumber that holds
		// a double as an SInt64 is lossy — so a binding that trusts the
		// boolean throws the value away. DAAppearanceTime is such a
		// number, and it read back as 0 on this machine until the type was
		// consulted first.
		if cfNumberIsFloat(v) {
			var f float64
			cfNumberGetValue(v, kCFNumberDoubleType, unsafe.Pointer(&f))
			return f
		}
		var n int64
		cfNumberGetValue(v, kCFNumberSInt64Type, unsafe.Pointer(&n))
		return n
	case cfURLTypeID():
		p := cfURLCopyFSPath(v, kCFURLPOSIXPathStyle)
		if p == 0 {
			return ""
		}
		defer cfRelease(p)
		return goString(p)
	case cfUUIDTypeID():
		p := cfUUIDCreateString(0, v)
		if p == 0 {
			return ""
		}
		defer cfRelease(p)
		return goString(p)
	case cfDataTypeID():
		n := cfDataGetLength(v)
		out := make([]byte, n)
		if n > 0 {
			copy(out, unsafe.Slice(cfDataGetBytePtr(v), n))
		}
		return out
	}
	p := cfCopyDescription(v)
	if p == 0 {
		return ""
	}
	defer cfRelease(p)
	return goString(p)
}

// cfDictToMap flattens a CFDictionary of CFString keys into a Go map.
//
// The key's type is CHECKED before it is read, and that check is not
// defensive tidiness: CFStringGetLength on an object that is not a CFString
// does not return a wrong answer, it SIGSEGVs — measured, on a dictionary whose
// key was a CFNumber. Every DADiskDescription key is a CFString, so nothing on
// the real path exercises this; the day one is not, the difference is between a
// dropped entry and a crash with none of this package on the stack.
func cfDictToMap(dict uintptr) map[string]any {
	n := cfDictGetCount(dict)
	m := make(map[string]any, n)
	if n == 0 {
		return m
	}
	keys := make([]uintptr, n)
	vals := make([]uintptr, n)
	cfDictGetKeysValues(dict, &keys[0], &vals[0])
	strID := cfStringTypeID()
	for i := 0; i < n; i++ {
		if keys[i] == 0 || cfGetTypeID(keys[i]) != strID {
			continue // not a key this package can name
		}
		k := goString(keys[i])
		if k == "" {
			continue
		}
		m[k] = cfValue(vals[i])
	}
	return m
}

// ---------------------------------------------------------------------------
// Sessions and their run loops.
// ---------------------------------------------------------------------------

// session is one live DASession plus the run loop its callbacks arrive on.
type session struct {
	ref    uintptr // DASessionRef
	runner *objc.Runner
	cancel context.CancelFunc
	done   chan struct{}
}

// The session registry.
//
// A handle is a small integer, not a pointer, and that is deliberate: the same
// value is handed to C as the callback context, and a Go pointer may not be
// stored in C memory. The registry turns the integer back into the session when
// a callback arrives.
var (
	regMu   sync.Mutex
	regNext handle = 1
	regMap         = map[handle]*session{}
)

// lookup returns the session for a handle, or nil once it has been closed.
func lookup(h handle) *session {
	regMu.Lock()
	defer regMu.Unlock()
	return regMap[h]
}

// runnerClassOnce registers the Objective-C class objc.Run instantiates. It is
// registered once per process: registering the same class name twice fails, and
// a session is not the only thing that may be opened twice.
var (
	runnerClassOnce sync.Once
	runnerClass     objc.Class
	runnerClassErr  error
)

// runnerClassFor returns the run-loop runner class, registering it on first
// use. objc.Run requires a class with a no-op keepAlive: method, which the
// timer that keeps the loop from spinning targets.
func runnerClassFor() (objc.Class, error) {
	runnerClassOnce.Do(func() {
		runnerClass, runnerClassErr = objc.RegisterClass(
			"GoMacOSDiskArbitrationRunner",
			objc.GetClass("NSObject"),
			[]objc.MethodDef{{
				Cmd: objc.Sel("keepAlive:"),
				Fn:  func(self objc.ID, cmd objc.SEL, timer objc.ID) {},
			}},
		)
	})
	return runnerClass, runnerClassErr
}

// realSessionOpen creates the DASession and starts its run loop.
//
// The loop is objc.Run's, not one written here: it pins its goroutine to an OS
// thread, drives a CFRunLoop, and gives back a Runner whose Submit marshals work
// onto that thread. The session is scheduled from INSIDE the setup callback,
// because DASessionScheduleWithRunLoop must name the run loop of the thread that
// will service it and CFRunLoopGetCurrent only answers correctly there.
//
// Open does not return until the loop is running and the session is scheduled.
// Returning earlier would let a caller's first Unmount register a completion
// callback on a loop that is not yet turning, and wait out its timeout for an
// answer that was delivered before anyone was listening.
func realSessionOpen() (handle, error) {
	if loadErr != nil {
		return 0, loadErr
	}
	cls, err := runnerClassFor()
	if err != nil {
		return 0, fmt.Errorf("diskarbitration: register run-loop class: %w", err)
	}
	ref := daSessionCreate(0)
	if ref == 0 {
		return 0, ErrNoSession
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &session{ref: ref, cancel: cancel, done: make(chan struct{})}
	ready := make(chan struct{})
	go func() {
		defer close(s.done)
		_ = objc.Run(ctx, cls, func(r *objc.Runner) {
			s.runner = r
			daSessionSchedule(ref, cfRunLoopGetCurrent(), modeRef)
			close(ready)
		})
	}()
	<-ready

	regMu.Lock()
	h := regNext
	regNext++
	regMap[h] = s
	regMu.Unlock()
	return h, nil
}

// realSessionClose unschedules the session, stops the loop and releases the ref.
//
// The unschedule runs ON the run-loop thread, through Submit, for the same
// reason the schedule did: the daemon's plumbing is attached to that thread and
// detaching it from another is a data race inside CoreFoundation. Only then is
// the loop cancelled and waited for, so nothing is released underneath a
// callback that is still running.
func realSessionClose(h handle) {
	regMu.Lock()
	s := regMap[h]
	delete(regMap, h)
	regMu.Unlock()
	if s == nil {
		return
	}
	s.runner.Submit(func() { daSessionUnschedule(s.ref, cfRunLoopGetCurrent(), modeRef) })
	s.cancel()
	<-s.done
	cfRelease(s.ref)
}

// ---------------------------------------------------------------------------
// Describe.
// ---------------------------------------------------------------------------

// realDescribe copies one disk's description.
//
// Both CoreFoundation objects are owned by this function — DADiskCreateFromBSDName
// and DADiskCopyDescription both follow the Create rule — so both are released
// here, and the dictionary is fully converted to Go values before the release so
// that nothing survives it but Go memory.
func realDescribe(h handle, bsd string) (map[string]any, error) {
	s := lookup(h)
	if s == nil {
		return nil, ErrClosed
	}
	disk := daDiskCreateFromBSD(0, s.ref, bsd)
	if disk == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoDisk, bsd)
	}
	defer cfRelease(disk)
	desc := daDiskCopyDescrip(disk)
	if desc == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoDescription, bsd)
	}
	defer cfRelease(desc)
	return cfDictToMap(desc), nil
}

// ---------------------------------------------------------------------------
// Unmount and eject.
// ---------------------------------------------------------------------------

// pending is one in-flight unmount or eject waiting for its completion
// callback.
type pending struct {
	op   string
	disk string
	res  chan error
}

// The pending registry, keyed the same way as the session one and for the same
// reason: the key travels to C as the callback context.
var (
	pendMu   sync.Mutex
	pendNext uintptr = 1
	pendMap          = map[uintptr]*pending{}
)

// completionCallback is the single DADiskUnmountCallback / DADiskEjectCallback
// for the whole process.
//
// One callback, not one per operation: purego.NewCallback draws from a fixed
// pool of trampolines — a few thousand for the life of the process — and a
// program that made a new one per unmount would run out and die on an unmount
// that looked no different from the ones before it. The context integer says
// which operation is completing.
var completionCallback = sync.OnceValue(func() uintptr {
	return purego.NewCallback(func(disk uintptr, dissenter uintptr, ctx uintptr) uintptr {
		onCompletion(dissenter, ctx)
		return 0
	})
})

// onCompletion is the trampoline's body, in a named function so it can be
// called from a test — a purego callback cannot be invoked from Go, and a test
// that re-implemented the body would be checking its own copy.
func onCompletion(dissenter, ctx uintptr) {
	pendMu.Lock()
	p := pendMap[ctx]
	delete(pendMap, ctx)
	pendMu.Unlock()
	if p == nil {
		return // already timed out; nobody is listening
	}
	p.res <- dissenterError(p.op, p.disk, dissenter)
}

// dissenterError turns a dissenter reference into an error. A NULL dissenter is
// success — that is the whole of DiskArbitration's success signal.
func dissenterError(op, disk string, dissenter uintptr) error {
	if dissenter == 0 {
		return nil
	}
	status := daDissenterGetStatus(dissenter)
	msg := goString(daDissenterGetString(dissenter))
	if err := newDiskError(op, disk, status, msg); err != nil {
		return err
	}
	// A dissenter carrying kDAReturnSuccess is a contradiction the API
	// permits: the operation was refused by something that then reported no
	// reason. It is reported rather than swallowed, because silently
	// returning nil here would tell a caller the volume is unmounted when it
	// is not.
	return &DiskError{Op: op, Disk: disk, Status: ReturnError, Message: "the operation was refused and macOS gave no reason"}
}

// realUnmount is DADiskUnmount, made synchronous.
func realUnmount(h handle, bsd string, opts uint32) error {
	return operate(h, "unmount", bsd, func(disk, cb, ctx uintptr) {
		daDiskUnmountFn(disk, opts, cb, ctx)
	})
}

// realEject is DADiskEject, made synchronous.
func realEject(h handle, bsd string) error {
	return operate(h, "eject", bsd, func(disk, cb, ctx uintptr) {
		daDiskEjectFn(disk, 0, cb, ctx)
	})
}

// operate issues one asynchronous disk operation and waits for its completion
// callback.
//
// The call is made ON the run-loop thread, through Submit. DADiskUnmount
// associates the request with the run loop its session is scheduled on, and
// issuing it from a goroutine that macOS has never seen is the difference
// between a callback and a wait that never ends.
//
// The DADiskRef is released only after the answer arrives: releasing it while
// the request is in flight is a use-after-free inside the daemon's client
// library, and it does not fail on the machine it was written on.
func operate(h handle, op, bsd string, issue func(disk, cb, ctx uintptr)) error {
	s := lookup(h)
	if s == nil {
		return ErrClosed
	}
	cb := completionCallback()

	p := &pending{op: op, disk: bsd, res: make(chan error, 1)}
	pendMu.Lock()
	ctx := pendNext
	pendNext++
	pendMap[ctx] = p
	pendMu.Unlock()

	var disk uintptr
	s.runner.Submit(func() {
		disk = daDiskCreateFromBSD(0, s.ref, bsd)
		if disk == 0 {
			return
		}
		issue(disk, cb, ctx)
	})
	if disk == 0 {
		pendMu.Lock()
		delete(pendMap, ctx)
		pendMu.Unlock()
		return fmt.Errorf("%w: %s", ErrNoDisk, bsd)
	}
	defer func() { s.runner.Submit(func() { cfRelease(disk) }) }()

	select {
	case err := <-p.res:
		return err
	case <-time.After(unmountTimeout):
		pendMu.Lock()
		delete(pendMap, ctx)
		pendMu.Unlock()
		return fmt.Errorf("diskarbitration: %s %s: no answer from the disk arbitration daemon after %s", op, bsd, unmountTimeout)
	}
}

// ---------------------------------------------------------------------------
// Watching.
// ---------------------------------------------------------------------------

// watch is one registration's delivery target.
type watch struct {
	emit func(EventKind, string, map[string]any)
}

// The watch registry, keyed by the integer that travels to C as the context.
var (
	watchMu   sync.Mutex
	watchNext uintptr = 1
	watchMap          = map[uintptr]*watch{}
)

// appearedCallback and disappearedCallback are the process's two
// DADiskAppearedCallback trampolines — one pair for every watch on every
// session, for the same trampoline-pool reason as [completionCallback].
var appearedCallback = sync.OnceValue(func() uintptr {
	return purego.NewCallback(func(disk uintptr, ctx uintptr) uintptr {
		deliver(ctx, Appeared, disk, true)
		return 0
	})
})

var disappearedCallback = sync.OnceValue(func() uintptr {
	return purego.NewCallback(func(disk uintptr, ctx uintptr) uintptr {
		deliver(ctx, Disappeared, disk, false)
		return 0
	})
})

// deliver looks the watch up and hands it the event.
//
// A disappeared disk is not described: DADiskCopyDescription answers NULL for
// media that has gone, so asking would cost an IPC round trip to learn nothing.
// The BSD name still reads correctly off the reference, which is what identifies
// the disk that left.
func deliver(ctx uintptr, kind EventKind, disk uintptr, describe bool) {
	watchMu.Lock()
	w := watchMap[ctx]
	watchMu.Unlock()
	if w == nil {
		return // unregistered between the daemon's send and this call
	}
	name := daDiskGetBSDName(disk)
	var raw map[string]any
	if describe {
		if desc := daDiskCopyDescrip(disk); desc != 0 {
			raw = cfDictToMap(desc)
			cfRelease(desc)
		}
	}
	w.emit(kind, name, raw)
}

// realWatch registers the appearance callbacks on the session's run loop.
//
// A nil match dictionary means "every disk", which is what a caller who asked
// to watch disks meant. Registration is submitted to the run-loop thread so the
// replay of the disks already present begins on the thread that will carry it.
func realWatch(h handle, emit func(EventKind, string, map[string]any)) (func(), error) {
	s := lookup(h)
	if s == nil {
		return nil, ErrClosed
	}
	app, dis := appearedCallback(), disappearedCallback()

	watchMu.Lock()
	ctx := watchNext
	watchNext++
	watchMap[ctx] = &watch{emit: emit}
	watchMu.Unlock()

	s.runner.Submit(func() {
		daRegisterAppeared(s.ref, 0, app, ctx)
		daRegisterDisappear(s.ref, 0, dis, ctx)
	})

	return func() {
		watchMu.Lock()
		delete(watchMap, ctx)
		watchMu.Unlock()
		// The session may already be gone when a Watcher is stopped after
		// its session was closed; unregistering against a released session
		// would be a use-after-free, so the registry is consulted again.
		if live := lookup(h); live != nil {
			live.runner.Submit(func() {
				daUnregisterCallback(live.ref, app, ctx)
				daUnregisterCallback(live.ref, dis, ctx)
			})
		}
	}, nil
}
