package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	ipFreebind   = 15   // IP_FREEBIND
	ipv6Freebind = 78   // IPV6_FREEBIND
	maxInflight  = 8192 // requests being worked on at once; a slow upstream must not make the proxy drop at 1024
	udpReaders   = 4    // goroutines reading the UDP socket
	udpWorkers   = 64   // goroutines that stay alive to answer requests (see readUDP)
)

// NTPFrontend serves NTP on one VIP (UDP) and relays every client request through the pool's fastest healthy
// upstream.
type NTPFrontend struct {
	addr netip.Addr
	port int
	pool func() *Pool
	gw   *srvSeries // the gateway's history (srvhist.go): client latency, errors; nil when not set

	mu     sync.Mutex
	udp    net.PacketConn
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	sem    chan struct{}
}

func NewNTPFrontend(addr netip.Addr, port int, pool func() *Pool) *NTPFrontend {
	return &NTPFrontend{addr: addr, port: port, pool: pool}
}

func (f *NTPFrontend) listenAddr() string {
	return net.JoinHostPort(f.addr.String(), strconv.Itoa(f.port))
}

// freebind lets us bind the VIP before the kernel has it configured
// (e.g. an AFN whose address is added a moment later).
func freebind(network string, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		if network == "udp6" || network == "tcp6" {
			serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, ipv6Freebind, 1)
		} else {
			serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, ipFreebind, 1)
		}
	})
	if err != nil {
		return err
	}
	return serr
}

func (f *NTPFrontend) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.udp != nil {
		return nil
	}
	lc := net.ListenConfig{Control: freebind}
	netw := "4"
	if f.addr.Is6() {
		netw = "6"
	}
	udp, err := lc.ListenPacket(context.Background(), "udp"+netw, f.listenAddr())
	if err != nil {
		return err
	}
	f.udp = udp
	f.ctx, f.cancel = context.WithCancel(context.Background())
	f.sem = make(chan struct{}, maxInflight)
	f.wg.Add(1)
	go f.serveUDP(udp)
	infof("ntp: proxy listening on %s (udp)", f.listenAddr())
	return nil
}

func (f *NTPFrontend) Stop() {
	f.mu.Lock()
	if f.udp == nil {
		f.mu.Unlock()
		return
	}
	f.cancel()
	f.udp.Close()
	f.udp = nil
	f.mu.Unlock()
	// The socket is closed and the context cancelled, so in-flight requests end on their own; but one stuck in an
	// upstream call must not hold up whoever stopped us (a gateway restart holds the engine lock around this).  After
	// ntpStopWait the stragglers are left to finish by themselves: they only touch their own, closed, sockets.
	done := make(chan struct{})
	go func() { f.wg.Wait(); close(done) }()
	select {
	case <-done:
		infof("ntp: proxy on %s stopped", f.listenAddr())
	case <-time.After(ntpStopWait):
		warnf("ntp: proxy on %s stopped; some requests were still running after %v and are left to finish on their own", f.listenAddr(), ntpStopWait)
	}
}

// ntpStopWait is how long Stop waits for requests that are still being answered (a variable for the test).
var ntpStopWait = 3 * time.Second

func (f *NTPFrontend) Listening() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.udp != nil
}

// resolve answers one client request: the upstream's answer, or what to say instead.  nil means say nothing.
func (f *NTPFrontend) resolve(req []byte, client netip.Addr) []byte {
	if !ntpRelayable(req) {
		return nil // not a client request this proxy passes on (ntpq control and private modes, a server's answer, rubbish)
	}
	p := f.pool()
	if p == nil {
		return unsyncResponse(req)
	}
	// who may ask, and how often, comes first
	if v := p.admit(client); v != admitOK {
		resp := p.turnedAway(req, v)
		if f.ctx.Err() == nil {
			qstats.Record(client, "", ntpVersion(req), outRefused)
		}
		return resp
	}
	var t0 time.Time
	timed := f.gw.sampled()
	if timed {
		t0 = time.Now()
	}
	resp, via, err := p.Forward(f.ctx, req, client)
	if timed && f.ctx.Err() == nil {
		f.gw.lat(time.Since(t0))
	}
	out := outAnswered
	switch {
	case err != nil:
		out, resp = outFailed, unsyncResponse(req)
		if f.ctx.Err() == nil {
			p.Failed.Add(1)
			debugf("ntp: %v — answering as an unsynchronised server", err)
		}
	case kodCode(resp) != "":
		out = outKoD
	}
	if f.ctx.Err() == nil {
		qstats.Record(client, via, ntpVersion(req), out)
		if out == outAnswered {
			f.gw.addOK()
		} else {
			f.gw.addFail()
		}
	}
	return resp
}

// bigRcvBuf asks for a large receive buffer on the listening socket: a burst of requests that arrives while every
// reader is busy waits there instead of being dropped by the kernel (the client sees that as a timeout).  The
// FORCE variant ignores net.core.rmem_max and needs privilege, so the plain one is the fallback.
func bigRcvBuf(pc net.PacketConn) {
	uc, ok := pc.(*net.UDPConn)
	if !ok {
		return
	}
	const want = 8 << 20
	if rc, err := uc.SyscallConn(); err == nil {
		forced := false
		rc.Control(func(fd uintptr) {
			forced = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUFFORCE, want) == nil
		})
		if forced {
			return
		}
	}
	uc.SetReadBuffer(want)
}

func (f *NTPFrontend) serveUDP(pc net.PacketConn) {
	defer f.wg.Done()
	bigRcvBuf(pc)
	rep := newUDPReplier(pc)
	defer rep.close() // after the readers are gone; the Stop that closed pc also waits for the workers
	var rw sync.WaitGroup
	// Each reader has its own small set of workers and its own hand-off channel.  One channel shared by all 64
	// workers (each also waiting on the shutdown channel) was the biggest lock wait left at 98k requests a second:
	// every hand-off and every wake-up took the same two channel locks.
	for i := 0; i < udpReaders; i++ {
		jobs := make(chan udpJob) // unbuffered: a request goes only to a worker that is idle right now
		for w := 0; w < udpWorkers/udpReaders; w++ {
			f.wg.Add(1)
			go f.udpWorker(jobs)
		}
		rw.Add(1)
		go func() {
			defer rw.Done()
			defer close(jobs) // the workers end with their reader
			f.readUDP(pc, rep, jobs)
		}()
	}
	rw.Wait()
}

type udpJob struct {
	rep    *udpReplier
	q      []byte
	bp     *[]byte // the pooled buffer q lives in, to be given back when the request is answered; nil if q is not pooled
	client net.Addr
}

// queryBufSize is the largest request kept in a pooled buffer (a plain request is 48 bytes; one with NTS
// extension fields is a few hundred); a larger one gets a buffer of its own.
const queryBufSize = 2048

// queryBufs holds the buffers received requests are copied into: a request used to cost an allocation of its own
// for every packet.  Nothing keeps a reference to the request once resolve has returned.
var queryBufs = sync.Pool{New: func() any { b := make([]byte, 0, queryBufSize); return &b }}

func (f *NTPFrontend) answerUDP(j udpJob) {
	if resp := f.resolve(j.q, addrOf(j.client)); resp != nil {
		j.rep.WriteTo(resp, j.client)
	}
	if j.bp != nil {
		queryBufs.Put(j.bp)
	}
}

// udpWorker answers requests for as long as the frontend runs.  A goroutine per request costs a new stack that has
// to grow to the depth of the resolver for every one of them (a tenth of the CPU at 90k requests a second), so a
// set of workers stays alive and keeps its stack; when they are all busy (an upstream is slow) the reader falls
// back to a goroutine per request, up to maxInflight of them.
func (f *NTPFrontend) udpWorker(jobs <-chan udpJob) {
	defer f.wg.Done()
	for j := range jobs {
		f.answerUDP(j)
	}
}

func (f *NTPFrontend) readUDP(pc net.PacketConn, rep *udpReplier, jobs chan<- udpJob) {
	buf := make([]byte, 65535)
	for {
		n, client, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if n < ntpHeaderLen || n > ntpMaxPacket {
			continue
		}
		j := udpJob{rep: rep, client: client}
		if n <= queryBufSize {
			j.bp = queryBufs.Get().(*[]byte)
			*j.bp = append((*j.bp)[:0], buf[:n]...)
			j.q = *j.bp
		} else {
			j.q = append([]byte(nil), buf[:n]...)
		}
		select {
		case jobs <- j:
			continue
		default:
		}
		select {
		case f.sem <- struct{}{}:
		default:
			if j.bp != nil {
				queryBufs.Put(j.bp)
			}
			continue // overloaded: drop, the client will ask again
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer func() { <-f.sem }()
			f.answerUDP(j)
		}()
	}
}

// addrOf extracts the IP address from a net.Addr (UDP or TCP).
func addrOf(a net.Addr) netip.Addr {
	switch v := a.(type) {
	case *net.UDPAddr:
		return v.AddrPort().Addr()
	case *net.TCPAddr:
		return v.AddrPort().Addr()
	}
	if a == nil {
		return netip.Addr{}
	}
	ap, err := netip.ParseAddrPort(a.String())
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr()
}
