//go:build pprof

package main

import "testing"

// The profiling listener serves secrets to anyone who can reach it, so only
// a loopback IP literal is accepted.
func TestProfilingListensOnLoopbackOnly(t *testing.T) {
	for address, want := range map[string]bool{
		"127.0.0.1:6060": true, "[::1]:6060": true, "127.0.0.2:0": true,
		":6060": false, "0.0.0.0:6060": false, "[::]:6060": false, "localhost:6060": false, "192.168.1.5:6060": false,
		"127.0.0.1": false, "": false,
	} {
		if got := loopback(address); got != want {
			t.Errorf("%q: %v", address, got)
		}
	}
}
