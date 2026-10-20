package main

import (
	"net/netip"
	"testing"
	"time"
)

func TestQStatsDistinctClientsPerPoint(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	s, cur := qsTestStats(t0)
	a, b, c := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"), netip.MustParseAddr("192.0.2.3")
	s.Record(a, srvA, 4, outAnswered)
	s.Record(b, srvA, 4, outKoD)
	*cur = t0.Add(10 * time.Minute)
	s.Record(a, srvA, 4, outAnswered)
	s.Record(c, srvA, 4, outAnswered)
	*cur = t0.Add(11 * time.Minute)

	r := s.Query(t0.Add(-time.Hour), *cur, QFilter{})
	if len(r.ClientsN) != len(r.Total) {
		t.Fatalf("lengths %d vs %d", len(r.ClientsN), len(r.Total))
	}
	at := func(r *QStatsResult, tm time.Time) uint64 { return r.ClientsN[(tm.Unix()-r.Start)/int64(r.Step)] }
	if at(r, t0) != 2 || at(r, t0.Add(10*time.Minute)) != 2 || at(r, t0.Add(5*time.Minute)) != 2 {
		t.Fatalf("counts %v", r.ClientsN)
	}
	if at(r, t0.Add(-time.Minute)) != 0 {
		t.Fatalf("a point before any request must be 0: %v", r.ClientsN)
	}
	kod := s.Query(t0.Add(-time.Hour), *cur, QFilter{Outcome: "kod"})
	if at(kod, t0) != 1 || at(kod, t0.Add(10*time.Minute)) != 0 {
		t.Fatalf("kod counts %v", kod.ClientsN)
	}
	if got := distinctIn(s.fine[:], t0.Unix()/60/qsFineSpan, t0.Add(10*time.Minute).Unix()/60/qsFineSpan, ""); got != 3 {
		t.Fatalf("union over two slots = %d, want 3", got)
	}
}
