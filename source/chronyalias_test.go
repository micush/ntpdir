package main

import "testing"

// A server written as a host name is found in chrony's numeric list through the name chrony was configured with.
func TestChronyHostNameServersAreFound(t *testing.T) {
	f := hookChrony(t)
	f.answers["-c -m tracking sources -n"] = "A9FE0101,ntp1,2,1.0,0.0,0.0,0.0,0.0,0.0,0.0,0.01,0.01,64.0,Normal\n" +
		"^,-,162.159.200.1,3,10,377,779,0.0,0.0,0.0\n^,*,216.239.35.8,1,10,377,825,0.0,0.0,0.0\n^,-,129.6.15.29,1,10,377,511,0.0,0.0,0.0\n"
	f.answers["-c -N sources"] = "^,-,time.cloudflare.com,3,10,377,779,0.0,0.0,0.0\n^,*,time.google.com,1,10,377,825,0.0,0.0,0.0\n^,-,time.nist.gov,1,10,377,511,0.0,0.0,0.0\n"
	st := chrony.readState()
	if len(st.Sources) != 3 {
		t.Fatalf("%+v", st)
	}
	for addr, want := range map[string]string{"time.google.com": "*", "time.nist.gov": "-", "time.cloudflare.com": "-", "216.239.35.8": "*", "time.example.org": ""} {
		if got := st.stateOf(addr); got != want {
			t.Errorf("stateOf(%s) = %q, want %q", addr, got, want)
		}
	}
	// asked again only when the list changes (or after a minute)
	n := len(f.calls)
	chrony.readState()
	for _, c := range f.calls[n:] {
		for _, a := range c {
			if a == "-N" {
				t.Fatal("the names were asked for again at the next look")
			}
		}
	}
}

// A chrony that does not know -N, or lists that do not line up, leave the numeric names as they were.
func TestChronyHostNameAliasFallsBack(t *testing.T) {
	f := hookChrony(t)
	f.answers["-c -m tracking sources -n"] = "A9FE0101,ntp1,2,1.0,0.0,0.0,0.0,0.0,0.0,0.0,0.01,0.01,64.0,Normal\n^,*,216.239.35.8,1,10,377,825,0.0,0.0,0.0\n"
	st := chrony.readState() // no answer for -N: an error
	if len(st.Sources) != 1 || st.Sources[0].Alias != "" || st.stateOf("216.239.35.8") != "*" || st.stateOf("time.google.com") != "" {
		t.Fatalf("%+v", st.Sources)
	}
}
