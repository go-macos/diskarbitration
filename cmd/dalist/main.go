// Copyright (c) 2026, the go-macos authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

// Command dalist prints what DiskArbitration knows about this machine's disks.
//
// It is the package's own proof of life, and it is READ-ONLY: it never
// unmounts, ejects or writes anything. Use it to see the values a consumer will
// receive.
//
//	dalist              # one line per device
//	dalist -l           # every description key, verbatim
//	dalist -mounts      # only the volumes that are mounted
//	dalist -watch 10s   # follow appearances and disappearances
//	dalist disk3s1s1    # just these devices
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/go-macos/diskarbitration"
)

// openSession is a seam: a test makes it fail to reach the branch that reports
// a session that could not be opened, which on a working macOS never happens.
var openSession = diskarbitration.Open

func main() {
	long := flag.Bool("l", false, "print every description key")
	mounts := flag.Bool("mounts", false, "only volumes that are mounted")
	watch := flag.Duration("watch", 0, "follow disk appearances for this long")
	flag.Parse()

	if err := run(*long, *mounts, *watch, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "dalist:", err)
		os.Exit(1)
	}
}

func run(long, mountsOnly bool, watch time.Duration, args []string) error {
	s, err := openSession()
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	if watch > 0 {
		return follow(s, watch)
	}

	var descs []*diskarbitration.Description
	switch {
	case len(args) > 0:
		for _, name := range args {
			d, err := s.Describe(name)
			if err != nil {
				return err
			}
			descs = append(descs, d)
		}
	case mountsOnly:
		descs, err = s.Mounts()
	default:
		descs, err = s.DescribeAll()
	}
	if err != nil {
		return err
	}

	for _, d := range descs {
		fmt.Println(d)
		if long {
			keys := make([]string, 0, len(d.Raw))
			for k := range d.Raw {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Printf("    %-24s %v\n", k, d.Raw[k])
			}
		}
	}
	return nil
}

// follow prints events for d, then stops. Events arrive on the run-loop thread,
// so the handler does nothing but hand them to a channel.
func follow(s *diskarbitration.Session, d time.Duration) error {
	events := make(chan diskarbitration.Event, 64)
	w, err := s.Watch(func(e diskarbitration.Event) { events <- e })
	if err != nil {
		return err
	}
	defer w.Stop()

	deadline := time.After(d)
	for {
		select {
		case e := <-events:
			fmt.Println(e)
		case <-deadline:
			return nil
		}
	}
}
