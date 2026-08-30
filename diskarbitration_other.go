// Copyright (c) 2026, the go-macos authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

//go:build !darwin

package diskarbitration

// init points the seams at stubs on platforms with no DiskArbitration
// framework. [Open] is the only door into the package, so making it report
// [ErrUnsupported] is enough to keep every other operation out of reach — but
// the rest are still assigned rather than left nil, because a nil seam is a
// panic waiting for the day somebody adds a path that reaches one before Open.
//
// Note what is NOT stubbed out: the BSD-name grammar, the ordering, the
// description typing, the DAReturn vocabulary and the closed-session guard.
// Those live in the portable file and behave here exactly as they do on macOS,
// which is what lets every branch of them be tested on a runner with no macOS
// anywhere — including under qemu on architectures Apple never shipped.
func init() {
	sessionOpen = func() (handle, error) { return 0, ErrUnsupported }
	sessionClose = func(handle) {}
	diskDescribe = func(handle, string) (map[string]any, error) { return nil, ErrUnsupported }
	diskUnmount = func(handle, string, uint32) error { return ErrUnsupported }
	diskEject = func(handle, string) error { return ErrUnsupported }
	diskWatch = func(handle, func(EventKind, string, map[string]any)) (func(), error) {
		return nil, ErrUnsupported
	}
}
