package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ── Upstream exchange ────────────────────────────────────────────────────────

var errNoServers = errors.New("no healthy upstream servers")

// exchange sends an NTP request to addr over UDP and returns the answer and the round-trip time.  A reused socket
// per upstream (upstreamudp.go) gives a source port that changes only now and then, and the kernel filters
// everything that is not from the upstream.
func exchange(ctx context.Context, addr string, req []byte, timeout time.Duration) ([]byte, time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	return exchangeUDP(ctx, addr, req, time.Now().Add(timeout))
}

// ntpSample is what one probe of a server found out.
type ntpSample struct {
	RTT       time.Duration // round trip as this side measured it
	Offset    time.Duration // the server's clock minus this node's (positive: the server is ahead)
	Delay     time.Duration // the round trip without the time the server held the packet
	Stratum   uint8
	Leap      uint8
	Version   uint8
	RefID     string
	RootDelay time.Duration
	RootDisp  time.Duration
	RootDist  time.Duration // the largest error the server's time can have: root delay/2 + root dispersion + delay/2
	KoD       string        // the kiss-o'-death code, when that is what came back
	xmit      uint64
	recv      uint64
}

// ntpProbe asks addr for the time on a socket of its own and works out the offset and delay from the four
// timestamps (RFC 5905, section 8).  It returns an error for a server that does not answer; whether the answer is
// good enough is for NTPConfig.judge.
func ntpProbe(ctx context.Context, addr string, timeout time.Duration) (ntpSample, error) {
	var s ntpSample
	c, err := dialUDP(ctx, addr)
	if err != nil {
		return s, err
	}
	defer c.Close()
	deadline := time.Now().Add(timeout)
	c.SetDeadline(deadline)
	req := buildNTPRequest(randomXmit())
	t1 := time.Now()
	if _, err := c.Write(req); err != nil {
		return s, err
	}
	bp := readBufs.Get().(*[]byte)
	defer readBufs.Put(bp)
	buf := *bp
	for {
		n, err := c.Read(buf)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
				return s, errors.New("no answer (timeout)")
			}
			return s, err
		}
		t4 := time.Now()
		if !ntpAnswers(req, buf[:n]) {
			continue // not for this request: keep waiting until the deadline
		}
		p, _ := parseNTP(buf[:n])
		s.RTT = t4.Sub(t1)
		s.Stratum, s.Leap, s.Version = p.Stratum, p.Leap, p.Version
		s.RefID = refIDString(p.Stratum, p.RefID)
		s.RootDelay, s.RootDisp = p.RootDelay, p.RootDisp
		s.xmit, s.recv = p.Xmit, p.Recv
		if p.Stratum == 0 {
			s.KoD = kodCode(buf[:n])
			return s, nil
		}
		if p.Recv != 0 && p.Xmit != 0 {
			t2, t3 := ntpToTime(p.Recv), ntpToTime(p.Xmit)
			hold := t3.Sub(t2)
			if hold < 0 {
				hold = 0
			}
			s.Delay = s.RTT - hold
			if s.Delay < 0 {
				s.Delay = 0
			}
			s.Offset = (t2.Sub(t1) + t3.Sub(t4)) / 2
		} else {
			s.Delay = s.RTT
		}
		s.RootDist = s.RootDelay/2 + s.RootDisp + s.Delay/2
		return s, nil
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// judge says whether a probe's answer is one the server may be used on: it must be a real answer from a
// synchronised clock that is near enough to its own source of time.  The error says why not.
func (c NTPConfig) judge(s ntpSample) error {
	switch {
	case s.KoD != "":
		return fmt.Errorf("kiss-o'-death %s: the server refuses to answer", s.KoD)
	case s.Leap == ntpLeapUnsynch || s.Stratum >= 16:
		return errors.New("the server's clock is not synchronised")
	case s.xmit == 0 || s.recv == 0:
		return errors.New("the answer carries no timestamps")
	case int(s.Stratum) > c.MaxStratum:
		return fmt.Errorf("stratum %d is more than max_stratum (%d)", s.Stratum, c.MaxStratum)
	case c.MaxRootDistanceMS > 0 && s.RootDist > time.Duration(c.MaxRootDistanceMS)*time.Millisecond:
		return fmt.Errorf("root distance %.1f ms is more than max_root_distance_ms (%d)", ms(s.RootDist), c.MaxRootDistanceMS)
	}
	return nil
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// ── Server pool ──────────────────────────────────────────────────────────────

// Server is one upstream NTP server with health and latency state.
type Server struct {
	Addr     string
	Fallback bool // used only while no normal server is healthy

	ipOnce   sync.Once // ip: the address of Addr (invalid for a server named by host)
	ip       netip.Addr
	loopSeen atomic.Int64 // when the "this server sends us its requests" warning was last logged (unix seconds)

	picked atomic.Uint64 // spread: the turn this server was last put first (0 = never)
	hist   *srvSeries    // this server's per-minute history (srvhist.go); shared by every pool that lists the address; nil in a hand-made Server

	mu          sync.Mutex
	healthy     bool
	ewma        float64 // ms, 0 until first sample
	last        float64
	fails       int // consecutive failures
	okCount     uint64
	failCount   uint64
	served      atomic.Uint64 // live requests answered (atomic: counted for every request without taking mu)
	failing     atomic.Bool   // fails > 0: the next good answer must reset it
	tick        atomic.Uint32 // live answers seen, to sample the latency of one in several
	lastErr     string
	lastProbe   time.Time
	info        ntpSample // the last answer a probe got, whatever it was
	haveInfo    bool
	falseticker bool        // the last round found its time far from that of the other servers
	round       *probeRound // this round's result, until the pool has judged all servers together
}

// probeRound is one server's result in the round under way.
type probeRound struct {
	s   ntpSample
	err error
}

// ServerStat is the exported snapshot of a Server.
type ServerStat struct {
	Addr         string  `json:"addr"`
	Name         string  `json:"name,omitempty"` // the name given to the server on the Topology page, if any
	Healthy      bool    `json:"healthy"`
	Fallback     bool    `json:"fallback,omitempty"` // a fallback server: in use only while every normal server is down
	Rank         int     `json:"rank"`               // 1 = fastest healthy; 0 = not eligible
	InBand       bool    `json:"in_band"`            // spread is on and the server is within the band of the fastest: it takes turns
	EWMAMS       float64 `json:"ewma_ms"`
	LastMS       float64 `json:"last_ms"`
	ConsecFails  int     `json:"consec_fails"`
	Successes    uint64  `json:"successes"`
	Failures     uint64  `json:"failures"`
	Served       uint64  `json:"served"`
	LastError    string  `json:"last_error,omitempty"`
	LastProbeAgo float64 `json:"last_probe_ago_s"`
	// What the last probe found.  Absent until the server has answered once.
	Time *TimeStat `json:"time,omitempty"`
	// Falseticker: its time differs from that of the other servers by more than max_offset_ms.
	Falseticker bool `json:"falseticker,omitempty"`
}

// TimeStat is what a server's last answer said about its clock.
type TimeStat struct {
	Stratum    int     `json:"stratum"`
	Leap       int     `json:"leap"`
	Version    int     `json:"version"`
	RefID      string  `json:"refid"`
	OffsetMS   float64 `json:"offset_ms"`
	DelayMS    float64 `json:"delay_ms"`
	RootDelay  float64 `json:"root_delay_ms"`
	RootDisp   float64 `json:"root_dispersion_ms"`
	RootDist   float64 `json:"root_distance_ms"`
	KissOfDeth string  `json:"kod,omitempty"`
}

func (s *Server) sample(rtt time.Duration, alpha float64) {
	m := float64(rtt.Microseconds()) / 1000
	s.last = m
	if s.ewma == 0 {
		s.ewma = m
	} else {
		s.ewma = alpha*m + (1-alpha)*s.ewma
	}
}

// Pool probes the configured servers and forwards traffic to the fastest eligible server first.
type Pool struct {
	cfg        NTPConfig
	servers    []*Server
	onFallback atomic.Bool              // the pool is answering from its fallback servers (for logging the change)
	rr         atomic.Uint64            // turn counter for spreading requests over the servers in the latency band
	rank       atomic.Pointer[rankSnap] // the ranking the forwarding path uses (see rankedSnap)
	rankGen    atomic.Uint64            // bumped when a server goes up or down: the snapshot is then out of date

	cancel context.CancelFunc
	wg     sync.WaitGroup

	Queries  atomic.Uint64 // client requests handled
	Answered atomic.Uint64 // answered from an upstream
	Failed   atomic.Uint64 // answered with nothing: no upstream replied
	Denied   atomic.Uint64 // refused: the client is not in allowed_clients
	Limited  atomic.Uint64 // turned away: the client was over client_rate

	lim *clientLimits // nil when no client list or rate is configured (clientlimit.go)
}

func NewPool(cfg NTPConfig) *Pool {
	p := &Pool{cfg: cfg, lim: newClientLimits(cfg)}
	for _, a := range cfg.Servers {
		p.servers = append(p.servers, &Server{Addr: a, hist: srvhist.series(a)})
	}
	for _, a := range cfg.FallbackServers {
		// a fallback is a last resort: it is never probed and always counts as up
		p.servers = append(p.servers, &Server{Addr: a, Fallback: true, healthy: true, hist: srvhist.series(a)})
	}
	return p
}

// Start launches the background probe loop (first round runs immediately).
func (p *Pool) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.ProbeNow(ctx)
		t := time.NewTicker(p.cfg.probeInterval())
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.ProbeNow(ctx)
			}
		}
	}()
}

func (p *Pool) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

// ProbeNow runs one probe round against every server concurrently, then judges the servers together: a server
// whose time disagrees with the others' is a falseticker and is taken out like one that does not answer.
func (p *Pool) ProbeNow(ctx context.Context) {
	var wg sync.WaitGroup
	var probed []*Server
	for _, s := range p.servers {
		if s.Fallback {
			continue
		}
		probed = append(probed, s)
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.probeServer(ctx, s)
		}()
	}
	wg.Wait()
	if ctx.Err() == nil {
		p.judgeRound(probed)
	}
	p.noteFallback()
}

// noteFallback logs the moment the pool starts or stops answering from its fallback servers.
func (p *Pool) noteFallback() {
	use := p.UsingFallback()
	if p.onFallback.Swap(use) == use {
		return
	}
	switch {
	case use:
		warnf("ntp: every configured server is down — answering from the fallback servers")
	case p.normalUp():
		infof("ntp: a configured server answers again — the fallback servers are no longer used")
	default:
		warnf("ntp: the fallback servers are down too — nothing can answer")
	}
}

// UsingFallback reports whether the pool is answering from its fallback servers: no normal server is
// healthy and at least one fallback is.
func (p *Pool) UsingFallback() bool {
	var fb, normal bool
	for _, s := range p.servers {
		s.mu.Lock()
		h := s.healthy
		s.mu.Unlock()
		if !h {
			continue
		}
		if s.Fallback {
			fb = true
		} else {
			normal = true
		}
	}
	return fb && !normal
}

// normalUp reports whether any normal (non-fallback) server is healthy.
func (p *Pool) normalUp() bool {
	for _, s := range p.servers {
		s.mu.Lock()
		h := s.healthy
		s.mu.Unlock()
		if h && !s.Fallback {
			return true
		}
	}
	return false
}

// nNormal is how many servers the pool has apart from the fallback ones.
func (p *Pool) nNormal() int {
	n := 0
	for _, s := range p.servers {
		if !s.Fallback {
			n++
		}
	}
	return n
}

// probeServer asks s for the time once and keeps the answer for judgeRound.
func (p *Pool) probeServer(ctx context.Context, s *Server) {
	smp, err := ntpProbe(ctx, s.Addr, p.cfg.probeTimeout())
	if ctx.Err() != nil {
		return
	}
	if err == nil {
		err = p.cfg.judge(smp)
	}
	s.mu.Lock()
	s.round = &probeRound{s: smp, err: err}
	if smp.RTT > 0 || smp.Stratum > 0 || smp.KoD != "" {
		s.info, s.haveInfo = smp, true
	}
	s.mu.Unlock()
}

// judgeRound turns the round's results into health.  A server passes when its probe did and, if at least three
// servers passed, its offset is within max_offset_ms (plus half its own delay, which bounds how well the offset can
// be known) of the median: with two servers there is nobody to say which of them is wrong.
func (p *Pool) judgeRound(probed []*Server) {
	type res struct {
		s   *Server
		r   *probeRound
		off float64
	}
	var all, good []res
	for _, s := range probed {
		s.mu.Lock()
		r := s.round
		s.round = nil
		s.mu.Unlock()
		if r == nil {
			continue
		}
		x := res{s: s, r: r, off: ms(r.s.Offset)}
		all = append(all, x)
		if r.err == nil {
			good = append(good, x)
		}
	}
	fals := map[*Server]error{}
	if p.cfg.MaxOffsetMS > 0 && len(good) >= 3 {
		offs := make([]float64, len(good))
		for i, g := range good {
			offs[i] = g.off
		}
		sort.Float64s(offs)
		med := offs[len(offs)/2]
		if len(offs)%2 == 0 {
			med = (offs[len(offs)/2-1] + offs[len(offs)/2]) / 2
		}
		for _, g := range good {
			tol := float64(p.cfg.MaxOffsetMS) + ms(g.r.s.Delay)/2
			if d := g.off - med; d > tol || -d > tol {
				fals[g.s] = fmt.Errorf("falseticker: its time is %.1f ms from the median of the servers (max_offset_ms is %d)", d, p.cfg.MaxOffsetMS)
			}
		}
	}
	for _, x := range all {
		err := x.r.err
		ft := false
		if fe, bad := fals[x.s]; bad {
			err, ft = fe, true
		}
		p.apply(x.s, x.r.s, err, ft)
	}
}

// apply folds one round's verdict on s into its health.
func (p *Pool) apply(s *Server, smp ntpSample, err error, falseticker bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastProbe = time.Now()
	if s.falseticker != falseticker {
		if falseticker {
			warnf("ntp: server %s is a falseticker: %v", s.Addr, err)
		} else {
			infof("ntp: server %s agrees with the other servers again", s.Addr)
		}
	}
	s.falseticker = falseticker
	if err == nil {
		s.sample(smp.RTT, p.cfg.LatencyAlpha)
		s.okCount++
		s.fails = 0
		s.failing.Store(false)
		s.lastErr = ""
		s.hist.addPrOK()
		s.hist.lat(smp.RTT)
		if !s.healthy {
			s.healthy = true
			p.rankGen.Add(1)
			infof("ntp: server %s is UP (%.1fms, stratum %d, offset %+.1fms)", s.Addr, s.ewma, smp.Stratum, ms(smp.Offset))
		}
		return
	}
	s.failCount++
	s.fails++
	s.failing.Store(true)
	s.lastErr = err.Error()
	s.hist.addPrFail()
	debugf("ntp: probe of %s failed (%d consecutive): %v", s.Addr, s.fails, err)
	if s.healthy && s.fails >= p.cfg.FailThreshold {
		s.healthy = false
		p.rankGen.Add(1)
		warnf("ntp: server %s is DOWN: %v", s.Addr, err)
	}
}

// Ranked returns the eligible servers, fastest first.
func (p *Pool) Ranked() []*Server {
	type ent struct {
		s *Server
		l float64
	}
	var es, fbs []ent
	for _, s := range p.servers {
		s.mu.Lock()
		if s.healthy {
			if s.Fallback {
				fbs = append(fbs, ent{s, s.ewma})
			} else {
				es = append(es, ent{s, s.ewma})
			}
		}
		s.mu.Unlock()
	}
	if len(es) == 0 { // every normal server is down: the fallback servers take over until one is back
		es = fbs
	}
	sort.Slice(es, func(i, j int) bool {
		if es[i].l != es[j].l {
			return es[i].l < es[j].l
		}
		return es[i].s.Addr < es[j].s.Addr
	})
	out := make([]*Server, len(es))
	for i, e := range es {
		out[i] = e.s
	}
	return out
}

// rankSnap is the ranking of the eligible servers at one moment: the servers fastest first and their latencies.
type rankSnap struct {
	at   int64 // unix nanoseconds when it was made
	gen  uint64
	list []*Server
	lat  []float64
}

// rankMaxAge is how long the forwarding path reuses a ranking.  Working it out takes every server's lock, and
// doing that for every request made those locks the busiest in the process; latencies move slowly (they are
// averages), and a server going up or down bumps the generation and so is seen at once.
const rankMaxAge = 100 * time.Millisecond

func (p *Pool) rankedSnap() *rankSnap {
	now := time.Now().UnixNano()
	gen := p.rankGen.Load()
	if sn := p.rank.Load(); sn != nil && sn.gen == gen && now-sn.at < int64(rankMaxAge) {
		return sn
	}
	sn := &rankSnap{at: now, gen: gen, list: p.Ranked()}
	sn.lat = make([]float64, len(sn.list))
	for i, s := range sn.list {
		s.mu.Lock()
		sn.lat[i] = s.ewma
		s.mu.Unlock()
	}
	p.rank.Store(sn)
	return sn
}

// Candidates is the order a request tries the servers in.  By default that is Ranked (fastest first).
// With spread on, the servers whose latency is within spread_band percent of the fastest take turns
// at the front (round-robin) and the slower ones follow in rank order as fallbacks.
func (p *Pool) Candidates() []*Server {
	sn := p.rankedSnap()
	if !p.cfg.Spread || len(sn.list) < 2 {
		return sn.list
	}
	return spreadOrder(sn.list, sn.lat, p.cfg.SpreadBand, p.rr.Add(1)-1)
}

// bandCount is how many of the servers (latencies sorted fastest first) are within band percent of the first.
func bandCount(lat []float64, band int) int {
	limit := lat[0] * (1 + float64(band)/100)
	n := 1
	for n < len(lat) && lat[n] <= limit {
		n++
	}
	return n
}

// inBand is the set of servers that currently take their turn at the front: with spread on, those within the
// band of the fastest healthy one (a lone fastest server counts); empty with spread off.
func (p *Pool) inBand() map[*Server]bool {
	out := map[*Server]bool{}
	if !p.cfg.Spread {
		return out
	}
	ranked := p.Ranked()
	if len(ranked) == 0 {
		return out
	}
	lat := make([]float64, len(ranked))
	for i, s := range ranked {
		s.mu.Lock()
		lat[i] = s.ewma
		s.mu.Unlock()
	}
	for _, s := range ranked[:bandCount(lat, p.cfg.SpreadBand)] {
		out[s] = true
	}
	return out
}

// spreadOrder puts the servers of ranked (sorted fastest first, lat their latencies) that are within band
// percent of the first one at the front, the one picked longest ago first (so they take turns, and a server
// that was out of the band for a while does not get a burst when it comes back), and keeps the slower ones
// behind them in rank order.  turn is a counter that grows with every call.
func spreadOrder(ranked []*Server, lat []float64, band int, turn uint64) []*Server {
	n := bandCount(lat, band)
	if n < 2 {
		return ranked
	}
	in := append([]*Server(nil), ranked[:n]...)
	sort.Slice(in, func(i, j int) bool {
		pi, pj := in[i].picked.Load(), in[j].picked.Load()
		if pi != pj {
			return pi < pj
		}
		return in[i].Addr < in[j].Addr
	})
	in[0].picked.Store(turn + 1)
	return append(in, ranked[n:]...)
}

// noteForwardFailure records a request that a server did not answer. It never takes the server out of service: a
// lost packet says little about the server, so only the server's own probes (judgeRound) mark it DOWN. The failure
// shows in the server's counts, history and last error.
func (p *Pool) noteForwardFailure(s *Server, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failCount++
	s.failing.Store(true)
	s.hist.addFail()
	s.lastErr = "forward: " + err.Error()
}

// Forward relays a client request to the fastest eligible server, falling back to the next fastest on timeout or
// a kiss-o'-death, up to max_attempts servers.  It returns the server's answer, unchanged, and the address of the
// server that gave it.  Live round-trip times feed the same latency estimate as the probes.
func (p *Pool) Forward(ctx context.Context, req []byte, client netip.Addr) (resp []byte, via string, err error) {
	return p.forward(ctx, req, client, true)
}

// forward is Forward; count says whether the request is a client's and so goes into the pool's counters.
func (p *Pool) forward(ctx context.Context, req []byte, client netip.Addr, count bool) ([]byte, string, error) {
	if len(req) < ntpHeaderLen {
		return nil, "", errors.New("short request")
	}
	if count {
		p.Queries.Add(1)
	}
	ranked := dropAsker(p.Candidates(), client)
	if len(ranked) == 0 {
		return nil, "", errNoServers
	}
	if len(ranked) > p.cfg.MaxAttempts {
		ranked = ranked[:p.cfg.MaxAttempts]
	}
	var lastResp []byte
	var lastVia string
	var lastErr error
	for _, s := range ranked {
		resp, rtt, err := exchange(ctx, s.Addr, req, p.cfg.queryTimeout())
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				break
			}
			p.noteForwardFailure(s, err)
			if count {
				noteFwdFailure(req, client, s.Addr, err)
			}
			continue
		}
		// every live answer is counted, but the server's lock (shared by every request it serves) is taken only to
		// clear a failure streak or to fold in the latency of one answer in eight: at tens of thousands of answers
		// a second that sampling is as good as sampling all of them and the lock is no longer contended
		s.served.Add(1)
		if s.failing.Load() || s.tick.Add(1)&7 == 0 {
			s.mu.Lock()
			s.sample(rtt, p.cfg.LatencyAlpha)
			s.fails = 0
			s.failing.Store(false)
			s.mu.Unlock()
			s.hist.lat(rtt)
		}
		if kodCode(resp) != "" { // the server turned this request away: try the next one, but keep this as a last resort
			s.hist.addFail()
			lastResp, lastVia = resp, s.Addr
			continue
		}
		s.hist.addOK()
		if count {
			p.Answered.Add(1)
		}
		return resp, s.Addr, nil
	}
	if lastResp != nil {
		if count {
			p.Answered.Add(1)
		}
		return lastResp, lastVia, nil
	}
	if lastErr == nil {
		lastErr = errNoServers
	}
	return nil, "", lastErr
}

func (p *Pool) Snapshot() []ServerStat {
	rank := map[*Server]int{}
	for i, s := range p.Ranked() {
		rank[s] = i + 1
	}
	band := p.inBand()
	out := make([]ServerStat, 0, len(p.servers))
	for _, s := range p.servers {
		s.mu.Lock()
		st := ServerStat{
			Addr: s.Addr, Fallback: s.Fallback, Healthy: s.healthy, Rank: rank[s], InBand: band[s],
			EWMAMS: round2(s.ewma), LastMS: round2(s.last),
			ConsecFails: s.fails, Successes: s.okCount, Failures: s.failCount,
			Served: s.served.Load(), LastError: s.lastErr, Falseticker: s.falseticker,
		}
		if s.haveInfo {
			i := s.info
			st.Time = &TimeStat{
				Stratum: int(i.Stratum), Leap: int(i.Leap), Version: int(i.Version), RefID: i.RefID,
				OffsetMS: round2(ms(i.Offset)), DelayMS: round2(ms(i.Delay)),
				RootDelay: round2(ms(i.RootDelay)), RootDisp: round2(ms(i.RootDisp)), RootDist: round2(ms(i.RootDist)),
				KissOfDeth: i.KoD,
			}
		}
		if !s.lastProbe.IsZero() {
			st.LastProbeAgo = round2(time.Since(s.lastProbe).Seconds())
		}
		s.mu.Unlock()
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Rank == 0) != (b.Rank == 0) {
			return a.Rank != 0
		}
		if a.Rank != b.Rank {
			return a.Rank < b.Rank
		}
		return a.Addr < b.Addr
	})
	return out
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
