//go:build linux && cgo && microsandboxcgo

package driver

import (
	microsandbox "github.com/superradcompany/microsandbox/sdk/go"
)

// microsandboxSDKNetworkConfig binds a pure microsandboxNetworkPlan to the
// Microsandbox Go SDK. It lives behind the same build tag as the runtime so the
// untagged mapping does not pull the SDK into the docker-only macOS build.
//
// Ingress stays allowed, matching the previous NetworkPolicy.AllowAll()
// behavior for inbound traffic: this change only tightens egress and applies
// the daemon-level DNS rebind protection setting.
func microsandboxSDKNetworkConfig(policy *SandboxNetworkPolicy, rebindProtection bool) (*microsandbox.NetworkConfig, error) {
	plan, err := planMicrosandboxNetwork(policy, rebindProtection)
	if err != nil {
		return nil, err
	}
	rebindProtectionValue := plan.RebindProtection
	config := &microsandbox.NetworkConfig{
		DefaultEgress:  microsandbox.PolicyAction(plan.DefaultEgress),
		DefaultIngress: microsandbox.PolicyActionAllow,
		DNS:            &microsandbox.DNSConfig{RebindProtection: &rebindProtectionValue},
		DenyDomains:    append([]string(nil), plan.DenyDomains...),
	}
	for _, rule := range plan.Rules {
		config.Rules = append(config.Rules, microsandbox.PolicyRule{
			Action:      microsandbox.PolicyAction(rule.Action),
			Direction:   microsandbox.PolicyDirectionEgress,
			Destination: rule.Destination,
			Port:        rule.Port,
			Protocol:    microsandbox.PolicyProtocol(rule.Protocol),
		})
	}
	return config, nil
}
