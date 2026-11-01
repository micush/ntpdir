package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// Self-check.
//
// A node keeps its gateway address and its anycast addresses for as long as its own process says it can answer: the
// gateway runs, chrony is up and keeping time, some upstream server is healthy.  None of that notices a chrony that has
// stopped answering clients while it is still running (a stuck process, a socket nobody reads, a rule that drops
// everything).  The VIP then stays with this node, the anycast routes stay announced, and clients time out.
//
// So every few seconds the node sends each address it serves a real NTP request, from the loopback address (chrony always
// allows 127.0.0.0/8 and ::1 while it is serving, whatever allowed_clients and blocked_clients say, and the address is
// local, so the request really reaches chrony's socket and the answer leaves from that address).  After selfFails
// misses in a row it pauses itself, in memory only, the same as Operate ▸ Node ▸ pause: its gateways resign, the VIP and
// the anycast addresses come off, and the other nodes carry on.  The file is not changed and a restart starts clean.
// After a while (selfHold, doubling up to selfHoldMax while it keeps failing) it lets the gateways start again and
// watches them; selfOKs good answers in a row end the trouble.  A person who pauses or resumes the node takes over.
// Switched by "self_check" in the node's settings (on unless the file says false).
//
// What it does not count as a miss: a moment when chrony is meant to be silent (no server in service, the clock too far
// off, the allow rules not sent yet, chrony not running), and an address that is not on this machine.  A request goes out
// once every few seconds to each address, far under any client_rate (one a second at the least), so the rate limit never
// meets it.

const (
	selfEvery   = 5 * time.Second
	selfTimeout = 2 * time.Second
	selfFails   = 3 // misses in a row before the node pauses itself
	selfOKs     = 6 // answers in a row, after a trial start, before it is trusted again
	selfHold    = time.Minute
	selfHoldMax = 10 * time.Minute
)

var (
	selfPaused atomic.Bool
	selfWhyMu  sync.Mutex
	selfWhy    string
)

func selfPauseActive() bool { return selfPaused.Load() }

func selfPausedWhy() string {
	selfWhyMu.Lock()
	defer selfWhyMu.Unlock()
	if !selfPaused.Load() {
		return ""
	}
	return selfWhy
}

func setSelfPause(on bool, why string) {
	selfWhyMu.Lock()
	selfWhy = why
	selfWhyMu.Unlock()
	selfPaused.Store(on)
}

func clearSelfPause() { setSelfPause(false, "") }

// nodePausedNow is whether the node is out of service: paused by a person (kept in the file) or by the self-check.
func (dc *DaemonConfig) nodePausedNow() bool {
	return dc != nil && (dc.NodePaused || selfPaused.Load())
}

// selfCheckOn says whether the self-check is switched on: it is, unless the file says self_check is false.
func (dc *DaemonConfig) selfCheckOn() bool { return dc == nil || dc.SelfCheck == nil || *dc.SelfCheck }

// isLocalAddr says whether ip is an address of this machine (an address that is not cannot be asked from here).
var isLocalAddr = func(ip netip.Addr) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if p, ok := a.(*net.IPNet); ok {
			if x, ok := netip.AddrFromSlice(p.IP); ok && x.Unmap() == ip.Unmap() {
				return true
			}
		}
	}
	return false
}

// probeNTP asks ap one NTP question, from the loopback address of its family.  ok: a valid answer came back (a kiss-o'-death
// too: chrony is alive); skip: the question could not be sent from here (the address is not on this machine yet), which says
// nothing about chrony.
func probeNTP(ap netip.AddrPort, timeout time.Duration) (ok, skip bool) {
	ip := ap.Addr().Unmap()
	if !isLocalAddr(ip) {
		return true, true
	}
	lo := netip.MustParseAddr("127.0.0.1")
	if ip.Is6() {
		lo = netip.IPv6Loopback()
	}
	d := net.Dialer{Timeout: timeout, LocalAddr: &net.UDPAddr{IP: lo.AsSlice()}}
	c, err := d.Dial("udp", netip.AddrPortFrom(ip, ap.Port()).String())
	if err != nil {
		return true, true
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	req := buildNTPRequest(randomXmit())
	if _, err := c.Write(req); err != nil {
		return true, true
	}
	buf := make([]byte, ntpMaxPacket)
	for {
		n, err := c.Read(buf) // a refused connection (nothing listens on the port) is a miss too
		if err != nil {
			return false, false
		}
		if ntpAnswers(req, buf[:n]) {
			return true, false
		}
	}
}

// selfCheck runs the check.  The functions are filled in by main (and by the tests).
type selfCheck struct {
	enabled func() bool
	expect  func() bool // chrony is meant to be answering clients right now (not muted, not starting up)
	targets func() []netip.AddrPort
	probe   func(netip.AddrPort) (ok, skip bool)
	guard   func() string                // why the node must not pause itself ("" when it may)
	apply   func(pause bool, why string) // pause or resume this node's gateways now
	now     func() time.Time

	bad, good  int
	paused     bool // paused by this check
	trial      bool // started again to see whether it answers; not yet trusted
	retryAt    time.Time
	hold       time.Duration
	refusedLog bool
}

func (s *selfCheck) tick() {
	now := s.now()
	if s.paused && !selfPauseActive() { // a person resumed or paused the node: this check lets go
		s.paused, s.trial, s.bad, s.good, s.hold = false, false, 0, 0, 0
	}
	if !s.enabled() {
		if s.paused {
			infof("self-check: switched off — resuming this node")
			s.resume()
			s.hold = 0
		}
		s.bad, s.good = 0, 0
		return
	}
	if s.paused {
		if !now.Before(s.retryAt) {
			infof("self-check: starting this node's gateways again to see whether they answer")
			s.resume()
			s.trial, s.bad, s.good = true, 0, 0
		}
		return
	}
	if s.expect != nil && !s.expect() { // chrony is silent on purpose: that says nothing about it
		s.bad = 0
		return
	}
	var miss string
	asked := 0
	for _, ap := range s.targets() {
		ok, skip := s.probe(ap)
		if skip {
			continue
		}
		asked++
		if !ok && miss == "" {
			miss = ap.String()
		}
	}
	if asked == 0 {
		s.bad = 0
		return
	}
	if miss == "" {
		s.bad = 0
		s.good++
		s.refusedLog = false
		if s.trial && s.good >= selfOKs {
			s.trial, s.hold = false, 0
			infof("self-check: this node's NTP answers again")
		}
		return
	}
	s.good = 0
	s.bad++
	if s.bad == 1 {
		warnf("self-check: %s did not answer its own NTP request within %s", miss, selfTimeout)
	}
	if s.bad < selfFails {
		return
	}
	if why := s.guard(); why != "" {
		if !s.refusedLog {
			warnf("self-check: %s has stopped answering, but this node is not pausing itself: %s", miss, why)
			s.refusedLog = true
		}
		s.bad = selfFails
		return
	}
	if s.hold == 0 {
		s.hold = selfHold
	} else if s.hold *= 2; s.hold > selfHoldMax {
		s.hold = selfHoldMax
	}
	why := fmt.Sprintf("%s stopped answering its own NTP requests (%d misses in a row)", miss, s.bad)
	errorf("self-check: %s — pausing this node; it tries again in %s", why, s.hold)
	setSelfPause(true, why)
	s.paused, s.trial, s.bad = true, false, 0
	s.retryAt = now.Add(s.hold)
	s.apply(true, why)
}

func (s *selfCheck) resume() {
	clearSelfPause()
	s.paused = false
	s.apply(false, "")
}

func (s *selfCheck) run(ctx context.Context) {
	t := time.NewTicker(selfEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick()
		}
	}
}

// selfAddrs is the state of the engine and the shared addresses it carries (without prefix lengths).
func (e *Engine) selfAddrs() (state string, addrs []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, v := range e.cfg.vipsFor(e.af) {
		if p, err := netip.ParsePrefix(v); err == nil {
			addrs = append(addrs, p.Addr().String())
		}
	}
	return lower(e.state.String()), addrs
}

// selfCheckTargets lists the addresses this node answers NTP on right now: each gateway's shared addresses while this node
// is the active or a forwarding member, and the anycast addresses that are on lo.
func selfCheckTargets(sup *Supervisor, port int) []netip.AddrPort {
	if port <= 0 || port > 65535 {
		port = 123
	}
	seen := map[netip.AddrPort]bool{}
	var out []netip.AddrPort
	add := func(a string) {
		ip, err := netip.ParseAddr(a)
		if err != nil {
			return
		}
		ap := netip.AddrPortFrom(ip.Unmap(), uint16(port))
		if !seen[ap] {
			seen[ap] = true
			out = append(out, ap)
		}
	}
	for _, e := range sup.engineList() {
		if st, addrs := e.selfAddrs(); st == "active" || st == "forward" {
			for _, a := range addrs {
				add(a)
			}
		}
	}
	anycastReg.Lock()
	for a, on := range anycastReg.onLo {
		if on {
			add(a)
		}
	}
	anycastReg.Unlock()
	return out
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
