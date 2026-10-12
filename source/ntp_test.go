package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// ── wire ─────────────────────────────────────────────────────────────────────

func TestNTPTimestampRoundTrip(t *testing.T) {
	for _, tm := range []time.Time{
		time.Date(2026, 10, 12, 6, 0, 0, 123456000, time.UTC),
		time.Date(1999, 12, 31, 23, 59, 59, 999000000, time.UTC),
		time.Date(2040, 1, 1, 0, 0, 0, 5000, time.UTC), // after the 2036 rollover: era 1
	} {
		got := ntpToTime(ntpTimestamp(tm))
		if d := got.Sub(tm); d < -time.Microsecond || d > time.Microsecond {
			t.Errorf("%v came back as %v (%v off)", tm, got, d)
		}
	}
}

func TestParseNTPAndBuildRequest(t *testing.T) {
	req := buildNTPRequest(0x0102030405060708)
	p, ok := parseNTP(req)
	if !ok || p.Version != 4 || p.Mode != ntpModeClient || p.Xmit != 0x0102030405060708 {
		t.Fatalf("%+v %v", p, ok)
	}
	if _, ok := parseNTP(req[:47]); ok {
		t.Fatal("a packet shorter than the header was parsed")
	}
	if ntpMode(req[:10]) != -1 || ntpVersion(req[:10]) != 0 {
		t.Fatal("short packets have no mode or version")
	}
	if x := randomXmit(); x == 0 || x == randomXmit() {
		t.Fatalf("random transmit timestamps must be non-zero and differ: %x", x)
	}
}

func TestNTPRelayable(t *testing.T) {
	mk := func(v, m byte, n int) []byte {
		b := make([]byte, n)
		b[0] = v<<3 | m
		return b
	}
	for name, c := range map[string]struct {
		pkt  []byte
		want bool
	}{
		"v4 client":        {mk(4, 3, 48), true},
		"v3 client":        {mk(3, 3, 48), true},
		"v1 client":        {mk(1, 3, 48), true},
		"v4 with a MAC":    {mk(4, 3, 68), true},
		"v0":               {mk(0, 3, 48), false},
		"v5":               {mk(5, 3, 48), false},
		"server answer":    {mk(4, 4, 48), false},
		"symmetric active": {mk(4, 1, 48), false},
		"broadcast":        {mk(4, 5, 48), false},
		"control mode 6":   {mk(2, 6, 48), false},
		"private mode 7":   {mk(2, 7, 48), false},
		"short":            {mk(4, 3, 47), false},
		"too long":         {mk(4, 3, ntpMaxPacket+1), false},
	} {
		if got := ntpRelayable(c.pkt); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

func TestNTPAnswersMatchesTheOrigin(t *testing.T) {
	req := buildNTPRequest(0xAAAA5555AAAA5555)
	good := make([]byte, ntpHeaderLen)
	good[0] = 4<<3 | ntpModeServer
	copy(good[24:32], req[40:48])
	if !ntpAnswers(req, good) {
		t.Fatal("the matching answer was refused")
	}
	other := append([]byte(nil), good...)
	other[31] ^= 1
	if ntpAnswers(req, other) {
		t.Fatal("an answer to another request was accepted")
	}
	client := append([]byte(nil), good...)
	client[0] = 4<<3 | ntpModeClient
	if ntpAnswers(req, client) {
		t.Fatal("a client-mode packet was taken for an answer")
	}
	if ntpAnswers(req, good[:40]) || ntpAnswers(req[:40], good) {
		t.Fatal("a short packet was accepted")
	}
}

func TestKissOfDeathAndUnsyncResponses(t *testing.T) {
	req := buildNTPRequest(0x1111222233334444)
	req[0] = 3<<3 | ntpModeClient // a version 3 client is answered in version 3
	req[2] = 6
	k := kodResponse(req, kodRate)
	if kodCode(k) != "RATE" || !ntpAnswers(req, k) || ntpVersion(k) != 3 || k[2] != 6 || k[0]>>6 != ntpLeapUnsynch {
		t.Fatalf("kod: % x", k)
	}
	if kodCode(buildNTPRequest(1)) != "" || kodCode(make([]byte, 10)) != "" {
		t.Fatal("a request or a short packet is not a kiss-o'-death")
	}
	u := unsyncResponse(req)
	if u[1] != 16 || u[0]>>6 != ntpLeapUnsynch || !ntpAnswers(req, u) || kodCode(u) != "" {
		t.Fatalf("unsync: % x", u)
	}
	if kodResponse(req[:10], kodRate) != nil || unsyncResponse(req[:10]) != nil {
		t.Fatal("a request too short to answer must get nothing")
	}
	if refIDString(1, [4]byte{'G', 'P', 'S', 0}) != "GPS" || refIDString(2, [4]byte{10, 1, 2, 3}) != "10.1.2.3" || refIDString(0, [4]byte{'R', 'A', 'T', 'E'}) != "RATE" {
		t.Fatal("reference ids")
	}
}

// ── probing ──────────────────────────────────────────────────────────────────

func TestProbeMeasuresOffsetDelayAndStratum(t *testing.T) {
	f := newFakeNTP(t)
	f.stratum.Store(3)
	f.offset.Store(int64(250 * time.Millisecond))
	f.rootDel.Store(int64(40 * time.Millisecond))
	f.rootDsp.Store(int64(10 * time.Millisecond))
	s, err := ntpProbe(context.Background(), f.addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if s.Stratum != 3 || s.Leap != 0 || s.Version != 4 || s.RefID != "127.127.1.0" {
		t.Fatalf("%+v", s)
	}
	if d := s.Offset - 250*time.Millisecond; d < -20*time.Millisecond || d > 20*time.Millisecond {
		t.Fatalf("offset %v, want about 250ms", s.Offset)
	}
	if s.Delay < 0 || s.Delay > 100*time.Millisecond || s.RTT <= 0 {
		t.Fatalf("delay %v rtt %v", s.Delay, s.RTT)
	}
	// root distance: root delay/2 + root dispersion + delay/2
	if min := 30 * time.Millisecond; s.RootDist < min || s.RootDist > min+50*time.Millisecond {
		t.Fatalf("root distance %v", s.RootDist)
	}
}

func TestProbeIgnoresAnAnswerToAnotherRequest(t *testing.T) {
	f := newFakeNTP(t)
	f.mode.Store(fakeWrongOrigin)
	if _, err := ntpProbe(context.Background(), f.addr, 200*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("an answer with the wrong origin timestamp must be ignored, got %v", err)
	}
}

func TestProbeSeesKissOfDeathAndTimeouts(t *testing.T) {
	f := newFakeNTP(t)
	f.mode.Store(fakeKoD)
	s, err := ntpProbe(context.Background(), f.addr, time.Second)
	if err != nil || s.KoD != "DENY" {
		t.Fatalf("%+v %v", s, err)
	}
	f.mode.Store(fakeDrop)
	if _, err := ntpProbe(context.Background(), f.addr, 150*time.Millisecond); err == nil {
		t.Fatal("a silent server answered")
	}
}

func TestJudge(t *testing.T) {
	c := defaultNTP()
	ok := ntpSample{Stratum: 2, xmit: 1, recv: 1, RootDist: 10 * time.Millisecond}
	if err := c.judge(ok); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*ntpSample){
		"kod":            func(s *ntpSample) { s.KoD = "RATE" },
		"unsynchronised": func(s *ntpSample) { s.Leap = ntpLeapUnsynch },
		"stratum 16":     func(s *ntpSample) { s.Stratum = 16 },
		"no timestamps":  func(s *ntpSample) { s.xmit = 0 },
		"too deep":       func(s *ntpSample) { s.Stratum = 15; c.MaxStratum = 10 },
		"root distance":  func(s *ntpSample) { s.RootDist = 2 * time.Second },
	} {
		c = defaultNTP()
		s := ok
		mod(&s)
		if c.judge(s) == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// ── pool ─────────────────────────────────────────────────────────────────────

func TestRankedFastestFirst(t *testing.T) {
	slow, mid, fast := newFakeNTP(t), newFakeNTP(t), newFakeNTP(t)
	slow.delay.Store(int64(80 * time.Millisecond))
	mid.delay.Store(int64(30 * time.Millisecond))
	p := NewPool(testNTPCfg(slow.addr, mid.addr, fast.addr))
	p.ProbeNow(context.Background())
	got := rankedAddrs(p)
	want := []string{fast.addr, mid.addr, slow.addr}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("ranking = %v, want %v", got, want)
	}
	// the spread band is off in this test: traffic goes to the fastest server only
	cfg := testNTPCfg(slow.addr, mid.addr, fast.addr)
	cfg.Spread = false
	p = NewPool(cfg)
	p.ProbeNow(context.Background())
	sh, mh, fh := slow.hits.Load(), mid.hits.Load(), fast.hits.Load()
	for i := 0; i < 5; i++ {
		req := clientReq(uint64(0x1234 + i))
		resp, via, err := p.Forward(context.Background(), req, mustAddr("192.0.2.10"))
		if err != nil || via != fast.addr || !ntpAnswers(req, resp) {
			t.Fatalf("%v via %s", err, via)
		}
	}
	if slow.hits.Load() != sh || mid.hits.Load() != mh {
		t.Fatalf("slow/mid got forwarded traffic: slow=%d mid=%d", slow.hits.Load()-sh, mid.hits.Load()-mh)
	}
	if fast.hits.Load() != fh+5 {
		t.Fatalf("fast hits = %d, want %d", fast.hits.Load(), fh+5)
	}
}

func TestForwardRelaysTheAnswerUnchanged(t *testing.T) {
	f := newFakeNTP(t)
	f.stratum.Store(1)
	p := NewPool(testNTPCfg(f.addr))
	p.ProbeNow(context.Background())
	req := clientReq(0xFEEDFACECAFEBEEF)
	req[0] = 3<<3 | ntpModeClient
	resp, _, err := p.Forward(context.Background(), req, mustAddr("192.0.2.10"))
	if err != nil {
		t.Fatal(err)
	}
	// what the server saw is exactly what the client sent (the proxy changes nothing), and the answer comes back whole
	seen, _ := f.lastReq.Load().([]byte)
	if !bytes.Equal(seen, req) {
		t.Fatalf("the request was changed on the way:\n sent % x\n seen % x", req, seen)
	}
	if pk, _ := parseNTP(resp); pk.Stratum != 1 || pk.Version != 3 || pk.Orig != 0xFEEDFACECAFEBEEF {
		t.Fatalf("%+v", pk)
	}
}

func TestLatencyReordersAfterChange(t *testing.T) {
	a, b := newFakeNTP(t), newFakeNTP(t)
	b.delay.Store(int64(40 * time.Millisecond))
	p := NewPool(testNTPCfg(a.addr, b.addr))
	p.ProbeNow(context.Background())
	if r := rankedAddrs(p); r[0] != a.addr {
		t.Fatalf("want a first: %v", r)
	}
	a.delay.Store(int64(120 * time.Millisecond))
	b.delay.Store(0)
	for i := 0; i < 4; i++ {
		p.ProbeNow(context.Background())
	}
	if r := rankedAddrs(p); r[0] != b.addr {
		t.Fatalf("want b first after change: %v", r)
	}
}

func TestHealthGating(t *testing.T) {
	good, kod, dead, unsync, deep := newFakeNTP(t), newFakeNTP(t), newFakeNTP(t), newFakeNTP(t), newFakeNTP(t)
	kod.mode.Store(fakeKoD)
	dead.mode.Store(fakeDrop)
	unsync.mode.Store(fakeUnsync)
	deep.stratum.Store(16)
	p := NewPool(testNTPCfg(good.addr, kod.addr, dead.addr, unsync.addr, deep.addr))
	p.ProbeNow(context.Background())
	if r := rankedAddrs(p); len(r) != 1 || r[0] != good.addr {
		t.Fatalf("only the good server should be eligible, got %v", r)
	}
	for _, s := range p.Snapshot() {
		if s.Addr != good.addr && (s.Healthy || s.LastError == "") {
			t.Fatalf("bad server %s not reported: %+v", s.Addr, s)
		}
	}
	// the kiss-o'-death server recovers and joins on the next round
	kod.mode.Store(fakeOK)
	p.ProbeNow(context.Background())
	if len(rankedAddrs(p)) != 2 {
		t.Fatalf("recovered server not re-admitted: %v", rankedAddrs(p))
	}
}

func TestFailThresholdKeepsAServerUntilItFailsEnoughTimes(t *testing.T) {
	f, g := newFakeNTP(t), newFakeNTP(t)
	cfg := testNTPCfg(f.addr, g.addr)
	cfg.FailThreshold = 3
	p := NewPool(cfg)
	p.ProbeNow(context.Background())
	f.mode.Store(fakeDrop)
	for i := 1; i <= 2; i++ {
		p.ProbeNow(context.Background())
		if len(rankedAddrs(p)) != 2 {
			t.Fatalf("taken out after %d failed probe(s), the threshold is 3", i)
		}
	}
	p.ProbeNow(context.Background())
	if r := rankedAddrs(p); len(r) != 1 || r[0] != g.addr {
		t.Fatalf("still in service after 3 failed probes: %v", r)
	}
}

func TestForwardFailoverAndMarkDown(t *testing.T) {
	fast, slow := newFakeNTP(t), newFakeNTP(t)
	slow.delay.Store(int64(20 * time.Millisecond))
	cfg := testNTPCfg(fast.addr, slow.addr)
	cfg.Spread = false
	p := NewPool(cfg)
	p.ProbeNow(context.Background())

	fast.mode.Store(fakeDrop) // the fast server dies between probes
	req := clientReq(7)
	resp, via, err := p.Forward(context.Background(), req, mustAddr("192.0.2.10"))
	if err != nil || via != slow.addr || !ntpAnswers(req, resp) {
		t.Fatalf("expected failover to the slow server: %v via %s", err, via)
	}
	// the failed request does not demote it; the next probe does (fail_threshold=1)
	if r := rankedAddrs(p); len(r) != 2 {
		t.Fatalf("a failed request took the server out of service: %v", r)
	}
	p.ProbeNow(context.Background())
	if r := rankedAddrs(p); len(r) != 1 || r[0] != slow.addr {
		t.Fatalf("dead server still ranked: %v", r)
	}
}

func TestKissOfDeathTriesTheNextServer(t *testing.T) {
	a, b := newFakeNTP(t), newFakeNTP(t)
	b.delay.Store(int64(20 * time.Millisecond))
	cfg := testNTPCfg(a.addr, b.addr)
	cfg.Spread = false
	p := NewPool(cfg)
	p.ProbeNow(context.Background())
	a.mode.Store(fakeKoD)
	req := clientReq(9)
	resp, via, err := p.Forward(context.Background(), req, mustAddr("192.0.2.10"))
	if err != nil || via != b.addr || kodCode(resp) != "" {
		t.Fatalf("expected b's good answer: %v via %s kod %q", err, via, kodCode(resp))
	}
	// when every server refuses, the client gets the kiss-o'-death (it tells it to back off) rather than silence
	b.mode.Store(fakeKoD)
	resp, _, err = p.Forward(context.Background(), req, mustAddr("192.0.2.10"))
	if err != nil || kodCode(resp) != "DENY" || !ntpAnswers(req, resp) {
		t.Fatalf("all servers refuse: %v kod %q", err, kodCode(resp))
	}
}

func TestNoHealthyServers(t *testing.T) {
	dead := newFakeNTP(t)
	dead.mode.Store(fakeDrop)
	p := NewPool(testNTPCfg(dead.addr))
	p.ProbeNow(context.Background())
	if _, _, err := p.Forward(context.Background(), clientReq(1), mustAddr("192.0.2.10")); err != errNoServers {
		t.Fatalf("err = %v", err)
	}
}

func TestFalsetickerIsTakenOut(t *testing.T) {
	a, b, c, liar := newFakeNTP(t), newFakeNTP(t), newFakeNTP(t), newFakeNTP(t)
	liar.offset.Store(int64(5 * time.Second)) // five seconds ahead of the other three
	cfg := testNTPCfg(a.addr, b.addr, c.addr, liar.addr)
	p := NewPool(cfg)
	p.ProbeNow(context.Background())
	r := rankedAddrs(p)
	if len(r) != 3 {
		t.Fatalf("the falseticker must not be ranked: %v", r)
	}
	var seen bool
	for _, s := range p.Snapshot() {
		if s.Addr == liar.addr {
			seen = true
			if !s.Falseticker || s.Healthy || !strings.Contains(s.LastError, "falseticker") || s.Time == nil || s.Time.OffsetMS < 4000 {
				t.Fatalf("%+v", s)
			}
		} else if s.Falseticker || !s.Healthy {
			t.Fatalf("%s wrongly judged: %+v", s.Addr, s)
		}
	}
	if !seen {
		t.Fatal("the liar is not in the snapshot")
	}
	// it corrects its clock and comes back
	liar.offset.Store(0)
	p.ProbeNow(context.Background())
	if len(rankedAddrs(p)) != 4 {
		t.Fatalf("the corrected server did not come back: %v", rankedAddrs(p))
	}
	// the check needs three servers: with two, a difference cannot say which one is right
	two := NewPool(testNTPCfg(a.addr, liar.addr))
	liar.offset.Store(int64(5 * time.Second))
	two.ProbeNow(context.Background())
	if len(rankedAddrs(two)) != 2 {
		t.Fatalf("with two servers neither can be called a falseticker: %v", rankedAddrs(two))
	}
	// and it can be switched off
	cfg.MaxOffsetMS = 0
	off := NewPool(cfg)
	off.ProbeNow(context.Background())
	if len(rankedAddrs(off)) != 4 {
		t.Fatalf("max_offset_ms 0 must switch the check off: %v", rankedAddrs(off))
	}
}

func TestSnapshotCarriesWhatTheServerSaid(t *testing.T) {
	f := newFakeNTP(t)
	f.stratum.Store(2)
	p := NewPool(testNTPCfg(f.addr))
	p.ProbeNow(context.Background())
	s := p.Snapshot()
	if len(s) != 1 || s[0].Time == nil || s[0].Time.Stratum != 2 || s[0].Time.Version != 4 || !s[0].Healthy || s[0].Rank != 1 {
		t.Fatalf("%+v", s)
	}
}

func TestNeverForwardsToTheAsker(t *testing.T) {
	// a request that comes from a server of the pool is not sent back to it (a loop); the others still get it
	a := &Server{Addr: "192.0.2.7:123"}
	b := &Server{Addr: "192.0.2.8:123"}
	got := dropAsker([]*Server{a, b}, mustAddr("192.0.2.7"))
	if len(got) != 1 || got[0] != b {
		t.Fatalf("the asker was not dropped: %v", got)
	}
	if got := dropAsker([]*Server{a, b}, mustAddr("192.0.2.9")); len(got) != 2 {
		t.Fatal("a stranger lost a server")
	}
	// a local program is not a server forwarding
	l := &Server{Addr: "127.0.0.1:123"}
	if got := dropAsker([]*Server{l}, mustAddr("127.0.0.1")); len(got) != 1 {
		t.Fatal("a request from this machine was treated as a loop")
	}
}

func TestLogServerFlips(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)
	f := newFakeNTP(t)
	p := NewPool(testNTPCfg(f.addr))
	p.ProbeNow(context.Background())
	if !strings.Contains(buf.String(), "is UP") {
		t.Fatalf("no UP line: %s", buf.String())
	}
	buf.Reset()
	p.ProbeNow(context.Background())
	if strings.Contains(buf.String(), "is UP") || strings.Contains(buf.String(), "is DOWN") {
		t.Fatalf("steady state logged: %s", buf.String())
	}
	f.mode.Store(fakeDrop)
	p.ProbeNow(context.Background())
	if !strings.Contains(buf.String(), "is DOWN") {
		t.Fatalf("no DOWN line: %s", buf.String())
	}
}

// ── frontend ─────────────────────────────────────────────────────────────────

func TestFrontendServesUDPAndAnswersUnsynchronisedWhenNothingIsUp(t *testing.T) {
	up := newFakeNTP(t)
	p := NewPool(testNTPCfg(up.addr))
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

	req := clientReq(0xBEEF00000000BEEF)
	resp, _, err := exchange(context.Background(), srv, req, time.Second)
	if err != nil || !ntpAnswers(req, resp) || kodCode(resp) != "" {
		t.Fatalf("udp: %v % x", err, resp)
	}
	if pk, _ := parseNTP(resp); pk.Stratum != 2 {
		t.Fatalf("the upstream's answer must come back as it is: %+v", pk)
	}
	// every upstream down: the client is told at once that this server is not synchronised, not left to time out
	up.mode.Store(fakeDrop)
	p.ProbeNow(context.Background())
	start := time.Now()
	resp, _, err = exchange(context.Background(), srv, req, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if pk, _ := parseNTP(resp); pk.Stratum != 16 || pk.Leap != ntpLeapUnsynch || !ntpAnswers(req, resp) {
		t.Fatalf("want an unsynchronised answer, got %+v", pk)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("the unsynchronised answer was not immediate")
	}
	// a control-mode packet gets no answer at all
	ctl := append([]byte(nil), req...)
	ctl[0] = 2<<3 | 6
	if r, _, err := exchange(context.Background(), srv, ctl, 150*time.Millisecond); err == nil {
		t.Fatalf("mode 6 was answered: % x", r)
	}
}

func TestConfigParsing(t *testing.T) {
	var d NTPConfig
	err := d.UnmarshalJSON([]byte(`{"servers":["10.0.0.123","[2001:db8::123]:1123","ntp.example.com"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if d.Servers[0] != "10.0.0.123:123" || d.Servers[1] != "[2001:db8::123]:1123" || d.Servers[2] != "ntp.example.com:123" {
		t.Fatalf("servers: %v", d.Servers)
	}
	if d.ListenPort != 123 || d.MaxStratum != 15 || d.MaxOffsetMS != 128 || !d.Spread {
		t.Fatalf("defaults: %+v", d)
	}
	if err := d.UnmarshalJSON([]byte(`{"serverz":[]}`)); err == nil {
		t.Fatal("typo'd key accepted")
	}
	for name, mod := range map[string]func(*NTPConfig){
		"bad server":   func(c *NTPConfig) { c.Servers = []string{"not a host!"} },
		"bad port":     func(c *NTPConfig) { c.Servers = []string{"10.0.0.1:99999"} },
		"stratum 0":    func(c *NTPConfig) { c.MaxStratum = 0 },
		"stratum 16":   func(c *NTPConfig) { c.MaxStratum = 16 },
		"negative off": func(c *NTPConfig) { c.MaxOffsetMS = -1 },
		"dup server":   func(c *NTPConfig) { c.Servers = []string{"10.0.0.1", "10.0.0.1:123"} },
	} {
		x := defaultNTP()
		mod(&x)
		if x.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// the listen port 53 and the like are fine: only 1-65535 is checked
	x := defaultNTP()
	x.ListenPort = 0
	if x.Validate() == nil {
		t.Error("listen port 0 accepted")
	}
}

var _ = binary.BigEndian
