package egress

import "testing"

func TestCIDRPolicyMatchesResolvedAddresses(t *testing.T) {
	policy := NewPolicy(Deny, Rule{
		ID:     "network.allow",
		Match:  MatchEndpoint,
		Names:  []string{FormatEndpointPattern("10.0.0.0/8", 443, ProtocolAny)},
		Action: Allow,
	})
	tests := []struct {
		name        string
		request     Request
		wantAllowed bool
	}{
		{
			name:        "name resolving inside the range",
			request:     Request{Name: "service.example:443/https", ResolvedIPs: []string{"10.2.16.21"}},
			wantAllowed: true,
		},
		{
			name:        "name resolving outside the range",
			request:     Request{Name: "service.example:443/https", ResolvedIPs: []string{"203.0.113.7"}},
			wantAllowed: false,
		},
		{
			name:        "direct address inside the range",
			request:     Request{Name: "10.2.16.21:443/https"},
			wantAllowed: true,
		},
		{
			name:        "one allowed and one disallowed answer",
			request:     Request{Name: "service.example:443/https", ResolvedIPs: []string{"10.2.16.21", "203.0.113.7"}},
			wantAllowed: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Decide(policy, tt.request).Allowed(); got != tt.wantAllowed {
				t.Fatalf("Decide() allowed = %v, want %v", got, tt.wantAllowed)
			}
		})
	}
}

func TestIPv6PolicyMatchesDirectAndResolvedAddresses(t *testing.T) {
	policy := NewPolicy(Deny, Rule{
		ID:     "network.allow",
		Match:  MatchEndpoint,
		Names:  []string{FormatEndpointPattern("2001:db8::/32", 443, ProtocolAny)},
		Action: Allow,
	})
	for _, request := range []Request{
		{Name: "[2001:db8::1]:443/https"},
		{Name: "service.example:443/https", ResolvedIPs: []string{"2001:db8::1"}},
	} {
		if !Decide(policy, request).Allowed() {
			t.Fatalf("Decide(%+v) denied an IPv6 destination inside the range", request)
		}
	}
	if Decide(policy, Request{Name: "[2001:db9::1]:443/https"}).Allowed() {
		t.Fatal("Decide() allowed an IPv6 destination outside the range")
	}
}
