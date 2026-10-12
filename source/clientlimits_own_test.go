package main

import (
	"strings"
	"testing"
	"time"
)

func TestOwnClientLimit(t *testing.T) {
	// no pool rate at all: only the one client is limited, and a network that is "never limited" does not excuse it
	l := newClientLimits(limCfg(func(d *NTPConfig) {
		d.ClientLimits = []string{"10.1.1.1/32 5"}
		d.ClientExempt = []string{"10.1.0.0/16"}
	}))
	if l == nil {
		t.Fatal("a client limit alone must switch the limiter on")
	}
	t0 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	c := mustAddr("10.1.1.1")
	for i := 0; i < 10; i++ { // burst: twice the rate, at least 10
		if v := l.check(c, t0); v != admitOK {
			t.Fatalf("request %d inside the burst: %v", i, v)
		}
	}
	if v := l.check(c, t0); v != admitLimited {
		t.Fatalf("the 11th at once must be limited, got %v", v)
	}
	if v := l.check(c, t0.Add(time.Second)); v != admitOK { // 5 a second
		t.Fatal("a second later 5 requests are allowed again")
	}
	for i := 0; i < 1000; i++ {
		if v := l.check(mustAddr("10.1.1.2"), t0); v != admitOK {
			t.Fatal("another client is not limited")
		}
	}
}

func TestOwnClientLimitBeatsPoolRate(t *testing.T) {
	l := newClientLimits(limCfg(func(d *NTPConfig) {
		d.ClientRate = 1000
		d.ClientLimits = []string{"192.0.2.0/24 2"}
	}))
	t0 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	n := 0
	for l.check(mustAddr("192.0.2.9"), t0) == admitOK && n < 100 {
		n++
	}
	if n != 10 {
		t.Fatalf("its own limit (burst 10) applies, not the pool's: %d", n)
	}
	n = 0
	for l.check(mustAddr("198.51.100.9"), t0) == admitOK && n < 5000 {
		n++
	}
	if n != 2000 {
		t.Fatalf("others keep the pool's rate (burst 2000): %d", n)
	}
}

func TestClientLimitsValidation(t *testing.T) {
	d := limCfg(func(d *NTPConfig) { d.ClientLimits = []string{"10.1.1.1/32 5", " 10.2.0.0/16   200 ", "2001:db8::5 7"} })
	if got := strings.Join(d.ClientLimits, ","); got != "10.1.1.1 5,10.2.0.0/16 200,2001:db8::5 7" {
		t.Errorf("normalised: %s", got)
	}
	for _, bad := range [][]string{{"10.1.1.1"}, {"10.1.1.1 0"}, {"10.1.1.1 x"}, {"nonsense 5"}, {"10.1.1.1 5 6"}, {"10.1.1.1 5", "10.1.1.1/32 9"}, {"10.1.1.1 99999999"}} {
		x := defaultNTP()
		x.Servers = []string{"127.0.0.1:12399"}
		x.ClientLimits = bad
		if err := x.Validate(); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
