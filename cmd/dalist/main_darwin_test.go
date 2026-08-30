// Copyright (c) 2026, the go-macos authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

//go:build darwin

package main

import (
	"errors"
	"testing"
	"time"

	"github.com/go-macos/diskarbitration"
)

// dalist is read-only, so its tests are simply: run it against the machine and
// require that it works. Nothing here unmounts or ejects anything — the tool
// has no code path that could.

func TestRunModes(t *testing.T) {
	cases := []struct {
		name       string
		long       bool
		mountsOnly bool
		args       []string
	}{
		{"plain", false, false, nil},
		{"long", true, false, nil},
		{"mounts", false, true, nil},
		{"mounts long", true, true, nil},
		{"named", true, false, []string{"disk0"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := run(c.long, c.mountsOnly, 0, c.args); err != nil {
				t.Fatalf("run: %v", err)
			}
		})
	}
}

func TestRunNamedRejectsRubbish(t *testing.T) {
	if err := run(false, false, 0, []string{"not-a-disk"}); err == nil {
		t.Fatal("a name that is not a device must be reported")
	}
}

func TestRunWatch(t *testing.T) {
	// The replay of the disks already present arrives well inside this; the
	// duration is the tool's own stopping condition, not a guess at how long
	// the daemon takes.
	if err := run(false, false, 300*time.Millisecond, nil); err != nil {
		t.Fatalf("run -watch: %v", err)
	}
}

func TestRunReportsAFailedOpen(t *testing.T) {
	boom := errors.New("no daemon")
	old := openSession
	defer func() { openSession = old }()
	openSession = func() (*diskarbitration.Session, error) { return nil, boom }

	if err := run(false, false, 0, nil); !errors.Is(err, boom) {
		t.Fatalf("run = %v, want %v", err, boom)
	}
}

func TestFollowReportsAFailedWatch(t *testing.T) {
	// A closed session cannot register a watch. It is the only way to reach
	// that branch without a broken machine.
	s, err := diskarbitration.Open()
	if err != nil {
		t.Skipf("no session: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := follow(s, time.Second); !errors.Is(err, diskarbitration.ErrClosed) {
		t.Fatalf("follow on a closed session = %v, want ErrClosed", err)
	}
}
