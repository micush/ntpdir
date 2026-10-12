package main

import (
	"errors"
	"net/netip"
	"testing"
)

func TestForwardFailuresKeepTheLastOnesWithWhoAsked(t *testing.T) {
	fwdFails.mu.Lock()
	fwdFails.ring, fwdFails.next, fwdFails.n = nil, 0, 0
	fwdFails.mu.Unlock()
	q := clientReq(0x1234)
	for i := 0; i < fwdFailKeep+5; i++ {
		noteFwdFailure(q, netip.MustParseAddr("10.1.2.3"), "10.20.0.196:123", errors.New("i/o timeout"))
	}
	r := recentFwdFailures()
	last := r["last"].([]fwdFailure)
	if len(last) != fwdFailKeep || r["total_since_start"].(uint64) != uint64(fwdFailKeep+5) {
		t.Fatalf("kept %d of %v", len(last), r["total_since_start"])
	}
	if last[0].Client != "10.1.2.3" || last[0].Server != "10.20.0.196:123" || last[0].Version != 4 {
		t.Fatalf("%+v", last[0])
	}
}
