package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// The rules of the alerts (see alerts.go) and what they read.

var alertDefs = []alertDef{
	{ID: "chrony_down", Title: "Chrony is not running", Subject: "", Kind: "updown", Sev: "critical", On: true, Fix: []string{"restart_chrony"}, Go: "ntp"},
	{ID: "not_synchronised", Title: "Clock not synchronised", Subject: "", Kind: "updown", Sev: "critical", On: true, DefMode: "minutes", DefAfter: 10, Go: "ntp"},
	{ID: "no_reachable_source", Title: "No time source answers", Subject: "", Kind: "updown", Sev: "critical", On: true, Fix: []string{"recheck"}, Go: "ntp"},
	{ID: "clock_offset", Title: "Clock is off", Subject: "", Kind: "value", Sev: "warning", On: true, Unit: "ms offset", Thresh: 100, ForMin: 10, Go: "ntp"},
	{ID: "stratum_high", Title: "Stratum is high", Subject: "", Kind: "value", Sev: "info", On: false, Unit: "stratum", Thresh: 4, ForMin: 15, Go: "ntp"},
	{ID: "upstream_down", Title: "Upstream server down", Subject: "upstream server", Kind: "updown", Sev: "warning", On: true, Fix: []string{"recheck"}, Go: "ntp"},
	{ID: "upstream_falseticker", Title: "Upstream server has the wrong time", Subject: "upstream server", Kind: "updown", Sev: "warning", On: true, Fix: []string{"recheck"}, Go: "ntp"},
	{ID: "fallback_in_use", Title: "Only fallback servers in use", Subject: "pool", Kind: "updown", Sev: "warning", On: true, Fix: []string{"recheck"}, Go: "ntp"},
	{ID: "listener_down", Title: "NTP listener down", Subject: "gateway", Kind: "updown", Sev: "critical", On: true, Fix: []string{"restart_chrony"}, Go: "gateways"},
	{ID: "no_active_gateway", Title: "No active gateway", Subject: "gateway", Kind: "updown", Sev: "critical", On: true, Go: "gateways"},
	{ID: "certificate_expiring", Title: "Certificate about to expire", Subject: "certificate", Kind: "value", Sev: "warning", On: true, Unit: "days left", Below: true, Thresh: 14, ForMin: 0, Go: "cfg_web"},
	{ID: "backup_failed", Title: "Backup failed", Subject: "", Kind: "updown", Sev: "warning", On: true, DefMode: "minutes", DefAfter: 30, Go: "cfg_backup"},
	{ID: "cpu_high", Title: "CPU busy", Subject: "", Kind: "value", Sev: "warning", On: true, Unit: "% CPU", Thresh: 90, ForMin: 15, Go: "host"},
	{ID: "memory_high", Title: "Memory nearly full", Subject: "", Kind: "value", Sev: "warning", On: true, Unit: "% memory", Thresh: 90, ForMin: 15, Go: "host"},
	{ID: "disk_full", Title: "Disk nearly full", Subject: "filesystem", Kind: "value", Sev: "warning", On: true, Unit: "% full", Thresh: 90, ForMin: 10, Go: "host"},
}

func alertDefByID(id string) (alertDef, bool) {
	for _, d := range alertDefs {
		if d.ID == id {
			return d, true
		}
	}
	return alertDef{}, false
}

// cntPoint is kept for the engine's per-key counter history (unused by the NTP rules, which read chrony's own state).
type cntPoint struct {
	at   time.Time
	q, f uint64
}

func poolLabel(s *StatusServer, key int) string {
	if key == 0 {
		return "shared"
	}
	if n := s.sup.groupName(key); n != "" {
		return n
	}
	return "group " + itoa(key)
}

// chronyNow is what chrony says about itself (replaceable in tests).
var chronyNow = func() ChronyState { return chrony.State() }

// sampleNode reads everything the rules look at, on this node.
func (e *AlertEngine) sampleNode(now time.Time) []alertSample {
	st := e.st
	if st == nil {
		return nil
	}
	var out []alertSample
	cs := chronyNow()
	out = append(out, alertSample{Rule: "chrony_down", Bad: !cs.Running, Message: "chrony is not running on this node"})
	if cs.Running {
		out = append(out, alertSample{Rule: "not_synchronised", Bad: !cs.Synced, Message: "this node's clock is not synchronised to any time source"})
		reach := false
		for _, c := range cs.Sources {
			if c.Reach > 0 {
				reach = true
			}
		}
		out = append(out, alertSample{Rule: "no_reachable_source", Bad: !reach, Message: fmt.Sprintf("none of chrony's %d time sources has answered its recent polls", len(cs.Sources))})
		if cs.Synced {
			off := math.Abs(cs.OffsetMS)
			out = append(out, alertSample{Rule: "clock_offset", Value: off, Bad: off > e.threshold("clock_offset"), Message: fmt.Sprintf("the system clock is %.1f ms from the time chrony believes", off)})
			out = append(out, alertSample{Rule: "stratum_high", Value: float64(cs.Stratum), Bad: float64(cs.Stratum) > e.threshold("stratum_high"), Message: fmt.Sprintf("this node is at stratum %d", cs.Stratum)})
		}
	}
	for _, pi := range st.sup.poolList() {
		pl := poolLabel(st, pi.Key)
		for _, sv := range st.namedServers(pi) {
			if sv.Fallback {
				continue
			}
			who := sv.Addr
			if sv.Name != "" {
				who = sv.Name + " (" + sv.Addr + ")"
			}
			subj := sv.Addr
			if pi.Key != 0 {
				subj += " · " + pl
			}
			msg := who + " does not pass its tests"
			if sv.LastError != "" {
				msg += ": " + sv.LastError
			}
			out = append(out, alertSample{Rule: "upstream_down", Subject: subj, Bad: !sv.Healthy, Message: msg})
			out = append(out, alertSample{Rule: "upstream_falseticker", Subject: subj, Bad: sv.Healthy && sv.Falseticker,
				Message: who + " gives a time that differs from the other servers by more than the allowed offset"})
		}
		out = append(out, alertSample{Rule: "fallback_in_use", Subject: pl, Bad: pi.Pool.UsingFallback(), Message: "every normal server of " + pl + " is down, so only fallback servers are used"})
	}
	type gk struct {
		gid int
		af  string
	}
	active, local, serving, up := map[gk]bool{}, map[gk]bool{}, map[gk]bool{}, map[gk]bool{}
	for _, r := range st.snapshot() {
		k := gk{r.GroupID, r.AF}
		if r.State == "active" {
			active[k] = true
		}
		if r.Local {
			local[k] = true
			if r.State == "active" || r.State == "forward" {
				serving[k] = true
				up[k] = r.NTPUp
			}
		}
	}
	for k := range local {
		subj := fmt.Sprintf("group %d %s", k.gid, k.af)
		out = append(out, alertSample{Rule: "no_active_gateway", Subject: subj, Bad: !active[k], Message: "no node is the active gateway of " + subj})
		if serving[k] {
			out = append(out, alertSample{Rule: "listener_down", Subject: subj, Bad: !up[k], Message: "this node serves " + subj + " but its NTP listener is not up"})
		}
	}
	if e.mg != nil && e.mg.certs != nil {
		if lc, err := e.mg.certs.current(false); err == nil && lc != nil && !lc.info.NotAfter.IsZero() {
			days := time.Until(lc.info.NotAfter).Hours() / 24
			out = append(out, alertSample{Rule: "certificate_expiring", Subject: "web", Value: days, Bad: days < e.threshold("certificate_expiring"),
				Message: fmt.Sprintf("the web certificate runs out on %s (%.0f days)", lc.info.NotAfter.Format("2006-01-02"), days)})
		}
	}
	if e.mg != nil && e.mg.backup != nil {
		if failing, enabled, msg := e.mg.backup.Failing(); enabled {
			out = append(out, alertSample{Rule: "backup_failed", Bad: failing, Message: "the last backup failed: " + msg})
		}
	}
	if h := hoststats.lastNow(); h.At != 0 {
		out = append(out, alertSample{Rule: "cpu_high", Value: h.CPU, Bad: h.CPU > e.threshold("cpu_high"), Message: fmt.Sprintf("CPU is at %.0f%%", h.CPU)})
		out = append(out, alertSample{Rule: "memory_high", Value: h.MemPct, Bad: h.MemPct > e.threshold("memory_high"), Message: fmt.Sprintf("memory is %.0f%% in use", h.MemPct)})
		for _, f := range h.FS {
			out = append(out, alertSample{Rule: "disk_full", Subject: f.Mount, Value: f.Pct, Bad: f.Pct > e.threshold("disk_full"), Message: fmt.Sprintf("%s is %.0f%% full", f.Mount, f.Pct)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Rule < out[j].Rule })
	return out
}

// threshold is the rule's threshold in force.
func (e *AlertEngine) threshold(id string) float64 {
	d, _ := alertDefByID(id)
	return e.cfg().rule(d).Threshold
}

// ── the Fix menu ─────────────────────────────────────────────────────────────

var alertFixes = map[string]string{
	"recheck":        "Check the upstream servers now",
	"restart_chrony": "Restart chrony",
}

func (e *AlertEngine) fix(action, actor string) (string, error) {
	if e.st == nil {
		return "", errors.New("not available")
	}
	switch action {
	case "recheck":
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		for _, pi := range e.st.sup.poolList() {
			go func(p *Pool) { p.ProbeNow(ctx) }(pi.Pool)
		}
		go func() { time.Sleep(60 * time.Second); cancel() }()
		return "The upstream servers are being checked.", nil
	case "restart_chrony":
		if err := chronyRestart(); err != nil {
			return "", err
		}
		return "Chrony was restarted.", nil
	}
	return "", errors.New("unknown action")
}

// ── the operations behind /api/alerts ────────────────────────────────────────

func (m *Mgmt) alertsOp(cmd string, raw json.RawMessage, actor string) (any, error) {
	e := m.alerts
	if e == nil {
		return nil, errors.New("alerts are not available on this node")
	}
	var a struct {
		ID      string `json:"id"`
		Ack     bool   `json:"ack"`
		Minutes int    `json:"minutes"`
		Action  string `json:"action"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
	}
	ok := map[string]any{"ok": true}
	switch cmd {
	case "alerts.list":
		return e.View(), nil
	case "alerts.ack":
		return ok, e.Ack(a.ID, a.Ack, actor)
	case "alerts.silence":
		return ok, e.Silence(a.ID, a.Minutes, actor)
	case "alerts.resolve":
		return ok, e.Resolve(a.ID, actor)
	case "alerts.test":
		if err := e.Test(actor); err != nil {
			return nil, err
		}
		return ok, nil
	case "alerts.fix":
		view := e.View()
		var allowed bool
		for _, al := range view.Alerts {
			if al.ID == a.ID {
				d, _ := alertDefByID(al.Rule)
				allowed = containsStr(d.Fix, a.Action)
			}
		}
		if !allowed {
			return nil, errors.New("that action does not go with that alert")
		}
		msg, err := e.fix(a.Action, actor)
		if err != nil {
			return nil, err
		}
		warnf("alerts: %q ran %q for an alert", actor, alertFixes[a.Action])
		return map[string]any{"ok": true, "message": msg}, nil
	}
	return nil, fmt.Errorf("unknown command %q", cmd)
}

var _ = strings.TrimSpace
