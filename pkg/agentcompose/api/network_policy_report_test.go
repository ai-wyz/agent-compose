package api

import (
	"testing"

	driverpkg "github.com/chaitin/agent-compose/pkg/driver"
	"github.com/chaitin/agent-compose/pkg/egress"
)

// TestNetworkPolicyReportsToProtoCarriesTheDeclaredAccount pins that the
// transport does not lose the parts a consumer decides with: the address ranges
// a declaration constrained, the mechanism, and the warning that says the
// declaration will not be enforced as written.
func TestNetworkPolicyReportsToProtoCarriesTheDeclaredAccount(t *testing.T) {
	reports := []driverpkg.AgentNetworkPolicyReport{{
		Agent:     "worker",
		Driver:    "docker",
		Declared:  true,
		Restricts: true,
		Default:   egress.Deny,
		Allow: []egress.DeclaredAllowance{{
			Endpoint:    "api.github.com:443/https",
			Inspectable: true,
			AllowedIPs:  []string{"203.0.113.0/24"},
		}},
		Deny: []egress.DeclaredDenial{{
			Endpoint:    "metadata.example.com:80/tcp",
			Inspectable: false,
		}},
		Mediated:    true,
		Enforced:    true,
		Enforcement: driverpkg.SandboxEgressForwardProxyEnforcement("docker"),
		Warning:     "",
	}}

	mapped := NetworkPolicyReportsToProto(reports)
	if len(mapped) != 1 {
		t.Fatalf("mapped = %+v, want one report", mapped)
	}
	report := mapped[0]
	if report.GetAgent() != "worker" || report.GetDriver() != "docker" || !report.GetRestricts() || !report.GetMediated() || !report.GetEnforced() {
		t.Fatalf("mapped report = %+v, want the declared, mediated default-deny account", report)
	}
	if report.GetDefault() != string(egress.Deny) || report.GetMechanism() != "forward_proxy" || report.GetStrength() != "allowlist" {
		t.Fatalf("mapped report = %+v, want the default action and the proxy mechanism", report)
	}
	allow := report.GetAllow()
	if len(allow) != 1 || allow[0].GetEndpoint() != "api.github.com:443/https" || !allow[0].GetInspectable() {
		t.Fatalf("allow = %+v, want the inspectable https allowance", allow)
	}
	if got := allow[0].GetAllowedIps(); len(got) != 1 || got[0] != "203.0.113.0/24" {
		t.Fatalf("allowed_ips = %v, want the declared range", got)
	}
	deny := report.GetDeny()
	if len(deny) != 1 || deny[0].GetEndpoint() != "metadata.example.com:80/tcp" || deny[0].GetInspectable() {
		t.Fatalf("deny = %+v, want the opaque declared denial", deny)
	}
}

// TestNetworkPolicyReportsToProtoKeepsTheGapVisible pins that a report with
// nothing to enforce still travels: an empty report list must not be confused
// with "the engine enforces nothing here".
func TestNetworkPolicyReportsToProtoKeepsTheGapVisible(t *testing.T) {
	report := driverpkg.AgentNetworkPolicyReport{
		Agent:     "worker",
		Driver:    "k8s",
		Declared:  true,
		Restricts: true,
		Default:   egress.Deny,
		Unapplied: []string{"the declared allowances"},
		Warning:   "the engine will not enforce this declaration on driver \"k8s\"",
	}
	mapped := NetworkPolicyReportsToProto([]driverpkg.AgentNetworkPolicyReport{report})
	if len(mapped) != 1 || mapped[0].GetEnforced() {
		t.Fatalf("mapped = %+v, want the unenforced account", mapped)
	}
	if len(mapped[0].GetUnapplied()) != 1 || mapped[0].GetWarning() == "" {
		t.Fatalf("mapped report = %+v, want the unapplied parts and the warning", mapped[0])
	}
	if got := NetworkPolicyReportsToProto(nil); got != nil {
		t.Fatalf("NetworkPolicyReportsToProto(nil) = %+v, want nil so an absent field stays absent", got)
	}
}
