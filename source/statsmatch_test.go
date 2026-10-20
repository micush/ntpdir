package main

import (
	"fmt"
	"net/netip"
	"testing"
	"time"
)

func sumOf(v []uint64) (n uint64) {
	for _, x := range v {
		n += x
	}
	return
}

func listSum(l []NameCount) (n uint64) {
	for _, e := range l {
		n += e.Count
	}
	return
}

// The number cards, the chart series and the lists must tell the same story over a range that is fully
// counted: every card is the sum of its chart series, the outcomes add up to the total, every list adds up to
// the total (or, with an outcome picked, to that outcome's card), and the last point of the Clients line is
// the Clients card.
func TestStatsCardsChartAndListsAgree(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	s, cur := qsTestStats(t0)
	outs := []int{outAnswered, outAnswered, outAnswered, outRefused, outKoD, outFailed}
	n := 0
	for m := 0; m < 90; m++ {
		*cur = t0.Add(time.Duration(m) * time.Minute)
		for i := 0; i < 7; i++ {
			c := netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", 1+(m*7+i)%40))
			s.Record(c, fmt.Sprintf("10.0.0.%d:123", (m+i)%5), 3+(i%2), outs[(m+i)%len(outs)])
			n++
		}
	}
	*cur = t0.Add(95 * time.Minute)
	for _, hours := range []time.Duration{3, 30} { // the 10-minute tier and a coarser step
		from := t0.Add(-time.Minute)
		to := *cur
		if hours == 30 {
			*cur = t0.Add(26 * time.Hour)
			to = *cur
		}
		r := s.Query(from, to, QFilter{})
		sm := r.Sums
		for name, pair := range map[string][2]uint64{
			"total": {sumOf(r.Total), sm.Total}, "answered": {sumOf(r.Answered), sm.Answered}, "failed": {sumOf(r.Failed), sm.Failed},
			"kod": {sumOf(r.KoD), sm.KoD}, "refused": {sumOf(r.Refused), sm.Refused},
		} {
			if pair[0] != pair[1] {
				t.Errorf("%dh %s: the chart adds up to %d, the card says %d", hours, name, pair[0], pair[1])
			}
		}
		if sm.Total != uint64(n) {
			t.Errorf("%dh total card %d, recorded %d", hours, sm.Total, n)
		}
		if k := sm.Answered + sm.Failed + sm.KoD + sm.Refused; k != sm.Total {
			t.Errorf("%dh the outcomes add up to %d, the total is %d", hours, k, sm.Total)
		}
		for name, l := range map[string][]NameCount{"clients": r.Clients, "servers": r.Servers, "versions": r.Versions} {
			if got := listSum(l); got != sm.Total {
				t.Errorf("%dh the %s list adds up to %d, the total is %d", hours, name, got, sm.Total)
			}
		}
		if last := r.ClientsN[len(r.ClientsN)-1]; int(last) != sm.Clients {
			t.Errorf("%dh the Clients line ends at %d, the card says %d", hours, last, sm.Clients)
		}
		for kind, want := range map[string]uint64{"answered": sm.Answered, "failed": sm.Failed, "kod": sm.KoD, "refused": sm.Refused} {
			k := s.Query(from, to, QFilter{Outcome: kind})
			if got := listSum(k.Clients); got != want {
				t.Errorf("%dh %s: the client list adds up to %d, its card says %d", hours, kind, got, want)
			}
			if got := listSum(k.Servers); got != want {
				t.Errorf("%dh %s: the server list adds up to %d, its card says %d", hours, kind, got, want)
			}
			if last := k.ClientsN[len(k.ClientsN)-1]; int(last) != k.Sums.Clients {
				t.Errorf("%dh %s: the Clients line ends at %d, the card says %d", hours, kind, last, k.Sums.Clients)
			}
		}
	}
}
