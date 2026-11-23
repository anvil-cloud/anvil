package awssite

import "testing"

func TestResolveHostname(t *testing.T) {
	cases := []struct {
		name, domain, zone, stage, env string
		wantHost, wantZone             string
	}{
		{"prod keeps domain", "www.example.com", "", "prod", "prod", "www.example.com", "example.com"},
		{"nonprod inserts stage", "www.example.com", "", "test", "nonprod", "www.test.example.com", "example.com"},
		{"apex nonprod", "example.com", "", "test", "nonprod", "test.example.com", "example.com"},
		{"apex prod", "example.com", "", "prod", "prod", "example.com", "example.com"},
		{"explicit zone", "www.example.co.uk", "example.co.uk", "pr-142", "nonprod", "www.pr-142.example.co.uk", "example.co.uk"},
		{"deep subdomain", "app.eu.example.com", "", "dev", "nonprod", "app.eu.dev.example.com", "example.com"},
		{"normalises case and dots", "WWW.Example.com.", "example.com.", "Test", "nonprod", "www.test.example.com", "example.com"},
		{"sanitises stage", "www.example.com", "", "Damien_Pace.2", "nonprod", "www.damien-pace-2.example.com", "example.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, zone, err := ResolveHostname(c.domain, c.zone, c.stage, c.env)
			if err != nil {
				t.Fatal(err)
			}
			if host != c.wantHost || zone != c.wantZone {
				t.Fatalf("got (%q, %q), want (%q, %q)", host, zone, c.wantHost, c.wantZone)
			}
		})
	}
}

func TestResolveHostnameErrors(t *testing.T) {
	if _, _, err := ResolveHostname("www.example.com", "other.com", "test", "nonprod"); err == nil {
		t.Error("domain outside zone: want error")
	}
	if _, _, err := ResolveHostname("www.example.com", "", "___", "nonprod"); err == nil {
		t.Error("stage with no usable characters: want error")
	}
	if _, _, err := ResolveHostname("www.example.com", "", "___", "prod"); err != nil {
		t.Errorf("prod ignores stage: %v", err)
	}
}

func TestStageLabelLength(t *testing.T) {
	long := ""
	for i := 0; i < 70; i++ {
		long += "a"
	}
	label, err := StageLabel(long)
	if err != nil || len(label) != 63 {
		t.Fatalf("got %q (%d), %v", label, len(label), err)
	}
}

func TestPairedName(t *testing.T) {
	cases := []struct {
		host, env string
		enabled   bool
		want      string
	}{
		{"youlosei.win", "prod", true, "www.youlosei.win"},
		{"www.youlosei.win", "prod", true, "youlosei.win"},
		{"app.youlosei.win", "prod", true, ""},
		{"youlosei.win", "prod", false, ""},
		{"damien.youlosei.win", "nonprod", true, ""},
		{"www.damien.youlosei.win", "nonprod", true, ""},
	}
	for _, c := range cases {
		if got := PairedName(c.host, "youlosei.win", c.env, c.enabled); got != c.want {
			t.Errorf("PairedName(%q, %s, %v) = %q, want %q", c.host, c.env, c.enabled, got, c.want)
		}
	}
}
