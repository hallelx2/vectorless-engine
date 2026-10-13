package config

import "testing"

// TestDefaultOrgFromEnv: a self-host with no control plane names its
// single tenant with VLS_AUTH_DEFAULT_ORG; by default there is none, so
// the server keeps requiring X-Vectorless-Org behind the control plane.
func TestDefaultOrgFromEnv(t *testing.T) {
	t.Setenv("VLS_AUTH_DEFAULT_ORG", "")
	cfg := Default()
	applyEnvOverrides(&cfg)
	if cfg.Auth.DefaultOrg != "" {
		t.Fatalf("default config has default_org %q, want none", cfg.Auth.DefaultOrg)
	}

	t.Setenv("VLS_AUTH_DEFAULT_ORG", "00000000-0000-0000-0000-000000000000")
	cfg = Default()
	applyEnvOverrides(&cfg)
	if cfg.Auth.DefaultOrg != "00000000-0000-0000-0000-000000000000" {
		t.Fatalf("VLS_AUTH_DEFAULT_ORG not applied: %q", cfg.Auth.DefaultOrg)
	}
}
