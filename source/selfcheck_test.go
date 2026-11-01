package main

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func newTestCheck(t *testing.T) (*selfCheck, *bool, *[]bool, *time.Time) {
	t.Helper()
	clearSelfPause()
	t.Cleanup(clearSelfPause)
	answer := true
	var applied []bool
	clock := time.Unix(1000, 0)
	s := &selfCheck{
		enabled: func() bool { return true },
		targets: func() []netip.AddrPort { return []netip.AddrPort{netip.MustParseAddrPort("10.0.0.1:123")} },
		probe:   func(netip.AddrPort) (bool, bool) { return answer, false },
		guard:   func() string { return "" },
		apply:   func(p bool, _ string) { applied = append(applied, p) },
		now:     func() time.Time { return clock },
	}
	return s, &answer, &applied, &clock
}

func TestSelfCheckPausesAndComesBack(t *testing.T) {
	s, answer, applied, clock := newTestCheck(t)
	for i := 0; i < 5; i++ {
		s.tick()
	}
	if selfPauseActive() || len(*applied) != 0 {
		t.Fatal("a node that answers must not be paused")
	}
	*answer = false
	s.tick()
	s.tick()
	if selfPauseActive() {
		t.Fatal("two misses must not pause it")
	}
	s.tick()
	if !selfPauseActive() || len(*applied) != 1 || !(*applied)[0] {
		t.Fatalf("three misses must pause it: %v", *applied)
	}
	if !(&DaemonConfig{}).nodePausedNow() {
		t.Fatal("the node must now count as paused")
	}
	if got := newDaemonConfig().effective(); got == nil {
		t.Fatal("effective")
	}
	cfg := newDaemonConfig()
	cfg.Groups = []GroupConfig{{GroupID: 1}}
	if !cfg.effective().Groups[0].Paused {
		t.Fatal("a self-paused node must run every gateway paused")
	}
	if cfg.NodePaused {
		t.Fatal("the saved configuration must not change")
	}
	// waits for the hold, then a trial start
	*clock = clock.Add(selfHold - time.Second)
	s.tick()
	if len(*applied) != 1 {
		t.Fatal("it must wait for the hold before starting again")
	}
	*clock = clock.Add(2 * time.Second)
	s.tick()
	if len(*applied) != 2 || (*applied)[1] || selfPauseActive() {
		t.Fatalf("after the hold the gateways start again: %v", *applied)
	}
	// still wedged: pauses again, for twice as long
	s.tick()
	s.tick()
	s.tick()
	if !selfPauseActive() || s.hold != 2*selfHold {
		t.Fatalf("a failed trial pauses again for longer: hold %v", s.hold)
	}
	*clock = clock.Add(3 * selfHold)
	s.tick()
	*answer = true
	for i := 0; i < selfOKs; i++ {
		s.tick()
	}
	if selfPauseActive() || s.trial || s.hold != 0 {
		t.Fatalf("answers in a row end the trouble: trial %v hold %v", s.trial, s.hold)
	}
}

func TestSelfCheckWillNotPauseTheLastNodeAndCanBeSwitchedOff(t *testing.T) {
	s, answer, applied, _ := newTestCheck(t)
	s.guard = func() string { return "it is the only node in service" }
	*answer = false
	for i := 0; i < 10; i++ {
		s.tick()
	}
	if selfPauseActive() || len(*applied) != 0 {
		t.Fatal("the only node in service must not pause itself")
	}
	s.guard = func() string { return "" }
	s.tick()
	if !selfPauseActive() {
		t.Fatal("it pauses once another node can carry on")
	}
	s.enabled = func() bool { return false }
	s.tick()
	if selfPauseActive() || len(*applied) != 2 || (*applied)[1] {
		t.Fatalf("switching the check off resumes the node: %v", *applied)
	}
	clearSelfPause()
}

func TestSelfCheckLetsGoWhenAPersonTakesOver(t *testing.T) {
	s, answer, applied, _ := newTestCheck(t)
	*answer = false
	s.tick()
	s.tick()
	s.tick()
	if !selfPauseActive() {
		t.Fatal("not paused")
	}
	clearSelfPause() // what NodePause does
	*answer = true
	s.tick()
	if s.paused || len(*applied) != 1 {
		t.Fatalf("the check must let go: %v", *applied)
	}
}

// While chrony is silent on purpose (muted, starting up, clock too far off) nothing counts against it.
func TestSelfCheckIgnoresASilenceChronyWasToldToKeep(t *testing.T) {
	s, answer, applied, _ := newTestCheck(t)
	expected := false
	s.expect = func() bool { return expected }
	*answer = false
	for i := 0; i < 10; i++ {
		s.tick()
	}
	if selfPauseActive() || len(*applied) != 0 {
		t.Fatal("a muted chrony must not pause the node")
	}
	// the misses before it was muted do not add up with the ones after
	expected = true
	s.tick()
	s.tick()
	expected = false
	s.tick()
	expected = true
	s.tick()
	s.tick()
	if selfPauseActive() {
		t.Fatal("a break in the misses starts the count again")
	}
	s.tick()
	if !selfPauseActive() {
		t.Fatal("three misses in a row while chrony should answer must pause the node")
	}
}

// A node with nothing to ask (no gateway active here, no address on this machine) is never paused.
func TestSelfCheckWithNoTargetsOrNoLocalAddress(t *testing.T) {
	s, answer, applied, _ := newTestCheck(t)
	*answer = false
	s.targets = func() []netip.AddrPort { return nil }
	for i := 0; i < 10; i++ {
		s.tick()
	}
	s.targets = func() []netip.AddrPort { return []netip.AddrPort{netip.MustParseAddrPort("10.0.0.1:123")} }
	s.probe = func(netip.AddrPort) (bool, bool) { return true, true } // skip: not an address of this machine
	for i := 0; i < 10; i++ {
		s.tick()
	}
	if selfPauseActive() || len(*applied) != 0 {
		t.Fatal("nothing was asked, so nothing was missed")
	}
}

func TestProbeNTP(t *testing.T) {
	ok := newFakeNTP(t)
	ap := netip.MustParseAddrPort(ok.addr)
	if got, skip := probeNTP(ap, time.Second); !got || skip {
		t.Fatalf("an answering server: ok %v skip %v", got, skip)
	}
	ok.mode.Store(fakeKoD) // a kiss-o'-death is an answer: chrony is alive
	if got, skip := probeNTP(ap, time.Second); !got || skip {
		t.Fatalf("a kiss-o'-death is an answer: ok %v skip %v", got, skip)
	}
	ok.mode.Store(2) // reads and never answers
	if got, skip := probeNTP(ap, 300*time.Millisecond); got || skip {
		t.Fatalf("a silent server must fail: ok %v skip %v", got, skip)
	}
	// nothing listens on the port: refused at once, also a miss
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()
	if got, skip := probeNTP(netip.MustParseAddrPort(addr), 300*time.Millisecond); got || skip {
		t.Fatalf("a closed port must fail: ok %v skip %v", got, skip)
	}
	// an address that is not on this machine cannot be asked from here
	if got, skip := probeNTP(netip.MustParseAddrPort("203.0.113.9:123"), 300*time.Millisecond); !got || !skip {
		t.Fatalf("a foreign address is skipped: ok %v skip %v", got, skip)
	}
}

// The question goes out from the loopback address, whatever the destination: a node that allows only some networks, or blocks
// a range, still hears its own check.
func TestProbeNTPComesFromLoopback(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer pc.Close()
	from := make(chan string, 1)
	go func() {
		b := make([]byte, 512)
		n, a, err := pc.ReadFrom(b)
		if err != nil {
			return
		}
		from <- a.(*net.UDPAddr).IP.String()
		r := make([]byte, ntpHeaderLen)
		r[0] = 4<<3 | ntpModeServer
		r[1] = 2
		copy(r[24:32], b[40:48]) // origin = the request's transmit time
		pc.WriteTo(r, a)
		_ = n
	}()
	if got, skip := probeNTP(netip.MustParseAddrPort(pc.LocalAddr().String()), time.Second); !got || skip {
		t.Fatalf("ok %v skip %v", got, skip)
	}
	if ip := <-from; ip != "127.0.0.1" {
		t.Fatalf("the check came from %s, want 127.0.0.1", ip)
	}
}

// The setting is on unless the file says false, and a file that never mentioned it is written back the same.
func TestSelfCheckSetting(t *testing.T) {
	dc := newDaemonConfig()
	if !dc.selfCheckOn() || !(*DaemonConfig)(nil).selfCheckOn() {
		t.Fatal("on by default")
	}
	b, _ := json.Marshal(dc)
	if strings.Contains(string(b), "self_check") {
		t.Fatalf("a config that never used it must not carry the key (older versions reject unknown keys): %s", b)
	}
	off := false
	dc.SelfCheck = &off
	b, _ = json.Marshal(dc)
	if !strings.Contains(string(b), `"self_check":false`) {
		t.Fatalf("off must be written: %s", b)
	}
	back := newDaemonConfig()
	if err := json.Unmarshal(b, back); err != nil || back.selfCheckOn() {
		t.Fatalf("round trip: %v on=%v", err, back.selfCheckOn())
	}
	d1, d2 := newDaemonConfig(), newDaemonConfig()
	d2.SelfCheck = &off
	var found bool
	for _, s := range configSections(d1, d2) {
		if s.Label == "Self-check" && strings.Contains(s.Detail, "off") {
			found = true
		}
	}
	if !found {
		t.Fatal("the history must show the change")
	}
}

// Targets are the addresses a gateway carries while this node serves it, plus the anycast ones on lo.
func TestSelfCheckTargets(t *testing.T) {
	resetAnycast()
	defer resetAnycast()
	dc := newDaemonConfig()
	dc.Groups = nil
	sup := NewSupervisor(context.Background(), dc)
	if got := selfCheckTargets(sup, 0); len(got) != 0 {
		t.Fatalf("nothing running: %v", got)
	}
	anycastReg.Lock()
	anycastReg.onLo["203.0.113.123"], anycastReg.onLo["198.51.100.1"] = true, false
	anycastReg.Unlock()
	got := selfCheckTargets(sup, 1123)
	if len(got) != 1 || got[0].String() != "203.0.113.123:1123" {
		t.Fatalf("anycast on lo: %v", got)
	}
	if got := selfCheckTargets(sup, 0); len(got) != 1 || got[0].Port() != 123 {
		t.Fatalf("default port: %v", got)
	}
}
