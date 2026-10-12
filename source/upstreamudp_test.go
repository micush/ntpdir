package main

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// udpStub is a UDP server whose answer to each packet is decided by fn (nil result = no answer); it records the
// source address of every packet and how many it saw.
type udpStub struct {
	pc    net.PacketConn
	addr  string
	n     atomic.Int64
	mu    sync.Mutex
	froms map[string]bool
}

func newUDPStub(t *testing.T, fn func(q []byte, reply func([]byte))) *udpStub {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &udpStub{pc: pc, addr: pc.LocalAddr().String(), froms: map[string]bool{}}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			s.n.Add(1)
			s.mu.Lock()
			s.froms[from.String()] = true
			s.mu.Unlock()
			q := append([]byte(nil), buf[:n]...)
			go fn(q, func(r []byte) { pc.WriteTo(r, from) })
		}
	}()
	t.Cleanup(func() { pc.Close(); upstreamSockets.closeAll() })
	return s
}

func answerTo(q []byte) []byte {
	r := make([]byte, ntpHeaderLen)
	r[0] = byte(ntpVersion(q))<<3 | ntpModeServer
	r[1] = 2
	copy(r[24:32], q[40:48])
	now := ntpTimestamp(time.Now())
	binary.BigEndian.PutUint64(r[32:], now)
	binary.BigEndian.PutUint64(r[40:], now)
	return r
}

func TestUpstreamUDPSocketIsReused(t *testing.T) {
	st := newUDPStub(t, func(q []byte, reply func([]byte)) { reply(answerTo(q)) })
	for i := 0; i < 8; i++ {
		q := clientReq(uint64(i + 1))
		resp, _, err := exchange(context.Background(), st.addr, q, time.Second)
		if err != nil || !ntpAnswers(q, resp) {
			t.Fatalf("exchange %d: %v", i, err)
		}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.froms) != 1 {
		t.Fatalf("8 sequential requests used %d source ports, want 1", len(st.froms))
	}
}

// A late answer to another request (what a reused socket can receive) is not this request's answer.
func TestUpstreamUDPIgnoresAnswerToAnotherRequest(t *testing.T) {
	st := newUDPStub(t, func(q []byte, reply func([]byte)) {
		reply(answerTo(clientReq(999))) // a different request's answer
		time.Sleep(20 * time.Millisecond)
		reply(answerTo(q))
	})
	q := clientReq(12345)
	resp, _, err := exchange(context.Background(), st.addr, q, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !ntpAnswers(q, resp) {
		t.Fatalf("took the answer to another request: %x", resp)
	}
}

// A socket that timed out is not put back (its late answer would meet the next request).
func TestUpstreamUDPTimedOutSocketIsNotReused(t *testing.T) {
	var drop atomic.Bool
	drop.Store(true)
	st := newUDPStub(t, func(q []byte, reply func([]byte)) {
		if !drop.Load() {
			reply(answerTo(q))
		}
	})
	if _, _, err := exchange(context.Background(), st.addr, clientReq(1), 80*time.Millisecond); err == nil {
		t.Fatal("expected a timeout")
	}
	drop.Store(false)
	if _, _, err := exchange(context.Background(), st.addr, clientReq(2), time.Second); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.froms) != 2 {
		t.Fatalf("source ports %d, want 2 (the timed-out socket must be discarded)", len(st.froms))
	}
}

// With every worker busy on a slow upstream, further requests still proceed at once instead of queueing behind
// them: 150 requests against an upstream that takes a second all finish in about that time.
func TestSlowUpstreamDoesNotQueueRequestsBehindTheWorkers(t *testing.T) {
	st := newUDPStub(t, func(q []byte, reply func([]byte)) {
		time.Sleep(time.Second)
		reply(answerTo(q))
	})
	cfg := testNTPCfg(st.addr)
	cfg.ProbeTimeoutMS, cfg.QueryTimeoutMS = 5000, 5000
	p := NewPool(cfg)
	p.ProbeNow(context.Background())
	l, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	port := l.LocalAddr().(*net.UDPAddr).Port
	l.Close()
	fe := NewNTPFrontend(mustAddr("127.0.0.1"), port, func() *Pool { return p })
	if err := fe.Start(); err != nil {
		t.Skipf("cannot bind: %v", err)
	}
	defer fe.Stop()
	srv := net.JoinHostPort("127.0.0.1", itoa(port))
	const n = 150
	var wg sync.WaitGroup
	var failed atomic.Int64
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := clientReq(uint64(i+1) << 20)
			resp, _, err := exchange(context.Background(), srv, q, 6*time.Second)
			if err != nil || !ntpAnswers(q, resp) {
				failed.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if failed.Load() != 0 {
		t.Fatalf("%d of %d requests failed", failed.Load(), n)
	}
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Fatalf("%d requests took %v: they queued behind busy workers", n, d)
	}
}

// Another process holding the port without SO_REUSEPORT still makes Start fail.
func TestFrontendStartFailsWhenPortIsTaken(t *testing.T) {
	l, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.LocalAddr().(*net.UDPAddr).Port
	p := NewPool(testNTPCfg("127.0.0.1:9"))
	fe := NewNTPFrontend(mustAddr("127.0.0.1"), port, func() *Pool { return p })
	if err := fe.Start(); err == nil {
		fe.Stop()
		t.Fatal("Start succeeded on a port another socket holds")
	}
}

// The idle sockets live in shards, but a socket given in one is found by a caller that starts in another, and the
// per-upstream cap still holds across all of them.
func TestUpstreamSocketsShardsShareAndCap(t *testing.T) {
	u := newUDPUpstreams()
	mk := func() *net.UDPConn {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	defer u.closeAll()
	c1 := mk()
	u.give("up:123", c1)
	if got := u.take("up:123"); got != c1 {
		t.Fatalf("a socket given must be found by the next caller, whichever shard it starts in (got %v)", got)
	}
	if u.take("up:123") != nil || u.take("other:123") != nil {
		t.Fatal("nothing left to take")
	}
	for i := 0; i < udpIdleMax+40; i++ {
		u.give("up:123", mk())
	}
	n := 0
	for u.take("up:123") != nil {
		n++
	}
	if n == 0 || n > udpIdleMax {
		t.Fatalf("kept %d idle sockets, want between 1 and %d", n, udpIdleMax)
	}
}

// Many goroutines taking and giving at once (run with -race) never hand one socket to two callers.
func TestUpstreamSocketsConcurrent(t *testing.T) {
	u := newUDPUpstreams()
	defer u.closeAll()
	var mu sync.Mutex
	inUse := map[*net.UDPConn]bool{}
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				c := u.take("up:123")
				if c == nil {
					var err error
					if c, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}); err != nil {
						t.Error(err)
						return
					}
				}
				mu.Lock()
				if inUse[c] {
					mu.Unlock()
					t.Error("one socket handed to two callers")
					return
				}
				inUse[c] = true
				mu.Unlock()
				mu.Lock()
				delete(inUse, c)
				mu.Unlock()
				u.give("up:123", c)
			}
		}()
	}
	wg.Wait()
}

// A busy socket is retired after udpMaxUses exchanges, so its source port does not live for ever.
func TestUpstreamSocketIsRetiredAfterManyUses(t *testing.T) {
	st := newUDPStub(t, func(q []byte, reply func([]byte)) { reply(answerTo(q)) })
	for i := 0; i < udpMaxUses+10; i++ {
		if _, _, err := exchange(context.Background(), st.addr, clientReq(uint64(i+1)), time.Second); err != nil {
			t.Fatal(err)
		}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.froms) < 2 {
		t.Fatalf("one source port carried %d exchanges", udpMaxUses+10)
	}
}
