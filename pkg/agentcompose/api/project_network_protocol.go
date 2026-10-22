package api

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	agentcomposev2 "github.com/chaitin/agent-compose/proto/agentcompose/v2"
)

// legacyNetworkProtocolField is the field number the removed network entry
// protocol used in SandboxNetworkAllowSpec and SandboxNetworkDenySpec. The
// generated types reserve it, so a client built against the old contract still
// sends it but the decoder can only keep it in unknown fields.
const legacyNetworkProtocolField = 3

// LegacyNetworkProtocolIssues reports network entries that still carry the
// removed entry protocol. The generated types reserve the field, so decoding
// drops it and the daemon would apply an entry wider than the client declared
// without any error. The field's default, "any", is accepted because it leaves the
// entry unchanged; that is also what keeps a client that never chose a protocol
// working during an upgrade.
//
// The issue paths use the YAML block name, "policy", so they match the path the
// compose file and canonical spec errors use for the same field.
func LegacyNetworkProtocolIssues(agents []*agentcomposev2.AgentSpec) []*agentcomposev2.ProjectValidationIssue {
	var issues []*agentcomposev2.ProjectValidationIssue
	for index, agent := range agents {
		network := agent.GetSandbox().GetNetwork()
		if network == nil {
			continue
		}
		for entryIndex, entry := range network.GetAllow() {
			if protocol, ok := unknownEntryProtocol(entry); ok && !legacyProtocolIsAny(protocol) {
				issues = append(issues, legacyProtocolIssue(
					fmt.Sprintf("agents[%d].policy.network.allow[%d].protocol", index, entryIndex), protocol))
			}
		}
		for entryIndex, entry := range network.GetDeny() {
			if protocol, ok := unknownEntryProtocol(entry); ok && !legacyProtocolIsAny(protocol) {
				issues = append(issues, legacyProtocolIssue(
					fmt.Sprintf("agents[%d].policy.network.deny[%d].protocol", index, entryIndex), protocol))
			}
		}
	}
	return issues
}

func legacyProtocolIssue(path, protocol string) *agentcomposev2.ProjectValidationIssue {
	return ProjectValidationIssue(path, fmt.Sprintf(
		"the client sent the removed network entry protocol %q; network entries match any protocol now, so upgrade the client or remove the field", protocol))
}

// legacyProtocolIsAny matches the value the removed field defaulted to. A client
// built before the removal always sends the field, so rejecting the default would
// break every upgrade instead of only the entries whose meaning changed.
func legacyProtocolIsAny(protocol string) bool {
	return strings.EqualFold(strings.TrimSpace(protocol), "any")
}

// unknownEntryProtocol reads the removed field from a message's unknown fields.
// It returns false when the field is absent or cannot be read, which leaves the
// entry to the normal validation path.
func unknownEntryProtocol(message proto.Message) (string, bool) {
	raw := message.ProtoReflect().GetUnknown()
	for len(raw) > 0 {
		number, wireType, tagLength := protowire.ConsumeTag(raw)
		if tagLength < 0 {
			return "", false
		}
		raw = raw[tagLength:]
		if number == legacyNetworkProtocolField && wireType == protowire.BytesType {
			value, valueLength := protowire.ConsumeBytes(raw)
			if valueLength < 0 {
				return "", false
			}
			return string(value), true
		}
		fieldLength := protowire.ConsumeFieldValue(number, wireType, raw)
		if fieldLength < 0 {
			return "", false
		}
		raw = raw[fieldLength:]
	}
	return "", false
}
