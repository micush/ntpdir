package main

import (
	"encoding/json"
	"net"
	"os"
	"sort"
	"strings"
)

// The Anyname DNS Director (dnsdir) can live on the same host.  The two products cannot share BGP (each owns
// /etc/frr/frr.conf, see bgp.go), so while dnsdir is installed ntpdir does no BGP of its own; instead a gateway may
// answer NTP on chosen addresses of dnsdir: anycast addresses it holds on lo and announces, or the shared address of one of its
// gateways (GroupConfig.DNSDirVIPs).  ntpdir only listens on them: it never adds or removes them and never announces them.

// dnsdirMarkers are what an installed dnsdir leaves behind (a variable for the tests).  The state directory is not
// one of them: it stays after an uninstall without --purge.
var dnsdirMarkers = []string{"/usr/local/sbin/dnsdir", "/usr/local/share/dnsdir", "/etc/systemd/system/dnsdir.service"}

// dnsdirConfPath is dnsdir's configuration, where its anycast addresses are listed (a variable for the tests).
var dnsdirConfPath = "/var/lib/dnsdir/dnsdir.conf"

// dnsdirPresent says whether dnsdir is installed on this host.
func dnsdirPresent() bool {
	for _, p := range dnsdirMarkers {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// dnsdirOffered lists the addresses dnsdir has that a gateway may answer NTP on, sorted: the anycast addresses of its
// gateways (extra_vips) and their shared addresses (vip4, vip6, more_vip4, more_vip6, without the prefix length).
// Nothing when dnsdir is not installed or its configuration cannot be read.
func dnsdirOffered() []string {
	any, shared := dnsdirAddrs()
	out := append(any, shared...)
	sort.Strings(out)
	return out
}

// dnsdirSharedSet is the set of dnsdir's gateway shared addresses (see dnsdirAddrs).
func dnsdirSharedSet() map[string]bool {
	_, shared := dnsdirAddrs()
	m := map[string]bool{}
	for _, a := range shared {
		m[a] = true
	}
	return m
}

// dnsdirAddrs reads dnsdir's configuration: its anycast addresses, and the shared addresses of its gateways.  The shared
// ones are held by dnsdir on whichever node serves the gateway (they are not on lo everywhere), and ntpdir only listens
// on them.
func dnsdirAddrs() (anycast, shared []string) {
	anycast, shared = []string{}, []string{}
	if !dnsdirPresent() {
		return
	}
	b, err := os.ReadFile(dnsdirConfPath)
	if err != nil {
		return
	}
	var c struct {
		Groups []struct {
			ExtraVIPs []string `json:"extra_vips"`
			VIP4      string   `json:"vip4"`
			VIP6      string   `json:"vip6"`
			MoreVIP4  []string `json:"more_vip4"`
			MoreVIP6  []string `json:"more_vip6"`
		} `json:"groups"`
	}
	if json.Unmarshal(b, &c) != nil {
		return
	}
	seen := map[string]bool{}
	add := func(dst *[]string, a string) {
		a = strings.TrimSpace(a)
		if ip, _, err := net.ParseCIDR(a); err == nil {
			a = ip.String()
		}
		if n, err := normalizeAnycast(a); err == nil && !seen[n] {
			seen[n] = true
			*dst = append(*dst, n)
		}
	}
	for _, g := range c.Groups {
		for _, a := range g.ExtraVIPs {
			add(&anycast, a)
		}
	}
	for _, g := range c.Groups {
		for _, a := range append(append([]string{g.VIP4, g.VIP6}, g.MoreVIP4...), g.MoreVIP6...) {
			if a != "" {
				add(&shared, a)
			}
		}
	}
	sort.Strings(anycast)
	sort.Strings(shared)
	return
}

// DNSDirInfo is what the GUI needs to offer dnsdir's addresses.
type DNSDirInfo struct {
	Present bool     `json:"present"`
	Offered []string `json:"offered"`
}

func dnsdirInfo() DNSDirInfo { return DNSDirInfo{Present: dnsdirPresent(), Offered: dnsdirOffered()} }

// checkDNSDirAdded refuses a dnsdir address that was not on the gateway before unless dnsdir is installed here and
// announces it.  Addresses already in the configuration are not re-checked, so a config stays loadable (and its
// settings are kept) while dnsdir is away.
func checkDNSDirAdded(cur, next *DaemonConfig) error {
	had := map[string]bool{}
	for _, g := range cur.Groups {
		for _, a := range g.DNSDirVIPs {
			had[a] = true
		}
	}
	var offered map[string]bool
	for _, g := range next.Groups {
		for _, a := range g.DNSDirVIPs {
			if had[a] {
				continue
			}
			if !dnsdirPresent() {
				return errDNSDir("the Anyname DNS Director (dnsdir) is not installed on this host")
			}
			if offered == nil {
				offered = map[string]bool{}
				for _, o := range dnsdirOffered() {
					offered[o] = true
				}
			}
			if !offered[a] {
				return errDNSDir(a + " is not an anycast address of dnsdir")
			}
		}
	}
	return nil
}

type errDNSDir string

func (e errDNSDir) Error() string { return "dnsdir address: " + string(e) }
