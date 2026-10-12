package main

import (
	"net/netip"
	"sync"
	"time"
)

// The last forwarding failures (a server did not answer a client's request in time), with who asked.  A server is
// marked DOWN by its probes, so when it flaps this says which requests, from which clients, it failed on.  Kept in
// memory only; the troubleshooting bundle lists them (ntpdir/ntp-forward-failures.json).

type fwdFailure struct {
	Time    string `json:"time"`
	Client  string `json:"client"`
	Version int    `json:"version"`
	Server  string `json:"server"`
	Error   string `json:"error"`
}

const fwdFailKeep = 200

var fwdFails struct {
	mu   sync.Mutex
	ring []fwdFailure
	next int
	n    uint64
}

func noteFwdFailure(req []byte, client netip.Addr, server string, err error) {
	f := fwdFailure{Time: time.Now().UTC().Format(time.RFC3339), Server: server, Version: ntpVersion(req), Error: err.Error()}
	if client.IsValid() {
		f.Client = client.Unmap().String()
	}
	fwdFails.mu.Lock()
	if len(fwdFails.ring) < fwdFailKeep {
		fwdFails.ring = append(fwdFails.ring, f)
	} else {
		fwdFails.ring[fwdFails.next] = f
		fwdFails.next = (fwdFails.next + 1) % fwdFailKeep
	}
	fwdFails.n++
	fwdFails.mu.Unlock()
}

// recentFwdFailures lists them oldest first, with how many there have been in all.
func recentFwdFailures() map[string]any {
	fwdFails.mu.Lock()
	defer fwdFails.mu.Unlock()
	out := make([]fwdFailure, 0, len(fwdFails.ring))
	out = append(out, fwdFails.ring[fwdFails.next:]...)
	out = append(out, fwdFails.ring[:fwdFails.next]...)
	return map[string]any{"total_since_start": fwdFails.n, "last": out}
}
