package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// Piggyback.  When the Anyname DNS Director (dnsdir) is installed on the same host, ntpdir has no gateways of its own:
// dnsdir owns the shared addresses and the anycast addresses and decides which node holds them.  ntpdir keeps one gateway,
// marked Piggyback, with one list of NTP servers, and answers NTP on every address of dnsdir's gateways (its shared
// addresses and its anycast addresses).  It only listens there: it never adds, removes or announces an address.  Gateways
// ntpdir had of its own stay in the configuration but do not run while dnsdir is installed.  Without dnsdir it is the other
// way round.

// inactiveReason says why a gateway does not run on this host because of dnsdir; "" when it does.
func inactiveReason(g GroupConfig) string {
	present := dnsdirPresent()
	switch {
	case present && !g.Piggyback:
		return "dnsdir is installed here, so NTP answers on dnsdir's gateway addresses and this gateway does not run"
	case !present && g.Piggyback:
		return "dnsdir is not installed here"
	}
	return ""
}

// piggybackGroup says whether the gateway with this group number is one that mirrors a dnsdir gateway.
func piggybackGroup(dc *DaemonConfig, id int) bool {
	for _, g := range dc.Groups {
		if g.GroupID == id {
			return g.Piggyback
		}
	}
	return false
}

// piggybackLabel is the name a mirrored gateway shows: dnsdir's name for it, else its address, else its number.
func piggybackLabel(gw dnsdirGateway) string {
	n := gw.Name
	if n == "" {
		n = gw.VIP4
	}
	if n == "" {
		n = gw.VIP6
	}
	if n == "" {
		n = "dnsdir gateway " + strconv.Itoa(gw.ID)
	}
	if r := []rune(n); len(r) > maxGatewayName {
		n = string(r[:maxGatewayName])
	}
	return n
}

// wantPiggyback returns dc changed so that, while dnsdir is installed, it has one gateway for each gateway of dnsdir (the same
// name, answering on that gateway's shared and anycast addresses as dnsdir has them now), and none for a gateway dnsdir no
// longer has.  They all use one list of NTP servers.  changed says whether anything was altered.  Without dnsdir, or when
// dnsdir's configuration shows no gateway, nothing is changed.
func wantPiggyback(dc *DaemonConfig) (changed bool) {
	gws := dnsdirGateways()
	if len(gws) == 0 {
		return false
	}
	want := map[int]bool{}
	for _, gw := range gws {
		want[gw.ID] = true
	}
	// drop the ones whose dnsdir gateway is gone; a gateway made by v167 (no number yet) is adopted below
	var legacy []int
	kept := dc.Groups[:0:0]
	for _, g := range dc.Groups {
		if g.Piggyback && g.PiggybackOf != 0 && !want[g.PiggybackOf] {
			changed = true
			continue
		}
		kept = append(kept, g)
	}
	dc.Groups = kept
	by := map[int]int{}
	for i, g := range dc.Groups {
		switch {
		case !g.Piggyback:
		case g.PiggybackOf == 0:
			legacy = append(legacy, i)
		default:
			by[g.PiggybackOf] = i
		}
	}
	var tmpl *NTPConfig // the servers every mirrored gateway uses: those of the first one that has its own list
	for _, g := range dc.Groups {
		if g.Piggyback && g.NTP != nil {
			tmpl = g.NTP
			break
		}
	}
	used := map[int]bool{}
	for _, g := range dc.Groups {
		used[g.GroupID] = true
	}
	for _, gw := range gws {
		addrs := append(append([]string{}, gw.Shared...), gw.Anycast...)
		sort.Strings(addrs)
		idx, ok := by[gw.ID]
		if !ok && len(legacy) > 0 {
			idx, ok, legacy = legacy[0], true, legacy[1:]
			dc.Groups[idx].PiggybackOf = gw.ID
			changed = true
		}
		if !ok {
			id := firstGroupID
			for used[id] {
				id++
			}
			used[id] = true
			g := defaultGroup()
			g.GroupID, g.Piggyback, g.PiggybackOf, g.VIP4, g.VIP6, g.Interface = id, true, gw.ID, "", "", ""
			g.Neighbors = nil
			g.NTP = cloneNTPPtr(tmpl)
			dc.Groups = append(dc.Groups, g)
			idx, changed = len(dc.Groups)-1, true
			if tmpl == nil && dc.Groups[idx].NTP != nil {
				tmpl = dc.Groups[idx].NTP
			}
		}
		g := &dc.Groups[idx]
		if label := piggybackLabel(gw); g.Name != label {
			g.Name, changed = label, true
		}
		if !sameList(g.DNSDirShared, gw.Shared) {
			g.DNSDirShared, changed = append([]string(nil), gw.Shared...), true
		}
		if !sameList(g.DNSDirVIPs, addrs) {
			g.DNSDirVIPs, changed = addrs, true
		}
	}
	// a v167 gateway nothing was left to adopt
	if len(legacy) > 0 {
		drop := map[int]bool{}
		for _, i := range legacy {
			drop[dc.Groups[i].GroupID] = true
		}
		kept := dc.Groups[:0:0]
		for _, g := range dc.Groups {
			if !drop[g.GroupID] {
				kept = append(kept, g)
			}
		}
		dc.Groups, changed = kept, true
	}
	sort.Slice(dc.Groups, func(i, j int) bool { return dc.Groups[i].GroupID < dc.Groups[j].GroupID })
	return changed
}

// syncPiggybackNTP makes every mirrored gateway use the NTP servers of the one with group number from: they share one list.
func syncPiggybackNTP(dc *DaemonConfig, from int) {
	src, err := findGroup(dc, from)
	if err != nil || !src.Piggyback {
		return
	}
	for i := range dc.Groups {
		if g := &dc.Groups[i]; g.Piggyback && g.GroupID != from {
			g.NTP = cloneNTPPtr(src.NTP)
		}
	}
}

// piggybackEvery is how often the primary compares its gateway with dnsdir's addresses.
var piggybackEvery = 10 * time.Second

// syncPiggyback keeps the piggyback gateway in step with dnsdir.  Only the primary of a cluster (or a node on its own) writes;
// the others get the change with the shared settings.
func (m *Mgmt) syncPiggyback() error {
	if !dnsdirPresent() {
		return nil
	}
	if m.cl.Enabled() && m.cl.Snapshot().Role == RoleReplica {
		return nil
	}
	m.cfgMu.Lock()
	dc, _, err := m.LiveConfig()
	m.cfgMu.Unlock()
	if err != nil {
		return err
	}
	if !wantPiggyback(dc) {
		return nil
	}
	if err := m.PutConfig(dc, "dnsdir", "dnsdir addresses"); err != nil {
		return fmt.Errorf("could not follow dnsdir's addresses: %w", err)
	}
	infof("piggyback: answering NTP on the addresses of dnsdir's %d gateway(s)", len(dnsdirGateways()))
	return nil
}

// RunPiggyback is the loop behind syncPiggyback.
func (m *Mgmt) RunPiggyback(ctx context.Context) {
	t := time.NewTicker(piggybackEvery)
	defer t.Stop()
	last := ""
	for {
		if err := m.syncPiggyback(); err != nil && err.Error() != last {
			last = err.Error()
			warnf("%v", err)
		} else if err == nil {
			last = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// reevaluate starts and stops gateways when dnsdir has been installed or removed since they were started (nothing is
// restarted when it has not).  The supervisor calls it every few seconds.
func (s *Supervisor) reevaluate() {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	present := dnsdirPresent()
	s.mu.Lock()
	if present == s.dnsdirSeen {
		s.mu.Unlock()
		return
	}
	s.dnsdirSeen = present
	var stopped []*Engine
	for _, gc := range s.dc.Groups {
		running := s.anycast[gc.GroupID] != nil || s.engines[engineKey{gc.GroupID, afIPv4}] != nil || s.engines[engineKey{gc.GroupID, afIPv6}] != nil ||
			s.warming[gc.GroupID] != nil || s.offnet[gc.GroupID] != nil
		switch why := inactiveReason(gc); {
		case why != "" && running:
			infof("Group %d: stopping — %s", gc.GroupID, why)
			stopped = append(stopped, s.stopGroupLocked(gc.GroupID)...)
			s.inactive[gc.GroupID] = why
		case why == "" && !running:
			s.startGroupWhenReadyLocked(gc)
		}
	}
	dc := s.dc
	s.mu.Unlock()
	s.refreshPool(dc, false)
	s.applyChronyClients()
	for _, e := range stopped {
		e.Stop()
	}
}

// RunReevaluate calls reevaluate every few seconds until the context ends.
func (s *Supervisor) RunReevaluate(ctx context.Context) {
	s.mu.Lock()
	s.dnsdirSeen = dnsdirPresent()
	s.mu.Unlock()
	t := time.NewTicker(piggybackEvery / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reevaluate()
		}
	}
}

// piggybackRefusal refuses the edits that make no sense while dnsdir is installed: a gateway of ntpdir's own, and anything on
// the gateway that follows dnsdir's addresses except its name, its NTP servers and pausing it.
func piggybackRefusal(dc *DaemonConfig, e canvasEdit) error {
	if e.Kind == "gateway" && e.Action == "add" && dnsdirPresent() {
		return errors.New("dnsdir is installed on this host, so ntpdir has no gateways of its own: it answers NTP on the addresses of dnsdir's gateways (add NTP servers to one of them)")
	}
	g, err := findGroup(dc, e.Group)
	if err != nil || !g.Piggyback {
		return nil
	}
	switch {
	case e.Kind == "anycast" || e.Kind == "dnsdirvip":
		return errors.New("this gateway answers on dnsdir's addresses, all of them; they come from dnsdir and cannot be chosen here")
	case e.Kind == "gateway" && e.Action == "del":
		return errors.New("this gateway mirrors a gateway of dnsdir; it goes when dnsdir's gateway does")
	case e.Kind == "gateway" && e.Action == "set" && e.Label != "":
		return errors.New("this gateway has dnsdir's name for its gateway; rename it in dnsdir")
	case e.Kind == "gateway" && e.Action == "set" && (e.VIP != "" || e.VIP6 != "" || e.Interface != "" || e.Anycast != "" || e.RealMACs != ""):
		return errors.New("this gateway has no address, interface or virtual MACs of its own: it answers on dnsdir's addresses")
	}
	return nil
}

// piggybackServing says whether this node answers for a gateway that mirrors one of dnsdir's: it holds at least one of its
// addresses, is not paused, and has an NTP server that answers (or a fallback in use).  g has had markPiggyback applied.
func piggybackServing(g CanvasGateway) bool {
	switch g.Status {
	case "ok":
		return true
	case "warn":
		return len(g.Servers) > 0
	}
	return false
}
