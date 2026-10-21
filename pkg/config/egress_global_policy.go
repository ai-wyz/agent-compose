package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/chaitin/agent-compose/pkg/compose"
	"github.com/chaitin/agent-compose/pkg/egress"
	"gopkg.in/yaml.v3"
)

// EgressGlobalPolicyFileEnv names an optional YAML file that declares the
// daemon-wide outbound network policy. It has the same body as a compose
// policy.network block: default, allow, and deny.
const EgressGlobalPolicyFileEnv = "EGRESS_GLOBAL_POLICY_FILE"

// loadGlobalNetworkPolicy reads the daemon-wide policy once at startup. An
// unset variable means no daemon policy and preserves the undeclared behavior
// exactly. A configured file is parsed with unknown fields rejected so a typo
// cannot silently broaden the policy, then goes through the same compose
// normalization every agent declaration uses.
func loadGlobalNetworkPolicy() (*egress.NetworkDeclaration, error) {
	path := strings.TrimSpace(os.Getenv(EgressGlobalPolicyFileEnv))
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s %q: %w", EgressGlobalPolicyFileEnv, path, err)
	}
	var spec compose.SandboxNetworkSpec
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&spec); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s %q is empty", EgressGlobalPolicyFileEnv, path)
		}
		return nil, fmt.Errorf("parse %s %q: %w", EgressGlobalPolicyFileEnv, path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("%s %q must contain exactly one YAML document", EgressGlobalPolicyFileEnv, path)
		}
		return nil, fmt.Errorf("parse %s %q: %w", EgressGlobalPolicyFileEnv, path, err)
	}
	normalized, err := compose.NormalizeSandboxNetworkSpec("daemon network policy", &spec)
	if err != nil {
		return nil, fmt.Errorf("validate %s %q: %w", EgressGlobalPolicyFileEnv, path, err)
	}
	declaration, err := normalized.EgressDeclaration()
	if err != nil {
		return nil, fmt.Errorf("compile %s %q: %w", EgressGlobalPolicyFileEnv, path, err)
	}
	return &declaration, nil
}
