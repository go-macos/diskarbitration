// Copyright (c) 2026, the go-macos authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

//go:build !darwin

package diskarbitration

import (
	"errors"
	"testing"
)

// The stubs. They exist so a consumer cross-compiles without build tags of its
// own, and they are tested so that "it compiles elsewhere" is not the whole of
// the claim.

func TestOpenIsUnsupported(t *testing.T) {
	s, err := Open()
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Open = %v, want ErrUnsupported", err)
	}
	if s != nil {
		t.Fatalf("Open returned a session on a platform with no DiskArbitration: %v", s)
	}
}

func TestEverySeamIsUnsupported(t *testing.T) {
	// Open is the only door into the package, so these are not reachable
	// through the public API. They are asserted anyway: a nil seam is a
	// panic waiting for the day somebody adds a path that reaches one before
	// Open, and a stub that answers "no" is the same information without the
	// crash.
	if _, err := sessionOpen(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("sessionOpen = %v", err)
	}
	sessionClose(0) // must not panic
	if _, err := diskDescribe(0, "disk0"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("diskDescribe = %v", err)
	}
	if err := diskUnmount(0, "disk0", 0); !errors.Is(err, ErrUnsupported) {
		t.Errorf("diskUnmount = %v", err)
	}
	if err := diskEject(0, "disk0"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("diskEject = %v", err)
	}
	if _, err := diskWatch(0, func(EventKind, string, map[string]any) {}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("diskWatch = %v", err)
	}
}
