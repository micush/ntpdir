package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// roAuth: alice is in the login group, bob only in the read-only group "view".
type roAuth struct{ fakeAuth }

func (a *roAuth) InGroup(user, group string) (bool, error) {
	switch group {
	case "ntpdir,ntpdir-users":
		return user == "alice", nil
	case "view":
		return user == "bob", nil
	}
	return false, nil
}

func TestReadOnlyGroup(t *testing.T) {
	e := newWebEnv(t)
	e.ws.auth = &roAuth{}
	with := func(r *http.Request) {
		r.Header.Set("Cookie", cookie)
		if r.Method != "GET" {
			r.Header.Set("X-CSRF-Token", e.csrf)
		}
	}
	if r := e.login("bob", "pw"); r.code != 401 {
		t.Fatalf("a member of no group must not sign in: %d", r.code)
	}
	pol := *e.ws.policy.Load()
	pol.ReadGroup = "view"
	e.ws.policy.Store(&pol)

	if r := e.login("bob", "pw"); r.code != 200 || r.body["read_only"] != true {
		t.Fatalf("read-only login: %d %v", r.code, r.body)
	}
	if r := e.do("GET", "/api/session", nil, with); r.code != 200 || r.body["read_only"] != true {
		t.Fatalf("session: %d %v", r.code, r.body)
	}
	for _, p := range []string{"/api/gateways", "/api/config", "/api/qstats", "/api/log", "/api/users"} {
		if r := e.do("GET", p, nil, with); r.code == 403 || r.code == 401 {
			t.Errorf("GET %s was refused for a read-only account: %d %s", p, r.code, r.raw)
		}
	}
	for _, c := range []struct{ m, p string }{
		{"PUT", "/api/config"}, {"POST", "/api/schedule/run"}, {"POST", "/api/qstats/clear"}, {"POST", "/api/users/add"},
		{"POST", "/api/nettool"}, {"POST", "/api/power"}, {"POST", "/api/update/apply"}, {"POST", "/api/bgp/operate"},
	} {
		r := e.do(c.m, c.p, map[string]any{}, with)
		if r.code != 403 || !strings.Contains(string(r.raw), "read-only") {
			t.Errorf("%s %s as read-only: %d %s (want 403)", c.m, c.p, r.code, r.raw)
		}
	}
	for _, p := range []string{"/api/versions/get?id=x", "/api/versions/diff?a=x&b=y", "/api/versions/download"} {
		if r := e.do("GET", p, nil, with); r.code != 403 {
			t.Errorf("GET %s as read-only: %d (the whole configuration file must stay back)", p, r.code)
		}
	}
	if r := e.do("POST", "/api/logout", nil, with); r.code != 200 {
		t.Fatalf("a read-only account can sign out: %d", r.code)
	}

	// a member of a login group is not read-only and still changes things
	if r := e.login("alice", "pw"); r.code != 200 || r.body["read_only"] == true {
		t.Fatalf("alice: %d %v", r.code, r.body)
	}
	if r := e.do("POST", "/api/schedule/run", map[string]any{}, with); r.code == 403 {
		t.Fatalf("a full account was refused: %s", r.raw)
	}
}

func TestRedactSecrets(t *testing.T) {
	in := map[string]any{"a": map[string]any{"key": "abc", "password": "pw", "name": "n", "list": []any{map[string]any{"secret": "s", "key": ""}}}}
	out := redactSecrets(in)
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "abc") || strings.Contains(string(b), `"pw"`) || !strings.Contains(string(b), `"name":"n"`) || !strings.Contains(string(b), `"secret":"(hidden)"`) {
		t.Fatalf("redacted: %s", b)
	}
	if in["a"].(map[string]any)["key"] != "abc" {
		t.Fatal("the input was changed")
	}
}
