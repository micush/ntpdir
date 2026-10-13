package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Request statistics for the Monitor → Statistics page.  Everything is kept in memory for
// the last qsRetain (30 days) and saved to disk by persist.go, so a restart does not lose it.
//
//   - Totals by outcome, one slot per minute (the line chart and the tiles).
//   - NTP versions, address families, clients and upstream servers (the donuts and the top lists) in two
//     tiers: 10-minute slots for the last day, so a short range is exact, and hour slots for
//     the whole 30 days.  A range inside the last day reads the fine tier, a longer one the
//     coarse tier.  The distinct clients and servers per slot are capped, so a flood of
//     requests from forged addresses cannot grow memory without bound; the rest count as "(others)".

const (
	qsRetain      = 30 * 24 * time.Hour
	qsMinSlots    = 30 * 24 * 60
	qsFineSpan    = 10  // minutes per fine top-list slot (used for ranges within the last day)
	qsFineSlots   = 150 // 25 hours of fine slots
	qsFineWithin  = 24 * time.Hour
	qsCoarseSpan  = 60 // minutes per coarse top-list slot (older data, and longer ranges)
	qsCoarseSlots = 30 * 24
	qsCapClients  = 300
	qsCapServers  = 100
	qsKeepResult  = 100 // entries per top list in a reply
	qsMaxPoints   = 800
	qsOthers      = "(others)"
	qsRDNSTTL     = 10 * time.Minute
	qsRDNSNegTTL  = 2 * time.Minute // a client with no name found is asked again after this
	qsRDNSTimeout = 2 * time.Second
)

type qsMin struct {
	stamp                              int64 // minutes since the epoch; a slot with another stamp is stale
	total, answered, failed, kod, refd uint32
}

// The outcomes the top lists are kept apart by, so a tile can show who asked and was, say, turned away.  The
// order is the index into qsTop.c.
const (
	outAnswered = iota // chrony answered
	outFailed          // (not used with chrony)
	outKoD             // (not used with chrony)
	outRefused         // chrony dropped the request: not allowed, or over the rate limit
	qcCount
)

var qcNames = [qcCount]string{"answered", "failed", "kod", "refused"}

// qsCaps are the most distinct clients and servers one slot keeps per outcome; anything beyond counts as
// "(others)".  Failures are the rare kind, so they get less room.
var qsCaps = [qcCount][2]int{
	outAnswered: {qsCapClients, qsCapServers},
	outFailed:   {100, 50},
	outKoD:      {100, 50},
	outRefused:  {200, 10},
}

// qsPairCaps are the most distinct (client, server) pairs one slot keeps per outcome.
// They answer "which servers answered this client" and "whom did this server answer".  A pair beyond the cap
// is not kept; the reply makes up the difference as "(others)" from the client and server totals.
var qsPairCaps = [qcCount]int{
	outAnswered: 500,
	outFailed:   100,
	outKoD:      100,
	outRefused:  50,
}

// qsKind is the top lists of one outcome in one slot.
type qsKind struct {
	clients  map[string]uint32
	servers  map[string]uint32
	versions map[string]uint32
	pairs    map[string]map[string]uint32 // client → server → count
	npairs   int
	v4, v6   uint32
}

type qsTop struct {
	stamp int64 // slot periods since the epoch
	c     [qcCount]qsKind
}

// QStats is the collector.  The zero value is not usable: call NewQStats.
type QStats struct {
	mu         sync.Mutex
	who        map[netip.Addr]string // client address strings in use (whoString)
	start      time.Time
	now        func() time.Time
	pend       [qsShards]qsPending // events not yet counted (see Record)
	pendN      atomic.Uint32       // picks the shard
	flushArmed atomic.Bool
	mins       [qsMinSlots]qsMin
	fine       [qsFineSlots]qsTop
	coarse     [qsCoarseSlots]qsTop

	rmu   sync.Mutex
	names map[string]string // names given by hand (Settings), under rmu
	rdns  map[string]rdnsEntry
	rwork map[string]bool
}

type rdnsEntry struct {
	names []string
	at    time.Time
}

func NewQStats() *QStats {
	return &QStats{start: time.Now(), now: time.Now, rdns: map[string]rdnsEntry{}, rwork: map[string]bool{}}
}

// qstats is the daemon-wide collector, fed by every NTP frontend.
var qstats = NewQStats()

// Record counts one client request: who asked, which upstream server answered it (empty when none did), the NTP
// version of the request and how it ended (an out* constant).
func (s *QStats) Record(client netip.Addr, server string, version int, outcome int) {
	s.enqueue(qEvent{t: s.now().Unix(), client: client, server: server, version: uint8(version), outcome: uint8(outcome)})
}

// RecordN counts n requests of one client that ended the same way (chrony reports totals, not single requests).
func (s *QStats) RecordN(client netip.Addr, server string, version int, outcome int, n uint32) {
	if n == 0 {
		return
	}
	s.enqueue(qEvent{t: s.now().Unix(), client: client, server: server, version: uint8(version), outcome: uint8(outcome), n: n})
}

// qsWhoMax is how many client address strings are kept for reuse (see whoString).
const qsWhoMax = 4096

// whoString is the text of a client address: made once and reused, since String() allocates every time and the
// same few clients account for most requests.  The table is emptied when it is full, so a flood of distinct
// addresses cannot grow it past qsWhoMax.  s.mu is held.
func (s *QStats) whoString(client netip.Addr) string {
	if !client.IsValid() {
		return "unknown"
	}
	client = client.Unmap()
	if w, ok := s.who[client]; ok {
		return w
	}
	if s.who == nil || len(s.who) >= qsWhoMax {
		s.who = make(map[netip.Addr]string, 256)
	}
	w := client.String()
	s.who[client] = w
	return w
}

// versionName is how an NTP version shows in the statistics.
func versionName(v uint8) string {
	if v >= 1 && v <= 4 {
		return "NTPv" + strconv.Itoa(int(v))
	}
	return "other"
}

// apply counts one request; s.mu is held.
func (s *QStats) apply(e qEvent) {
	client, server, kind := e.client, e.server, int(e.outcome)
	if kind >= qcCount {
		kind = outFailed
	}
	if server == "" {
		server = "(none)"
	}
	n := e.n
	if n == 0 {
		n = 1
	}
	m := e.t / 60
	who := s.whoString(client)
	ms := &s.mins[m%qsMinSlots]
	if ms.stamp != m {
		*ms = qsMin{stamp: m}
	}
	ms.total += n
	switch kind {
	case outAnswered:
		ms.answered += n
	case outFailed:
		ms.failed += n
	case outKoD:
		ms.kod += n
	case outRefused:
		ms.refd += n
	}
	vn := versionName(e.version)
	v6 := client.IsValid() && !client.Unmap().Is4()
	for _, tier := range []struct {
		slots []qsTop
		span  int64
	}{{s.fine[:], qsFineSpan}, {s.coarse[:], qsCoarseSpan}} {
		p := m / tier.span
		tp := &tier.slots[p%int64(len(tier.slots))]
		if tp.stamp != p {
			*tp = qsTop{stamp: p}
		}
		k := &tp.c[kind]
		if k.clients == nil {
			k.clients, k.servers, k.versions = map[string]uint32{}, map[string]uint32{}, map[string]uint32{}
			k.pairs = map[string]map[string]uint32{}
		}
		bump(k.clients, who, qsCaps[kind][0], n)
		bump(k.servers, server, qsCaps[kind][1], n)
		k.versions[vn] += n
		if sm := k.pairs[who]; sm != nil && sm[server] > 0 {
			sm[server] += n
		} else if k.npairs < qsPairCaps[kind] {
			if sm == nil {
				sm = map[string]uint32{}
				k.pairs[who] = sm
			}
			sm[server] += n
			k.npairs++
		}
		if v6 {
			k.v6 += n
		} else {
			k.v4 += n
		}
	}
}

// Counting a request takes the collector's one lock, and with every worker doing it for every request the lock
// itself was about 6% of the CPU on a busy node and made the workers wait on each other.  So the workers only
// append an event to one of a few small buffers, each behind its own lock, and the events are counted in one
// batch a moment later (or at once, when something reads the statistics).  The count is the same; it just
// reaches the tables up to qsFlushEvery late.
const (
	qsShards     = 16
	qsFlushEvery = 200 * time.Millisecond
	qsShardMax   = 1 << 15 // events held per shard; a full shard is counted by the caller instead
)

type qEvent struct {
	t       int64 // unix seconds when it happened
	client  netip.Addr
	server  string
	version uint8
	outcome uint8
	n       uint32 // how many requests this event stands for; 0 is one
}

type qsPending struct {
	mu sync.Mutex
	ev []qEvent
	_  [40]byte // keep neighbouring shards off one cache line
}

// enqueue files an event; the first one after a flush also arms the timer that counts the batch.
func (s *QStats) enqueue(e qEvent) {
	p := &s.pend[s.pendN.Add(1)%qsShards]
	p.mu.Lock()
	if len(p.ev) >= qsShardMax {
		p.mu.Unlock()
		s.flush() // way behind: count what is waiting now rather than grow without bound
		p.mu.Lock()
	}
	p.ev = append(p.ev, e)
	p.mu.Unlock()
	if s.flushArmed.CompareAndSwap(false, true) {
		time.AfterFunc(qsFlushEvery, func() { s.flushArmed.Store(false); s.flush() })
	}
}

// flush counts every waiting event.  Everything that reads or saves the tables calls it first.
func (s *QStats) flush() {
	var batch [][]qEvent
	for i := range s.pend {
		p := &s.pend[i]
		p.mu.Lock()
		if len(p.ev) > 0 {
			batch = append(batch, p.ev)
			p.ev = nil
		}
		p.mu.Unlock()
	}
	if len(batch) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, evs := range batch {
		for _, e := range evs {
			s.apply(e)
		}
	}
}

func bump(m map[string]uint32, k string, limit int, n uint32) {
	if _, ok := m[k]; !ok && len(m) >= limit {
		k = qsOthers
	}
	m[k] += n
}

// NameCount is one row of a top list.
type NameCount struct {
	Name   string   `json:"name"`
	Count  uint64   `json:"count"`
	Host   string   `json:"host,omitempty"`   // reverse-DNS name of a client, when known
	Hosts  []string `json:"hosts,omitempty"`  // every reverse-DNS name it has, for the tooltip
	Manual bool     `json:"manual,omitempty"` // Host is a name given by hand (Name…), not a reverse-DNS one
}

// QStatsResult is the answer to a query: parallel series, one point per Step seconds
// from Start, plus the totals and top lists over the whole range.
type QStatsResult struct {
	Client   string       `json:"client,omitempty"` // when set, Servers lists the servers that answered this client
	Server   string       `json:"server,omitempty"` // when set, Clients lists whom this server answered
	Outcome  string       `json:"outcome"`          // the outcome the versions, families and top lists are limited to; empty is all
	Start    int64        `json:"start"`
	Step     int          `json:"step"`
	From     int64        `json:"from"`
	To       int64        `json:"to"`
	Since    int64        `json:"since"` // when the daemon started counting
	Total    []uint64     `json:"total"`
	Answered []uint64     `json:"answered"`
	Failed   []uint64     `json:"failed"`
	KoD      []uint64     `json:"kod"`
	Refused  []uint64     `json:"refused"`
	Sums     QStatsSums   `json:"sums"`
	Guard    MemGuardInfo `json:"guard"` // what the memory guard has dropped
	Versions []NameCount  `json:"versions"`
	Families []NameCount  `json:"families"`
	Clients  []NameCount  `json:"clients"`
	Servers  []NameCount  `json:"servers"`
	Cluster  *ClusterInfo `json:"cluster,omitempty"` // set when the numbers are those of several nodes added together
}

type QStatsSums struct {
	Total    uint64 `json:"total"`
	Answered uint64 `json:"answered"`
	Failed   uint64 `json:"failed"`
	KoD      uint64 `json:"kod"`
	Refused  uint64 `json:"refused"`
	Clients  int    `json:"clients"` // distinct clients (a lower bound once a slot hit its cap)
}

// stepFor picks the point spacing for a span: a minute up to 2 hours, ten minutes up to
// a day and a half, an hour up to a week, three hours beyond.
func stepFor(span time.Duration) int64 {
	switch {
	case span <= 2*time.Hour:
		return 60
	case span <= 36*time.Hour:
		return 600
	case span <= 7*24*time.Hour:
		return 3600
	}
	return 10800
}

// Query returns the statistics for [from, to).  The range is cut to what is retained,
// and aligned to the step so every node answers with the same points.
//
// f.Outcome limits the versions, families, clients and servers to one outcome ("answered", "failed", "kod" or
// "refused"); empty means every request.  f.Client replaces the server list by the servers that answered that
// client, f.Server the client list by whom that server answered (both within the outcome).  The time series and
// the sums always cover everything.
func (s *QStats) Query(from, to time.Time, f QFilter) *QStatsResult {
	outcome := f.Outcome
	now := s.now()
	if to.IsZero() || to.After(now) {
		to = now
	}
	if oldest := now.Add(-qsRetain); from.Before(oldest) {
		from = oldest
	}
	if !from.Before(to) {
		from = to.Add(-time.Hour)
	}
	step := stepFor(to.Sub(from))
	start := from.Unix() / step * step
	end := (to.Unix() + step) / step * step // the step containing `to` is included
	n := int((end - start) / step)
	if n > qsMaxPoints {
		n = qsMaxPoints
	}
	r := &QStatsResult{Outcome: outcome, Client: f.Client, Server: f.Server, Start: start, Step: int(step), From: from.Unix(), To: to.Unix(), Since: s.start.Unix(),
		Total: make([]uint64, n), Answered: make([]uint64, n), Failed: make([]uint64, n), KoD: make([]uint64, n), Refused: make([]uint64, n),
		Versions: []NameCount{}, Families: []NameCount{}, Clients: []NameCount{}, Servers: []NameCount{}}
	versions, clients, servers := map[string]uint64{}, map[string]uint64{}, map[string]uint64{}
	var v4, v6 uint64
	pairServers, pairClients := map[string]uint64{}, map[string]uint64{}

	s.flush()
	s.mu.Lock()
	for i := 0; i < n; i++ {
		for m := (start + int64(i)*step) / 60; m < (start+int64(i+1)*step)/60; m++ {
			ms := &s.mins[m%qsMinSlots]
			if ms.stamp != m {
				continue
			}
			r.Total[i] += uint64(ms.total)
			r.Answered[i] += uint64(ms.answered)
			r.Failed[i] += uint64(ms.failed)
			r.KoD[i] += uint64(ms.kod)
			r.Refused[i] += uint64(ms.refd)
		}
	}
	slots, span := s.coarse[:], int64(qsCoarseSpan)
	if now.Sub(from) <= qsFineWithin {
		slots, span = s.fine[:], qsFineSpan
	}
	for p := from.Unix() / 60 / span; p <= to.Unix()/60/span; p++ {
		tp := &slots[p%int64(len(slots))]
		if tp.stamp != p {
			continue
		}
		for ci := range tp.c {
			if outcome != "" && qcNames[ci] != outcome {
				continue
			}
			k := &tp.c[ci]
			for n, v := range k.clients {
				clients[n] += uint64(v)
			}
			for n, v := range k.servers {
				servers[n] += uint64(v)
			}
			for n, v := range k.versions {
				versions[n] += uint64(v)
			}
			v4 += uint64(k.v4)
			v6 += uint64(k.v6)
			if f.Client != "" {
				for n, v := range k.pairs[f.Client] {
					pairServers[n] += uint64(v)
				}
			}
			if f.Server != "" {
				for c, m := range k.pairs {
					if v := m[f.Server]; v > 0 {
						pairClients[c] += uint64(v)
					}
				}
			}
		}
	}
	s.mu.Unlock()

	for i := 0; i < n; i++ {
		r.Sums.Total += r.Total[i]
		r.Sums.Answered += r.Answered[i]
		r.Sums.Failed += r.Failed[i]
		r.Sums.KoD += r.KoD[i]
		r.Sums.Refused += r.Refused[i]
	}
	r.Guard = memguard.Info()
	r.Sums.Clients = len(clients)
	if _, ok := clients[qsOthers]; ok {
		r.Sums.Clients--
	}
	r.Versions = topList(versions, 0)
	if v4 > 0 {
		r.Families = append(r.Families, NameCount{Name: "IPv4", Count: v4})
	}
	if v6 > 0 {
		r.Families = append(r.Families, NameCount{Name: "IPv6", Count: v6})
	}
	if f.Client != "" {
		servers = withRest(pairServers, clients[f.Client])
	}
	if f.Server != "" {
		clients = withRest(pairClients, servers[f.Server])
	}
	r.Clients = topList(s.keepMatchingClients(clients, f.CMatch), qsKeepResult)
	r.Servers = topList(keepMatching(servers, f.SMatch), qsKeepResult)
	s.fillHosts(r.Clients)
	return r
}

// withRest adds an "(others)" entry for the part of total the pairs do not account for.
func withRest(m map[string]uint64, total uint64) map[string]uint64 {
	var sum uint64
	for _, v := range m {
		sum += v
	}
	if total > sum {
		m[qsOthers] = total - sum
	}
	return m
}

// topList sorts by count (name breaks ties); limit 0 keeps all.  "(others)" always sorts last.
func topList(m map[string]uint64, limit int) []NameCount {
	out := make([]NameCount, 0, len(m))
	for k, v := range m {
		out = append(out, NameCount{Name: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		oi, oj := out[i].Name == qsOthers, out[j].Name == qsOthers
		if oi != oj {
			return oj
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// fillHosts adds the reverse-DNS names to the listed clients, from a cache.  A miss starts a
// background lookup (a few at a time), so the page never waits for it: the names show up on a
// later refresh.
func (s *QStats) fillHosts(cl []NameCount) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	for i := range cl {
		ip := cl[i].Name
		if _, err := netip.ParseAddr(ip); err != nil {
			continue
		}
		e, ok := s.rdns[ip]
		if n := s.names[ip]; n != "" { // a name given by hand comes first, ahead of what reverse DNS says
			cl[i].Host, cl[i].Manual = n, true
			cl[i].Hosts = append([]string{n}, e.names...)
			if !ok && !s.rwork[ip] {
				s.rwork[ip] = true
				go s.lookup(ip) // the reverse names still go in the tooltip
			}
			continue
		}
		if ok && time.Since(e.at) < qsRDNSTTL && (len(e.names) > 0 || time.Since(e.at) < qsRDNSNegTTL) {
			setHosts(&cl[i], e.names)
			continue
		}
		if !s.rwork[ip] {
			s.rwork[ip] = true
			go s.lookup(ip)
		}
		if ok {
			setHosts(&cl[i], e.names) // stale beats nothing while the refresh runs
		}
	}
}

// SetNames sets the names given to clients by hand (address -> name); the daemon calls it with every configuration it loads.
func (s *QStats) SetNames(m map[string]string) {
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	s.rmu.Lock()
	s.names = c
	s.rmu.Unlock()
}

func setHosts(c *NameCount, names []string) {
	if len(names) > 0 {
		c.Host, c.Hosts = names[0], names
	}
}

// qsRDNSParallel bounds the reverse lookups running at once.
var qsRDNSSem = make(chan struct{}, 8)

func (s *QStats) lookup(ip string) {
	qsRDNSSem <- struct{}{}
	defer func() { <-qsRDNSSem }()
	var out []string
	add := func(names []string) {
		for _, n := range names {
			if n = strings.TrimSuffix(n, "."); n != "" && len(out) < 5 {
				out = append(out, n)
			}
		}
	}
	// whatever this machine's own resolver knows (hosts file, resolv.conf)
	ctx, cancel := context.WithTimeout(context.Background(), qsRDNSTimeout)
	if names, err := net.DefaultResolver.LookupAddr(ctx, ip); err == nil {
		add(names)
	}
	cancel()
	s.rmu.Lock()
	s.rdns[ip] = rdnsEntry{names: out, at: time.Now()}
	delete(s.rwork, ip)
	s.rmu.Unlock()
}

// qstatsRange reads the from/to arguments of the API and CLI: unix seconds, or a span
// such as "1h", "1d", "7d", "30d" meaning that long up to now.  Empty means the last hour.
func qstatsRange(from, to string) (time.Time, time.Time, error) {
	now := time.Now()
	end := now
	if to != "" {
		n, err := strconv.ParseInt(to, 10, 64)
		if err != nil || n <= 0 {
			return time.Time{}, time.Time{}, fmt.Errorf("%q is not a time (unix seconds)", to)
		}
		end = time.Unix(n, 0)
	}
	if from == "" {
		return end.Add(-time.Hour), end, nil
	}
	if n, err := strconv.ParseInt(from, 10, 64); err == nil && n > 1e9 {
		return time.Unix(n, 0), end, nil
	}
	d, err := parseSince(from)
	if err != nil || d == 0 {
		return time.Time{}, time.Time{}, fmt.Errorf("%q is not a range: use 1h, 1d, 7d, 30d or unix seconds", from)
	}
	return end.Add(-d), end, nil
}

// qstatsOutcome checks the outcome argument of the API and CLI.
func qstatsOutcome(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "all" {
		return "", nil
	}
	for _, n := range qcNames {
		if n == s {
			return s, nil
		}
	}
	return "", fmt.Errorf("%q is not an outcome: use %s", s, strings.Join(qcNames[:], ", "))
}

// QFilter narrows a Query; zero values mean "no filter".
type QFilter struct {
	Outcome string // an outcome, see qcNames
	Client  string // an address as listed in Top clients
	Server  string // an address as listed in Top servers
	// CMatch and SMatch keep only the clients / servers whose name contains the text (any case): the filter boxes of the
	// two top lists.  They look through everything the counters kept, not only the entries the reply lists.
	CMatch string
	SMatch string
}

// withMatch adds the two filter-box texts (trimmed, lower case, at most 100 characters) to a filter.
func (f QFilter) withMatch(client, server string) QFilter {
	clean := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		if len(s) > 100 {
			s = s[:100]
		}
		return s
	}
	f.CMatch, f.SMatch = clean(client), clean(server)
	return f
}

// qsHostLookups is how many clients without a host name yet are looked up in the background when a filter that looks like a
// host name is typed (their names are there on a later refresh).
const qsHostLookups = 200

// keepMatchingClients is keepMatching for the client list: a client is kept when its address or any of its reverse-DNS
// host names contains the text.  Only the names already looked up can match; a text that is not just digits, a-f, dots and
// colons (so it can be part of a host name) starts the lookups of the busiest clients that have none yet.
func (s *QStats) keepMatchingClients(m map[string]uint64, text string) map[string]uint64 {
	if text == "" {
		return m
	}
	hostLike := strings.Trim(text, "0123456789abcdef:.") != ""
	out := make(map[string]uint64, len(m))
	var todo []NameCount
	s.rmu.Lock()
	for k, v := range m {
		if k == qsOthers {
			continue
		}
		if strings.Contains(strings.ToLower(k), text) {
			out[k] = v
			continue
		}
		if strings.Contains(strings.ToLower(s.names[k]), text) {
			out[k] = v
			continue
		}
		e, seen := s.rdns[k]
		for _, n := range e.names {
			if strings.Contains(strings.ToLower(n), text) {
				out[k] = v
				break
			}
		}
		if hostLike && !seen && !s.rwork[k] {
			if _, err := netip.ParseAddr(k); err == nil {
				todo = append(todo, NameCount{Name: k, Count: v})
			}
		}
	}
	if len(todo) > 0 {
		sort.Slice(todo, func(i, j int) bool { return todo[i].Count > todo[j].Count })
		if len(todo) > qsHostLookups {
			todo = todo[:qsHostLookups]
		}
		for _, c := range todo {
			s.rwork[c.Name] = true
			go s.lookup(c.Name)
		}
	}
	s.rmu.Unlock()
	return out
}

// keepMatching drops the entries whose name does not contain text; "(others)" goes too, as it stands for names not kept.
func keepMatching(m map[string]uint64, text string) map[string]uint64 {
	if text == "" {
		return m
	}
	out := make(map[string]uint64, len(m))
	for k, v := range m {
		if k != qsOthers && strings.Contains(strings.ToLower(k), text) {
			out[k] = v
		}
	}
	return out
}

// qstatsFilter checks and normalises the outcome, client and server arguments of the API and CLI.
func qstatsFilter(outcome, client, server string) (QFilter, error) {
	var f QFilter
	var err error
	if f.Outcome, err = qstatsOutcome(outcome); err != nil {
		return f, err
	}
	client = strings.TrimSpace(client)
	server = strings.TrimSpace(server)
	if client != "" && server != "" {
		return f, fmt.Errorf("pick a client or a server, not both")
	}
	if client != "" {
		if a, err := netip.ParseAddr(client); err == nil {
			client = a.Unmap().String()
		} else if client != "unknown" {
			return f, fmt.Errorf("%q is not a client address", client)
		}
	}
	if len(server) > 255 || strings.ContainsAny(server, " \t\r\n") {
		return f, fmt.Errorf("%q is not a server address", server)
	}
	f.Client, f.Server = client, server
	return f, nil
}

// Clear forgets every count and top list (Statistics ▸ Clear).  The counting-since time starts again.
func (s *QStats) Clear() {
	s.flush()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mins = [qsMinSlots]qsMin{}
	s.fine = [qsFineSlots]qsTop{}
	s.coarse = [qsCoarseSlots]qsTop{}
	s.who = nil
	s.start = s.now()
}
