package main

import (
	"log"
	"runtime/debug"
)

// safeGo runs fn in a new goroutine with a panic-recovery wrapper. A panic
// in a long-running background goroutine (ticker, watcher, stats collector,
// stream reader) would otherwise kill the whole process. The `name` tag is
// included in the log so operators can tell which background job misbehaved.
//
// Use this for every `go func() {...}` that represents a long-running
// background job OR a fire-and-forget side-effect. Do NOT use it for
// goroutines where you want panics to propagate (tests, main goroutine).
func safeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("safeGo[%s]: recovered panic: %v\n%s", name, r, debug.Stack())
			}
		}()
		fn()
	}()
}
