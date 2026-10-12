package main

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

// Only the probes take a server out of service; requests that time out on it do not.
func TestForwardFailuresNeverMarkAServerDown(t *testing.T) {
	odd, good := newFakeNTP(t), newFakeNTP(t)
	odd.only4.Store(true)
	good.delay.Store(int64(5 * time.Millisecond))
	cfg := testNTPCfg(odd.addr, good.addr)
	cfg.FailThreshold = 1
	p := NewPool(cfg)
	p.ProbeNow(context.Background())
	for i := 0; i < 8; i++ {
		q := clientReq(uint64(i + 1))
		q[0] = 3<<3 | ntpModeClient // a version 3 client: the odd server stays silent for it
		if _, _, err := p.Forward(context.Background(), q, netip.Addr{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range p.servers {
		s.mu.Lock()
		h, fc := s.healthy, s.failCount
		s.mu.Unlock()
		if s.Addr == odd.addr && (!h || fc == 0) {
			t.Fatalf("healthy=%v failures counted=%d: timeouts must be counted but not mark the server down", h, fc)
		}
	}
	// the probe is what takes a dead server down
	odd.only4.Store(false)
	odd.mode.Store(2)
	p.ProbeNow(context.Background())
	for _, s := range p.servers {
		if s.Addr == odd.addr {
			s.mu.Lock()
			h := s.healthy
			s.mu.Unlock()
			if h {
				t.Fatal("a server that fails its probe stayed in service")
			}
		}
	}
}
