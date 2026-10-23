package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
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

// piggybackName is the name of the gateway created for dnsdir's addresses.
const piggybackName = "dnsdir"

// wantPiggyback returns dc changed so that, while dnsdir is installed, it has the piggyback gateway and that gateway
// answers on every address dnsdir has now.  changed says whether anything was altered.  Without dnsdir nothing is changed.
func wantPiggyback(dc *DaemonConfig) (changed bool) {
	if !dnsdirPresent() {
		return false
	}
	addrs := dnsdirOffered()
	for i := range dc.Groups {
		g := &dc.Groups[i]
		if !g.Piggyback {
			continue
		}
		if !sameList(g.DNSDirVIPs, addrs) {
			g.DNSDirVIPs = addrs
			return true
		}
		return false
	}
	id := firstGroupID
	used := map[int]bool{}
	for _, g := range dc.Groups {
		used[g.GroupID] = true
	}
	for used[id] {
		id++
	}
	g := defaultGroup()
	g.GroupID, g.Name, g.Piggyback, g.VIP4, g.VIP6, g.Interface = id, piggybackName, true, "", "", ""
	g.DNSDirVIPs = addrs
	g.Neighbors = nil
	dc.Groups = append(dc.Groups, g)
	sort.Slice(dc.Groups, func(i, j int) bool { return dc.Groups[i].GroupID < dc.Groups[j].GroupID })
	return true
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
	infof("piggyback: answering NTP on dnsdir's %d address(es)", len(dc.Groups[len(dc.Groups)-1].DNSDirVIPs))
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
		return errors.New("dnsdir is installed on this host, so ntpdir has no gateways of its own: it answers NTP on dnsdir's addresses (add NTP servers to the gateway named dnsdir)")
	}
	g, err := findGroup(dc, e.Group)
	if err != nil || !g.Piggyback {
		return nil
	}
	switch {
	case e.Kind == "anycast" || e.Kind == "dnsdirvip":
		return errors.New("this gateway answers on dnsdir's addresses, all of them; they come from dnsdir and cannot be chosen here")
	case e.Kind == "gateway" && e.Action == "del":
		return errors.New("this gateway answers on dnsdir's addresses; it goes when dnsdir is uninstalled")
	case e.Kind == "gateway" && e.Action == "set" && (e.VIP != "" || e.VIP6 != "" || e.Interface != "" || e.Anycast != "" || e.RealMACs != ""):
		return errors.New("this gateway has no address, interface or virtual MACs of its own: it answers on dnsdir's addresses")
	}
	return nil
}
