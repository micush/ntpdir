package main

import "testing"

func TestClampCount(t *testing.T) {
	for _, c := range []struct {
		in  uint64
		out uint32
	}{{0, 0}, {5, 5}, {1 << 30, 1 << 30}, {1<<30 + 1, 1 << 30}, {1 << 32, 1 << 30}, {^uint64(0), 1 << 30}} {
		if got := clampCount(c.in); got != c.out {
			t.Errorf("clampCount(%d) = %d, want %d", c.in, got, c.out)
		}
	}
}
