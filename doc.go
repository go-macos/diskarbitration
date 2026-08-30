// Copyright (c) 2026, the go-macos authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

// Package diskarbitration binds Apple's DiskArbitration framework: the macOS
// service that knows which block devices exist, which volume is mounted where,
// and how to take one away again politely.
//
// # What this package is for
//
// A program that manipulates disk images needs three answers macOS will not
// give it from the filesystem alone. Which /dev node did my image become? Where
// did the system mount it? How do I detach it without corrupting it? Today
// those answers are obtained by running hdiutil and diskutil and parsing their
// output — a subprocess, a plist parse, and a text format Apple never promised
// to keep. DiskArbitration is the service those tools themselves talk to.
//
// # The boundary — read this before adding anything
//
// THIS PACKAGE DECODES NO FILESYSTEM FORMAT. Not APFS, not HFS+, not FAT, not
// a partition map. It asks a macOS daemon what that daemon already believes and
// reports the answer. [Description.VolumeKind] is a label macOS handed over
// ("apfs", "hfs", "msdos"); nothing here has parsed a superblock to earn it.
//
// Filesystem decoding lives in the go-filesystems org, behind
// github.com/go-filesystems/interface, and it is OS-independent by
// construction: eighteen drivers that read bytes and do not know what host they
// are on. The moment a superblock is parsed here, that independence is gone and
// one of those drivers has been forked into a macOS-only copy.
//
// The Linux counterpart of THIS package — talking to a kernel that also already
// knows — is github.com/go-fsctl, in its own org, for the same reason. Two
// OS-specific packages that ask their own system, one OS-independent org that
// reads bytes. Keep them apart.
//
// # Shape of the API
//
// Everything hangs off a [Session]:
//
//	s, err := diskarbitration.Open()
//	if err != nil { return err }
//	defer s.Close()
//
//	for _, d := range must(s.DescribeAll()) {
//	    fmt.Println(d)
//	}
//
// [Session.Disks] enumerates the BSD names, [Session.Describe] answers with a
// [Description], [Session.Unmount] and [Session.Eject] take a volume away, and
// [Session.Watch] delivers an [Event] each time a disk appears or disappears.
//
// # The run loop
//
// DiskArbitration is asynchronous: unmount, eject and the appearance callbacks
// are delivered on a CFRunLoop, and a process that never runs one never hears
// them. [Open] therefore starts one on a dedicated, thread-pinned goroutine —
// github.com/go-macos/objc's Run, not a loop written here — and
// [Session.Close] stops it. Reads ([Session.Describe]) are synchronous and do
// not depend on it, but they are served from the same session so a caller has
// one object to keep and one thing to close.
//
// Callbacks are invoked ON that run-loop thread. A handler that blocks stops
// every other event, including the completion of an unmount somebody is waiting
// for. Hand the event to a channel and return.
//
// # Platforms
//
// macOS only. Every symbol exists on every platform and every operation
// reports [ErrUnsupported] elsewhere, so a consumer cross-compiles without
// build tags of its own. CGO_ENABLED=0 throughout: the framework is reached
// with purego, never cgo.
package diskarbitration
