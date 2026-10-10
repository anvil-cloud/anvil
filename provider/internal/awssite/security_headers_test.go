package awssite

import (
	"strings"
	"testing"

	"github.com/anvil-cloud/anvil/provider/sites"
)

func TestSecurityHeaderDefaults(t *testing.T) {
	for _, args := range []*sites.SiteSecurityHeadersArgs{nil, {}} {
		cfg, err := ResolveSecurityHeaders(args)
		if err != nil {
			t.Fatal(err)
		}
		want := SecurityHeadersConfig{
			Enabled:                 true,
			FrameOptions:            FrameOptionsSameOrigin,
			PermissionsPolicy:       DefaultPermissionsPolicy,
			CrossOriginOpenerPolicy: COOPSameOriginAllowPopups,
		}
		if cfg != want {
			t.Errorf("defaults = %+v, want %+v", cfg, want)
		}
	}
	if strings.Contains(DefaultPermissionsPolicy, "payment") {
		t.Error("default Permissions-Policy must not block payment (Apple Pay / Google Pay buttons)")
	}
}

func TestSecurityHeaderOptions(t *testing.T) {
	off := false
	tests := []struct {
		name  string
		args  sites.SiteSecurityHeadersArgs
		check func(SecurityHeadersConfig) bool
	}{
		{"disabled", sites.SiteSecurityHeadersArgs{Enabled: &off}, func(c SecurityHeadersConfig) bool { return !c.Enabled }},
		{"frame deny", sites.SiteSecurityHeadersArgs{FrameOptions: "deny"}, func(c SecurityHeadersConfig) bool { return c.FrameOptions == FrameOptionsDeny }},
		{"frame none", sites.SiteSecurityHeadersArgs{FrameOptions: "none"}, func(c SecurityHeadersConfig) bool { return c.FrameOptions == FrameOptionsNone }},
		{"hsts extras", sites.SiteSecurityHeadersArgs{Hsts: &sites.SiteHstsArgs{IncludeSubDomains: true, Preload: true}},
			func(c SecurityHeadersConfig) bool { return c.HstsIncludeSubDomains && c.HstsPreload }},
		{"custom permissions policy", sites.SiteSecurityHeadersArgs{PermissionsPolicy: "camera=(self)"},
			func(c SecurityHeadersConfig) bool { return c.PermissionsPolicy == "camera=(self)" }},
		{"no permissions policy", sites.SiteSecurityHeadersArgs{PermissionsPolicy: "none"},
			func(c SecurityHeadersConfig) bool { return c.PermissionsPolicy == "" }},
		{"strict coop", sites.SiteSecurityHeadersArgs{CrossOriginOpenerPolicy: "same-origin"},
			func(c SecurityHeadersConfig) bool { return c.CrossOriginOpenerPolicy == COOPSameOrigin }},
		{"no coop", sites.SiteSecurityHeadersArgs{CrossOriginOpenerPolicy: "none"},
			func(c SecurityHeadersConfig) bool { return c.CrossOriginOpenerPolicy == "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ResolveSecurityHeaders(&tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if !tt.check(cfg) {
				t.Errorf("unexpected config %+v", cfg)
			}
		})
	}
}

func TestSecurityHeaderValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    sites.SiteSecurityHeadersArgs
		wantErr string
	}{
		{"bad frame option", sites.SiteSecurityHeadersArgs{FrameOptions: "allow-all"}, `invalid securityHeaders.frameOptions "allow-all"`},
		{"preload without subdomains", sites.SiteSecurityHeadersArgs{Hsts: &sites.SiteHstsArgs{Preload: true}}, "preload requires hsts.includeSubDomains"},
		{"bad coop", sites.SiteSecurityHeadersArgs{CrossOriginOpenerPolicy: "unsafe-none"}, `invalid securityHeaders.crossOriginOpenerPolicy "unsafe-none"`},
		// A newline would let a value smuggle in extra headers.
		{"header injection", sites.SiteSecurityHeadersArgs{PermissionsPolicy: "camera=()\r\nSet-Cookie: x=1"}, "invalid character"},
		{"too long", sites.SiteSecurityHeadersArgs{PermissionsPolicy: strings.Repeat("a", 1800)}, "CloudFront allows at most 1783"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ResolveSecurityHeaders(&tt.args); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
