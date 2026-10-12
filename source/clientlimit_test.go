package main

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func limCfg(f func(*NTPConfig)) NTPConfig {
	d := defaultNTP()
	d.Servers = []string{"127.0.0.1:12399"}
	f(&d)
	if err := d.Validate(); err != nil {
		panic(err)
	}
	return d
}

func TestClientRateBurstThenRefill(t *testing.T) {
	l := newClientLimits(limCfg(func(d *NTPConfig) { d.ClientRate = 2; d.ClientBurst = 5 }))
	t0 := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	c := mustAddr("192.0.2.7")
	for i := 0; i < 5; i++ {
		if v := l.check(c, t0); v != admitOK {
			t.Fatalf("request %d inside the burst: %v", i, v)
		}
	}
	if v := l.check(c, t0); v != admitLimited {
		t.Fatalf("the 6th at once must be limited, got %v", v)
	}
	if v := l.check(mustAddr("192.0.2.8"), t0); v != admitOK {
		t.Fatal("another client has its own bucket")
	}
	// 2 a second: after 1.5 s there are 3 tokens
	for i := 0; i < 3; i++ {
		if v := l.check(c, t0.Add(1500*time.Millisecond)); v != admitOK {
			t.Fatalf("refilled request %d: %v", i, v)
		}
	}
	if v := l.check(c, t0.Add(1500*time.Millisecond)); v != admitLimited {
		t.Fatal("only 3 tokens had come back")
	}
	// a long pause refills to the burst, never beyond it
	n := 0
	for l.check(c, t0.Add(time.Hour)) == admitOK {
		n++
		if n > 100 {
			break
		}
	}
	if n != 5 {
		t.Fatalf("after a pause the client gets exactly the burst, got %d", n)
	}
}

func TestClientKeyIsAnIPv6Slash64(t *testing.T) {
	l := newClientLimits(limCfg(func(d *NTPConfig) { d.ClientRate = 1; d.ClientBurst = 1 }))
	t0 := time.Now()
	if l.check(mustAddr("2001:db8:1:2::1"), t0) != admitOK {
		t.Fatal("first")
	}
	if l.check(mustAddr("2001:db8:1:2:ffff::9"), t0) != admitLimited {
		t.Fatal("another address in the same /64 shares the bucket")
	}
	if l.check(mustAddr("2001:db8:1:3::1"), t0) != admitOK {
		t.Fatal("a different /64 has its own")
	}
	// ::ffff:a.b.c.d is the same client as a.b.c.d
	if l.check(mustAddr("198.51.100.4"), t0) != admitOK || l.check(mustAddr("::ffff:198.51.100.4"), t0) != admitLimited {
		t.Fatal("an IPv4-mapped address counts as its IPv4 address")
	}
}

func TestAllowedClientsExemptAndLoopback(t *testing.T) {
	l := newClientLimits(limCfg(func(d *NTPConfig) {
		d.AllowedClients = []string{"10.0.0.0/8", "192.168.1.5", "2001:db8::/32"}
		d.ClientRate = 1
		d.ClientBurst = 1
		d.ClientExempt = []string{"10.9.0.0/16"}
	}))
	t0 := time.Now()
	cases := []struct {
		ip   string
		want admitVerdict
	}{
		{"10.1.2.3", admitOK}, {"192.168.1.5", admitOK}, {"2001:db8:5::1", admitOK},
		{"192.168.1.6", admitDenied}, {"8.8.8.8", admitDenied}, {"2001:db9::1", admitDenied},
		{"127.0.0.1", admitOK}, {"::1", admitOK}, // the node itself: neither listed nor limited
	}
	for _, c := range cases {
		if got := l.check(mustAddr(c.ip), t0); got != c.want {
			t.Errorf("%s: got %v want %v", c.ip, got, c.want)
		}
	}
	for i := 0; i < 20; i++ {
		if l.check(mustAddr("127.0.0.1"), t0) != admitOK || l.check(mustAddr("10.9.1.1"), t0) != admitOK {
			t.Fatal("loopback and exempt networks are never limited")
		}
	}
	if l.check(mustAddr("10.1.2.3"), t0) != admitLimited {
		t.Fatal("a listed client is still rate limited")
	}
}

func TestNoLimitsMeansNoLimiter(t *testing.T) {
	if newClientLimits(limCfg(func(d *NTPConfig) {})) != nil {
		t.Fatal("nothing configured must cost nothing")
	}
	if NewPool(limCfg(func(d *NTPConfig) {})).admit(mustAddr("8.8.8.8")) != admitOK {
		t.Fatal("everyone is allowed by default")
	}
}

func TestLimiterMemoryIsBounded(t *testing.T) {
	l := newClientLimits(limCfg(func(d *NTPConfig) { d.ClientRate = 1; d.ClientBurst = 1 }))
	t0 := time.Now()
	for i := 0; i < limShards*limShardMax*2; i++ {
		l.check(netip.AddrFrom4([4]byte{byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)}), t0)
	}
	if n := l.clients(); n > limShards*limShardMax {
		t.Fatalf("%d clients kept, the bound is %d", n, limShards*limShardMax)
	}
}

func TestClientConfigValidation(t *testing.T) {
	d := limCfg(func(d *NTPConfig) {
		d.AllowedClients = []string{"10.1.2.3/8", "10.0.0.0/8", "::ffff:192.0.2.1", "192.0.2.1"}
		d.ClientAction = "drop"
	})
	if len(d.AllowedClients) != 2 || d.AllowedClients[0] != "10.0.0.0/8" || d.AllowedClients[1] != "192.0.2.1/32" {
		t.Fatalf("normalised/deduplicated: %v", d.AllowedClients)
	}
	if d.ClientAction != "" {
		t.Fatalf("the default action is not stored: %q", d.ClientAction)
	}
	for name, f := range map[string]func(*NTPConfig){
		"bad network": func(d *NTPConfig) { d.AllowedClients = []string{"not-an-ip"} },
		"bad exempt":  func(d *NTPConfig) { d.ClientExempt = []string{"10.0.0.0/33"} },
		"bad action":  func(d *NTPConfig) { d.ClientAction = "ban" },
		"neg rate":    func(d *NTPConfig) { d.ClientRate = -1 },
		"neg burst":   func(d *NTPConfig) { d.ClientBurst = -5 },
	} {
		x := defaultNTP()
		x.Servers = []string{"127.0.0.1:12399"}
		f(&x)
		if x.Validate() == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// a config without the keys writes none of them (older versions refuse unknown keys)
	b, _ := json.Marshal(defaultNTP())
	for _, k := range []string{"allowed_clients", "client_rate", "client_burst", "client_action", "client_exempt"} {
		if strings.Contains(string(b), `"`+k+`"`) {
			t.Errorf("%s is written for a config that does not use it", k)
		}
	}
}

// through the frontend: the list and the rate apply before anything is forwarded
func TestFrontendRefusesAndLimits(t *testing.T) {
	up := newFakeNTP(t)
	run := func(f func(*NTPConfig)) (*NTPFrontend, *Pool) {
		c := testNTPCfg(up.addr)
		f(&c)
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		p := NewPool(c)
		p.ProbeNow(context.Background())
		fe := NewNTPFrontend(mustAddr("127.0.0.2"), 0, func() *Pool { return p })
		fe.ctx, fe.cancel = context.WithCancel(context.Background())
		t.Cleanup(fe.cancel)
		return fe, p
	}
	q := clientReq(0xAABBCCDD11223344)
	out := mustAddr("203.0.113.9")

	// not on the list: a kiss-o'-death RSTR that carries the request's transmit time, nothing reaches the server; the
	// node itself is fine
	fe, p := run(func(c *NTPConfig) { c.AllowedClients = []string{"10.0.0.0/8"} })
	before := up.hits.Load()
	r := fe.resolve(q, out)
	if kodCode(r) != kodRestricted || !ntpAnswers(q, r) {
		t.Fatalf("not allowed must be answered RSTR (with the origin timestamp), got % x", r)
	}
	if up.hits.Load() != before || p.Denied.Load() != 1 {
		t.Fatalf("refused requests must not reach the server (%d), denied=%d", up.hits.Load()-before, p.Denied.Load())
	}
	if r := fe.resolve(q, mustAddr("10.2.3.4")); kodCode(r) != "" || !ntpAnswers(q, r) {
		t.Fatalf("an allowed client: % x", r)
	}
	if r := fe.resolve(q, mustAddr("127.0.0.1")); kodCode(r) != "" || !ntpAnswers(q, r) {
		t.Fatalf("the node itself: % x", r)
	}
	// modes 6 and 7 are never relayed, listed or not
	ctl := append([]byte(nil), q...)
	ctl[0] = 4<<3 | 6
	if r := fe.resolve(ctl, mustAddr("10.2.3.4")); r != nil {
		t.Fatalf("a control-mode packet must be dropped, got % x", r)
	}

	// over the rate: drop (default) sends nothing, kod answers RATE
	for _, action := range []string{"", actKoD} {
		fe, p := run(func(c *NTPConfig) { c.ClientRate = 1; c.ClientBurst = 2; c.ClientAction = action })
		for i := 0; i < 2; i++ {
			if r := fe.resolve(q, out); kodCode(r) != "" || !ntpAnswers(q, r) {
				t.Fatalf("%q: inside the burst: % x", action, r)
			}
		}
		r := fe.resolve(q, out)
		switch action {
		case "":
			if r != nil {
				t.Fatalf("drop must send nothing, got % x", r)
			}
		case actKoD:
			if kodCode(r) != kodRate || !ntpAnswers(q, r) {
				t.Fatalf("kod must answer RATE, got % x", r)
			}
		}
		if p.Limited.Load() != 1 {
			t.Fatalf("%q: limited=%d", action, p.Limited.Load())
		}
	}
}

func TestRefusalLogIsThrottled(t *testing.T) {
	l := newClientLimits(limCfg(func(d *NTPConfig) { d.ClientRate = 1 }))
	t0 := time.Now()
	c := mustAddr("192.0.2.1")
	l.noteRefusal(c, "x", t0) // logged at once: pending reset
	if l.pending != 0 {
		t.Fatalf("the first refusal is logged at once, pending=%d", l.pending)
	}
	for i := 1; i <= 50; i++ {
		l.noteRefusal(c, "x", t0.Add(time.Duration(i)*time.Second/2))
	}
	if l.pending != 50 {
		t.Fatalf("within a minute only count: pending=%d", l.pending)
	}
	l.noteRefusal(c, "x", t0.Add(61*time.Second))
	if l.pending != 0 {
		t.Fatalf("after a minute the next one logs and resets: pending=%d", l.pending)
	}
}
