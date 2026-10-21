package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chaitin/agent-compose/pkg/egress"
)

func TestLoadGlobalNetworkPolicy(t *testing.T) {
	t.Setenv(EgressGlobalPolicyFileEnv, "")
	policy, err := loadGlobalNetworkPolicy()
	if err != nil || policy != nil {
		t.Fatalf("unset global policy = %+v, %v, want nil, nil", policy, err)
	}

	path := filepath.Join(t.TempDir(), "global-policy.yaml")
	data := []byte(`default: deny-all
allow:
  - host: "*.example.com"
    ports: [443, 80]
  - host: 10.0.0.0/8
    port: 443
deny:
  - host: metadata.example.com
    port: 80
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write policy file: %v", err)
	}
	t.Setenv(EgressGlobalPolicyFileEnv, path)
	policy, err = loadGlobalNetworkPolicy()
	if err != nil {
		t.Fatalf("loadGlobalNetworkPolicy() error = %v", err)
	}
	if policy == nil || policy.Default != egress.Deny {
		t.Fatalf("policy = %+v, want a deny-all policy", policy)
	}
	if len(policy.Allow) != 3 {
		t.Fatalf("allow = %+v, want the expanded ports", policy.Allow)
	}
	if policy.Allow[0].Host != "*.example.com" || policy.Allow[0].Port != 80 {
		t.Fatalf("allow[0] = %+v, want *.example.com:80", policy.Allow[0])
	}
	if policy.Allow[1].Host != "*.example.com" || policy.Allow[1].Port != 443 {
		t.Fatalf("allow[1] = %+v, want *.example.com:443", policy.Allow[1])
	}
	if policy.Allow[2].Host != "10.0.0.0/8" || policy.Allow[2].Port != 443 {
		t.Fatalf("allow[2] = %+v, want 10.0.0.0/8:443", policy.Allow[2])
	}
	if len(policy.Deny) != 1 || policy.Deny[0].Host != "metadata.example.com" {
		t.Fatalf("deny = %+v, want the declared denial", policy.Deny)
	}
}

func TestLoadGlobalNetworkPolicyRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty", body: ""},
		{name: "unknown field", body: "default: allow-all\nprotcol: https\n"},
		{name: "invalid default", body: "default: deny\n"},
		{name: "protocol field removed", body: "default: allow-all\nallow:\n  - host: api.example.com\n    port: 443\n    protocol: https\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "global-policy.yaml")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatalf("write policy file: %v", err)
			}
			t.Setenv(EgressGlobalPolicyFileEnv, path)
			if _, err := loadGlobalNetworkPolicy(); err == nil {
				t.Fatal("loadGlobalNetworkPolicy() accepted an invalid file")
			}
		})
	}
	t.Setenv(EgressGlobalPolicyFileEnv, filepath.Join(t.TempDir(), "missing.yaml"))
	if _, err := loadGlobalNetworkPolicy(); err == nil {
		t.Fatal("loadGlobalNetworkPolicy() accepted a missing file")
	}
}
