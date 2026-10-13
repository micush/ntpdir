package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestChronyMayGo(t *testing.T) {
	pv := func(addr string, self bool, roll string) PeerView {
		return PeerView{Addr: addr, Self: self, Reachable: true, ChronyRoll: roll}
	}
	cases := []struct {
		name  string
		peers []PeerView
		ok    bool
		why   string
	}{
		{"alone", []PeerView{pv("b", true, "pending")}, true, ""},
		{"nobody else busy", []PeerView{pv("b", true, "pending"), pv("a", false, ""), pv("c", false, "")}, true, ""},
		{"a lower address is waiting", []PeerView{pv("b", true, "pending"), pv("a", false, "pending")}, false, "a is first"},
		{"a higher address waits for us", []PeerView{pv("b", true, "pending"), pv("c", false, "pending")}, true, ""},
		{"someone is restarting", []PeerView{pv("b", true, "pending"), pv("c", false, "restarting")}, false, "c is restarting"},
		{"someone failed", []PeerView{pv("b", true, "pending"), pv("c", false, "failed")}, false, "c failed"},
		{"someone is updating", []PeerView{pv("b", true, "pending"), {Addr: "c", Reachable: true, Updating: true}}, false, "c is installing"},
	}
	for _, c := range cases {
		ok, why := chronyMayGo(c.peers)
		if ok != c.ok || !strings.Contains(why, c.why) {
			t.Errorf("%s: got %v %q", c.name, ok, why)
		}
	}
	// a member that cannot be reached is not waited for
	down := pv("a", false, "restarting")
	down.Reachable = false
	if ok, _ := chronyMayGo([]PeerView{pv("b", true, "pending"), down}); !ok {
		t.Error("an unreachable member held the roll")
	}
}

func TestApplyClientsNeverRestartsAndTracksTheKey(t *testing.T) {
	f := hookChrony(t)
	m := chrony
	m.keyFile = t.TempDir() + "/applied"
	d := defaultNTP()
	if err := m.applyClients(d); err != nil || f.restarts != 0 || m.needsRestart() {
		t.Fatalf("defaults: %v restarts %d need %v", err, f.restarts, m.needsRestart())
	}
	d.ClientRate = 16
	if err := m.applyClients(d); err != nil || f.restarts != 0 || !m.needsRestart() {
		t.Fatalf("new rate: %v restarts %d need %v", err, f.restarts, m.needsRestart())
	}
	m.markApplied()
	if m.needsRestart() {
		t.Fatal("still needed after it was applied")
	}
	d.AllowedClients = []string{"10.0.0.0/8"} // allow rules are live
	if err := m.applyClients(d); err != nil || m.needsRestart() {
		t.Fatalf("allow list: %v need %v", err, m.needsRestart())
	}
	d.ListenPort = 1123
	if err := m.applyClients(d); err != nil || !m.needsRestart() {
		t.Fatalf("port: %v need %v", err, m.needsRestart())
	}
}

// The node is paused, chrony restarted, and the node resumed only when chrony is synchronised again.
func TestChronyRollPausesRestartsWaitsResumes(t *testing.T) {
	f := hookChrony(t)
	od, os_ := chronyDrainWait, chronySyncWait
	chronyDrainWait, chronySyncWait = 10*time.Millisecond, 5*time.Second
	t.Cleanup(func() { chronyDrainWait, chronySyncWait = od, os_ })
	mg := newPowerMgmt(t)
	if err := newDaemonConfig().save(mg.confPath); err != nil {
		t.Fatal(err)
	}
	mg.othersFn = func() int { return 1 }
	chrony.keyFile = t.TempDir() + "/applied"
	d := defaultNTP()
	d.ClientRate = 8
	chrony.applyClients(d)
	if !chrony.needsRestart() {
		t.Fatal("no restart is waiting")
	}
	// chrony is up and synchronised only after the restart
	f.answers["-c tracking"] = ""
	synced := false
	pausedAtRestart := false
	chronyRestart = func() error {
		st, _ := mg.NodePauseStatus()
		pausedAtRestart = st.Paused
		synced = true
		return nil
	}
	ot := chronyRollTrack
	chronyRollTrack = func() ChronyState { return ChronyState{Running: true, Synced: synced} }
	t.Cleanup(func() { chronyRollTrack = ot })

	mg.croll.running.Store(true)
	mg.chronyRollRun(context.Background())
	if !pausedAtRestart {
		t.Fatal("chrony was restarted before the node was paused")
	}
	if st, _ := mg.NodePauseStatus(); st.Paused {
		t.Fatal("the node was not resumed")
	}
	if chrony.needsRestart() {
		t.Fatal("the new settings were not marked as applied")
	}
	if _, err := os.Stat(mg.rollMarkerPath()); err == nil {
		t.Fatal("the marker was left behind")
	}
	if mg.ChronyRollLabel() != "" {
		t.Fatalf("label %q", mg.ChronyRollLabel())
	}
}

// A node that cannot be taken out of service (it is the last) restarts chrony anyway; one that was paused before stays paused.
func TestChronyRollWithoutPauseAndFailure(t *testing.T) {
	hookChrony(t)
	od, os_ := chronyDrainWait, chronySyncWait
	chronyDrainWait, chronySyncWait = 10*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { chronyDrainWait, chronySyncWait = od, os_ })
	mg := newPowerMgmt(t)
	if err := newDaemonConfig().save(mg.confPath); err != nil {
		t.Fatal(err)
	}
	mg.othersFn = func() int { return 0 } // alone: the pause is refused
	chrony.keyFile = t.TempDir() + "/applied"
	d := defaultNTP()
	d.ListenPort = 1123
	chrony.applyClients(d)
	restarted := false
	chronyRestart = func() error { restarted = true; return nil }
	ot := chronyRollTrack
	chronyRollTrack = func() ChronyState { return ChronyState{Running: true, Synced: false} } // never synchronises
	t.Cleanup(func() { chronyRollTrack = ot })
	mg.croll.running.Store(true)
	mg.chronyRollRun(context.Background())
	if !restarted {
		t.Fatal("not restarted")
	}
	if st, _ := mg.NodePauseStatus(); st.Paused {
		t.Fatal("an unpausable node was paused")
	}
	if mg.ChronyRollLabel() != rollFailed {
		t.Fatalf("a chrony that does not synchronise is a failure, label %q", mg.ChronyRollLabel())
	}
}
