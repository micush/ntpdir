package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dnsdirWith installs a fake dnsdir (a marker and a configuration) for the test and returns the path of the configuration.
func dnsdirWith(t *testing.T, conf string) string {
	t.Helper()
	withDnsdir(t)
	p := filepath.Join(t.TempDir(), "dnsdir.conf")
	if err := os.WriteFile(p, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	old := dnsdirConfPath
	dnsdirConfPath = p
	t.Cleanup(func() { dnsdirConfPath = old })
	return p
}

const dnsdirConf = `{"groups":[
 {"group_id":1,"vip4":"10.0.0.53/24","vip6":"2001:db8::53/64","more_vip4":["10.0.0.54/24"],"extra_vips":["203.0.113.53"]},
 {"group_id":2,"vip4":"10.0.1.53/24","extra_vips":["203.0.113.54","203.0.113.53"]}]}`

func TestDNSDirAddrs(t *testing.T) {
	dnsdirWith(t, dnsdirConf)
	any, shared := dnsdirAddrs()
	if strings.Join(any, " ") != "203.0.113.53 203.0.113.54" {
		t.Errorf("anycast: %v", any)
	}
	if strings.Join(shared, " ") != "10.0.0.53 10.0.0.54 10.0.1.53 2001:db8::53" {
		t.Errorf("shared (the prefix length is dropped): %v", shared)
	}
	if got := dnsdirOffered(); len(got) != 6 {
		t.Errorf("offered: %v", got)
	}
	if set := dnsdirSharedSet(); !set["10.0.0.53"] || set["203.0.113.53"] {
		t.Errorf("shared set: %v", set)
	}
}

func TestWantPiggyback(t *testing.T) {
	dc := newDaemonConfig()
	g := defaultGroup()
	dc.Groups = []GroupConfig{g}
	// no dnsdir: nothing is added
	old := dnsdirMarkers
	dnsdirMarkers = []string{filepath.Join(t.TempDir(), "absent")}
	if wantPiggyback(dc) || len(dc.Groups) != 1 {
		t.Fatal("changed without dnsdir")
	}
	dnsdirMarkers = old
	conf := dnsdirWith(t, dnsdirConf)
	if !wantPiggyback(dc) || len(dc.Groups) != 2 {
		t.Fatalf("no gateway was made: %+v", dc.Groups)
	}
	p := dc.Groups[1]
	if !p.Piggyback || p.Name != "dnsdir" || p.GroupID == g.GroupID || len(p.DNSDirVIPs) != 6 || p.VIP4 != "" {
		t.Fatalf("piggyback gateway: %+v", p)
	}
	if err := dc.Validate(); err != nil {
		t.Fatalf("a gateway with no address of its own must validate: %v", err)
	}
	if wantPiggyback(dc) {
		t.Fatal("changed again with nothing new")
	}
	// dnsdir gets a new address: the gateway follows
	if err := os.WriteFile(conf, []byte(`{"groups":[{"group_id":1,"vip4":"10.0.0.53/24","extra_vips":["203.0.113.99"]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !wantPiggyback(dc) || strings.Join(dc.Groups[1].DNSDirVIPs, " ") != "10.0.0.53 203.0.113.99" || len(dc.Groups) != 2 {
		t.Fatalf("did not follow dnsdir: %+v", dc.Groups)
	}
}

func TestPiggybackValidate(t *testing.T) {
	g := defaultGroup()
	g.Piggyback, g.VIP4, g.Interface = true, "", ""
	if err := g.Validate(); err != nil {
		t.Errorf("no address and no interface: %v", err)
	}
	g.VIP4 = "10.0.0.1/24"
	if err := g.Validate(); err == nil {
		t.Error("a piggyback gateway with an address of its own was accepted")
	}
	a, b := defaultGroup(), defaultGroup()
	a.Piggyback, a.VIP4, a.Interface, a.GroupID = true, "", "", 100
	b.Piggyback, b.VIP4, b.Interface, b.GroupID = true, "", "", 101
	dc := newDaemonConfig()
	dc.Groups = []GroupConfig{a, b}
	if err := dc.Validate(); err == nil {
		t.Error("two piggyback gateways were accepted")
	}
	// the flag travels with the shared settings
	dc.Groups = []GroupConfig{a}
	sh := sharedOf(dc)
	if len(sh.Groups) != 1 || !sh.Groups[0].Piggyback {
		t.Fatalf("shared: %+v", sh.Groups)
	}
	var back GroupConfig
	applySharedGroup(&back, sh.Groups[0])
	if !back.Piggyback {
		t.Fatal("not applied")
	}
}

func TestInactiveReason(t *testing.T) {
	own, pig := defaultGroup(), defaultGroup()
	pig.Piggyback = true
	old := dnsdirMarkers
	dnsdirMarkers = []string{filepath.Join(t.TempDir(), "absent")}
	if inactiveReason(own) != "" || inactiveReason(pig) == "" {
		t.Error("without dnsdir the own gateway runs and the piggyback one does not")
	}
	dnsdirMarkers = old
	withDnsdir(t)
	if inactiveReason(own) == "" || inactiveReason(pig) != "" {
		t.Error("with dnsdir the own gateway does not run and the piggyback one does")
	}
}

// With dnsdir installed a gateway of ntpdir's own is not started (no engine, no anycast) and says why; the piggyback gateway
// is, and answers on dnsdir's addresses.
func TestSupervisorStartsOnlyThePiggybackGateway(t *testing.T) {
	dnsdirWith(t, dnsdirConf)
	own := defaultGroup()
	own.GroupID = 100
	pig := defaultGroup()
	pig.GroupID, pig.Piggyback, pig.VIP4, pig.Interface, pig.DNSDirVIPs = 101, true, "", "", []string{"203.0.113.53"}
	dc := newDaemonConfig()
	dc.Groups = []GroupConfig{own, pig}
	if err := dc.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewSupervisor(ctx, dc)
	s.mu.Lock()
	s.startGroupWhenReadyLocked(own)
	s.mu.Unlock()
	if why := s.inactiveGroups()[100]; why == "" {
		t.Fatal("the gateway of ntpdir's own was not held back")
	}
	if len(s.engines) != 0 {
		t.Fatalf("engines were started: %v", s.engines)
	}
	s.mu.Lock()
	s.startGroupLocked(pig)
	started := s.anycast[101] != nil
	s.mu.Unlock()
	if !started {
		t.Fatal("the piggyback gateway does not answer on dnsdir's address")
	}
	s.StopAll()
}

func TestMarkPiggyback(t *testing.T) {
	srv := func(st ...string) []CanvasServer {
		var out []CanvasServer
		for _, s := range st {
			out = append(out, CanvasServer{Status: s})
		}
		return out
	}
	up := func(n int, of int) []AnycastState {
		var out []AnycastState
		for i := 0; i < of; i++ {
			out = append(out, AnycastState{Addr: "10.0.0." + itoa(i+1), External: true, Up: i < n})
		}
		return out
	}
	for _, c := range []struct {
		name    string
		g       CanvasGateway
		status  string
		contain string
	}{
		{"no addresses", CanvasGateway{Servers: srv("ok")}, "idle", "no addresses"},
		{"none held here", CanvasGateway{Servers: srv("ok"), Anycast: up(0, 2)}, "idle", "none of dnsdir"},
		{"no servers", CanvasGateway{Anycast: up(2, 2)}, "warn", "no NTP server is configured"},
		{"all down", CanvasGateway{Servers: srv("bad", "bad"), Anycast: up(2, 2)}, "bad", "no NTP server is healthy"},
		{"some down", CanvasGateway{Servers: srv("ok", "bad"), Anycast: up(2, 2)}, "warn", "some NTP servers are down"},
		{"some held", CanvasGateway{Servers: srv("ok"), Anycast: up(1, 2)}, "warn", "1 of dnsdir's 2"},
		{"fine", CanvasGateway{Servers: srv("ok", "ok"), Anycast: up(2, 2)}, "ok", "all 2"},
		{"paused", CanvasGateway{Servers: srv("ok"), Anycast: up(2, 2), Paused: true}, "paused", "Paused"},
	} {
		c.g.Piggyback = true
		gs := []CanvasGateway{c.g}
		markPiggyback(gs)
		if gs[0].Status != c.status || !strings.Contains(gs[0].Detail, c.contain) {
			t.Errorf("%s: %s %q, want %s ... %q", c.name, gs[0].Status, gs[0].Detail, c.status, c.contain)
		}
	}
}

func TestPiggybackRefusal(t *testing.T) {
	dnsdirWith(t, dnsdirConf)
	own, pig := defaultGroup(), defaultGroup()
	own.GroupID, pig.GroupID, pig.Piggyback, pig.VIP4, pig.Interface = 100, 101, true, "", ""
	dc := newDaemonConfig()
	dc.Groups = []GroupConfig{own, pig}
	for _, e := range []canvasEdit{
		{Action: "add", Kind: "gateway", VIP: "10.9.0.1/24", Interface: "eth0"},
		{Action: "add", Kind: "anycast", Group: 101, Address: "203.0.113.9"},
		{Action: "add", Kind: "dnsdirvip", Group: 101, Address: "203.0.113.53"},
		{Action: "del", Kind: "gateway", Group: 101},
		{Action: "set", Kind: "gateway", Group: 101, VIP: "10.9.0.1/24"},
	} {
		if err := piggybackRefusal(dc, e); err == nil {
			t.Errorf("%+v was accepted", e)
		}
	}
	for _, e := range []canvasEdit{
		{Action: "set", Kind: "gateway", Group: 101, Label: "NTP"},
		{Action: "add", Kind: "server", Group: 101, Server: "10.1.1.1"},
		{Action: "set", Kind: "gateway", Group: 100, VIP: "10.9.0.1/24"},
	} {
		if err := piggybackRefusal(dc, e); err != nil {
			t.Errorf("%+v was refused: %v", e, err)
		}
	}
}
