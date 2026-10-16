package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func authTestDirs(t *testing.T) (auth, zone string) {
	t.Helper()
	auth, zone = t.TempDir(), t.TempDir()
	oa, oz, oh, oc := chronyAuthDir, zoneinfoDir, chronyHasNTS, ntsCertSource
	chronyAuthDir, zoneinfoDir = filepath.Join(auth, "a"), zone
	chronyHasNTS = func() bool { return true }
	t.Cleanup(func() { chronyAuthDir, zoneinfoDir, chronyHasNTS, ntsCertSource = oa, oz, oh, oc })
	return chronyAuthDir, zone
}

func TestValidateKeysAndServerAuth(t *testing.T) {
	d := defaultNTP()
	d.Keys = []string{" 7  sha256  HEX:00ff10 ", "", "8 MD5 secretpass"}
	d.ServerAuth = []string{"NTP.Example.com nts", "10.0.0.5 key 7"}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(d.Keys, "|") != "7 SHA256 HEX:00ff10|8 MD5 secretpass" {
		t.Fatalf("keys: %q", d.Keys)
	}
	if strings.Join(d.ServerAuth, "|") != "10.0.0.5:123 key 7|ntp.example.com:123 nts" {
		t.Fatalf("server_auth: %q", d.ServerAuth)
	}
	bad := map[string]func(*NTPConfig){
		"two fields":    func(d *NTPConfig) { d.Keys = []string{"7 SHA256"} },
		"key 0":         func(d *NTPConfig) { d.Keys = []string{"0 SHA256 abc"} },
		"duplicate id":  func(d *NTPConfig) { d.Keys = []string{"1 MD5 a", "1 SHA1 b"} },
		"unknown hash":  func(d *NTPConfig) { d.Keys = []string{"1 CRC32 abc"} },
		"odd hex":       func(d *NTPConfig) { d.Keys = []string{"1 MD5 HEX:abc"} },
		"hash sign":     func(d *NTPConfig) { d.Keys = []string{"1 MD5 a#b"} },
		"unknown key":   func(d *NTPConfig) { d.ServerAuth = []string{"10.0.0.5 key 9"} },
		"both options":  func(d *NTPConfig) { d.ServerAuth = []string{"10.0.0.5 nts key 1"} },
		"twice":         func(d *NTPConfig) { d.ServerAuth = []string{"a.example.com nts", "A.example.com:123 nts"} },
		"bad server":    func(d *NTPConfig) { d.ServerAuth = []string{"bad host nts"} },
		"bad leap mode": func(d *NTPConfig) { d.LeapMode = "smear" },
		"bad tz":        func(d *NTPConfig) { d.LeapTZ = "../etc/passwd" },
	}
	for name, mut := range bad {
		x := defaultNTP()
		mut(&x)
		if err := x.Validate(); err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}
	x := defaultNTP()
	x.LeapMode = "system"
	if err := x.Validate(); err != nil || x.LeapMode != "" {
		t.Fatalf("system is the default and is kept as empty: %v %q", err, x.LeapMode)
	}
}

func TestChronyFileHasKeysNTSAndLeapSettings(t *testing.T) {
	authDir, zone := authTestDirs(t)
	_ = os.MkdirAll(filepath.Join(zone, "right"), 0o755)
	_ = os.WriteFile(filepath.Join(zone, "right", "UTC"), []byte("x"), 0o644)
	cert, key := filepath.Join(t.TempDir(), "c.pem"), filepath.Join(t.TempDir(), "k.pem")
	_ = os.WriteFile(cert, []byte("CERT"), 0o644)
	_ = os.WriteFile(key, []byte("KEY"), 0o600)
	ntsCertSource = func() (string, string) { return cert, key }
	d := defaultNTP()
	d.Keys = []string{"7 SHA256 HEX:00ff10"}
	d.NTSServer, d.LeapTZ, d.LeapMode, d.LeapSmear = true, "right/UTC", "slew", true
	af, warns := prepareAuth(d)
	if len(warns) != 0 {
		t.Fatal(warns)
	}
	body, err := chronyConfBodyAuth(d, false, af)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"keyfile " + authDir + "/ntpdir.keys", "ntsservercert " + authDir + "/nts.crt", "ntsserverkey " + authDir + "/nts.key", "leapsectz right/UTC", "leapsecmode slew", "smoothtime 400 0.001 leaponly", "# authsum "} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
	st, err := os.Stat(authDir + "/ntpdir.keys")
	if err != nil || st.Mode().Perm() != 0o640 {
		t.Fatalf("key file: %v %v", st, err)
	}
	if b, _ := os.ReadFile(authDir + "/nts.key"); string(b) != "KEY" {
		t.Fatal("key copy")
	}
	rk := restartKey(body)
	for _, want := range []string{"keyfile ", "ntsservercert ", "leapsectz ", "leapsecmode ", "smoothtime ", "# authsum "} {
		if !strings.Contains(rk, want) {
			t.Errorf("restart key lacks %q: %q", want, rk)
		}
	}
	// a renewed certificate changes the sum, so chrony is restarted to read it
	_ = os.WriteFile(cert, []byte("CERT2"), 0o644)
	af2, _ := prepareAuth(d)
	if af2.sum == af.sum || af2.sum == "" {
		t.Fatal("a new certificate should change the sum")
	}
	// switching everything off removes the files and the lines
	off := defaultNTP()
	af3, _ := prepareAuth(off)
	b3, _ := chronyConfBodyAuth(off, false, af3)
	if strings.Contains(b3, "keyfile") || strings.Contains(b3, "nts") || strings.Contains(b3, "leapsec") || strings.Contains(b3, "smoothtime") || af3.sum != "" {
		t.Fatalf("%s", b3)
	}
	if _, err := os.Stat(authDir + "/ntpdir.keys"); err == nil {
		t.Fatal("the key file should be gone")
	}
	if _, err := os.Stat(authDir + "/nts.crt"); err == nil {
		t.Fatal("the certificate copy should be gone")
	}
}

func TestLeapTableMissingOnThisNodeIsLeftOut(t *testing.T) {
	authTestDirs(t)
	d := defaultNTP()
	d.LeapTZ = "right/UTC"
	af, _ := prepareAuth(d)
	if b, _ := chronyConfBodyAuth(d, false, af); strings.Contains(b, "leapsectz") {
		t.Fatalf("chrony does not start with a table it cannot find:\n%s", b)
	}
}

func TestNTSWithoutChronySupportIsIgnored(t *testing.T) {
	authTestDirs(t)
	chronyHasNTS = func() bool { return false }
	d := defaultNTP()
	d.NTSServer = true
	af, warns := prepareAuth(d)
	if af.ntsCert != "" || len(warns) == 0 {
		t.Fatalf("%+v %v", af, warns)
	}
}

func TestSourceLinesCarryTheirAuthentication(t *testing.T) {
	auth, err := parseServerAuth([]string{"time.cloudflare.com nts", "10.0.0.5 key 7"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := chronySourcesFileAuth([]string{"time.cloudflare.com:123", "10.0.0.5:123", "pool.ntp.org:123"}, auth)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"server time.cloudflare.com iburst nts", "server 10.0.0.5 iburst key 7", "server pool.ntp.org iburst\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
}

func TestAuthenticatedServerWaitsForTheRestart(t *testing.T) {
	authTestDirs(t)
	dir := t.TempDir()
	oldSrc, oldConf := chronySourceDir, chronyConfDir
	chronySourceDir, chronyConfDir = dir+"/s", dir+"/c"
	oc := chronyc
	chronyc = func(args ...string) ([]byte, error) { return []byte("200 OK"), nil }
	defer func() { chronySourceDir, chronyConfDir, chronyc = oldSrc, oldConf, oc }()
	m := &chronyMgr{warned: map[string]time.Time{}}
	d := defaultNTP()
	d.Keys = []string{"7 SHA256 HEX:00ff10"}
	d.ServerAuth = []string{"10.0.0.5 key 7"}
	if err := m.applyClients(d); err != nil {
		t.Fatal(err)
	}
	auth, _ := parseServerAuth(d.ServerAuth, nil)
	m.setAuth(auth)
	if err := m.applySources([]string{"10.0.0.5:123", "1.1.1.1:123"}, false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(chronySourceDir + "/ntpdir.sources")
	if strings.Contains(string(b), "10.0.0.5") || !strings.Contains(string(b), "1.1.1.1") {
		t.Fatalf("until chrony has the keys, the keyed server stays out (never used unauthenticated):\n%s", b)
	}
	// chrony restarted with the file: the keyed server comes in, with its key
	m.keyFile = dir + "/applied"
	m.markApplied()
	if err := m.applySources([]string{"10.0.0.5:123", "1.1.1.1:123"}, false); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(chronySourceDir + "/ntpdir.sources")
	if !strings.Contains(string(b), "server 10.0.0.5 iburst key 7") {
		t.Fatalf("%s", b)
	}
}

func TestKeysStayOutOfSupportBundlesAndHistory(t *testing.T) {
	v, n := redactJSON(map[string]any{"ntp": map[string]any{"keys": []any{"7 SHA256 HEX:00ff", "8 MD5 x"}, "server_auth": []any{"a nts"}}})
	m := v.(map[string]any)["ntp"].(map[string]any)
	if m["keys"] != tshootRedacted || n < 1 {
		t.Fatalf("%v (%d)", m, n)
	}
	a, b := newDaemonConfig(), newDaemonConfig()
	b.NTP.Keys = []string{"7 SHA256 HEX:deadbeef"}
	var text string
	for _, s := range configSections(a, b) {
		text += s.Label + ": " + s.Detail + "\n"
	}
	if !strings.Contains(text, "NTP keys: changed (1 now)") || strings.Contains(text, "deadbeef") {
		t.Fatalf("%q", text)
	}
}

func TestAuthAndLeapSettingsAreSharedAndNotKeptInPools(t *testing.T) {
	a := newDaemonConfig()
	a.NTP.Keys = []string{"7 SHA256 HEX:00ff10"}
	a.NTP.ServerAuth = []string{"10.0.0.5:123 key 7"}
	a.NTP.NTSServer, a.NTP.LeapTZ, a.NTP.LeapSmear = true, "right/UTC", true
	sh := sharedOf(a)
	b := newDaemonConfig()
	if _, err := mergeShared(b, sh, nil); err != nil {
		t.Fatal(err)
	}
	if len(b.NTP.Keys) != 1 || !b.NTP.NTSServer || b.NTP.LeapTZ != "right/UTC" || !b.NTP.LeapSmear || len(b.NTP.ServerAuth) != 1 {
		t.Fatalf("not replicated: %+v", b.NTP)
	}
	own := ownPoolFrom(a.NTP)
	if len(own.Keys) != 0 || own.NTSServer || own.LeapTZ != "" || len(own.ServerAuth) != 0 {
		t.Fatalf("a gateway's own pool must not copy them: %+v", own)
	}
	if l := a.NTP.live(); len(l.Keys) != 0 || l.LeapSmear {
		t.Fatal("the probe pool must not restart for them")
	}
}
