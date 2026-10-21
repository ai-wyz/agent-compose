package app

import (
	"fmt"
	"sort"

	"github.com/chaitin/agent-compose/pkg/compose"
	appconfig "github.com/chaitin/agent-compose/pkg/config"
	driverpkg "github.com/chaitin/agent-compose/pkg/driver"
	"github.com/chaitin/agent-compose/pkg/egress"
)

// sandboxEgressMediation reports whether the engine's forward proxy will be in a
// sandbox's traffic path. It is satisfied by the adapters' egress proxy, which
// answers with the same address resolution the creation seam uses, so an
// admission report cannot claim mediation the seam would not install.
type sandboxEgressMediation interface {
	MediatesDriver(driver string) bool
}

// agentNetworkPolicyReports accounts for every agent in a normalized project
// that declared an outbound network policy. It runs at admission, so an operator
// learns which mechanism will apply the declaration before any sandbox exists;
// an agent that declared nothing is omitted because undeclared is not deny.
//
// A declaration the engine cannot translate is reported as an error rather than
// skipped: the project would not compile a policy for it either, so silently
// dropping it from the report would describe a project the engine does not have.
func agentNetworkPolicyReports(spec *compose.NormalizedProjectSpec, mediation sandboxEgressMediation, config *appconfig.Config) ([]driverpkg.AgentNetworkPolicyReport, error) {
	if spec == nil {
		return nil, nil
	}
	mode := appconfig.EgressUnenforceableWarn
	var global *egress.NetworkDeclaration
	if config != nil {
		mode = config.EgressUnenforceablePolicy
		global = config.GlobalNetworkPolicy
	}
	reports := make([]driverpkg.AgentNetworkPolicyReport, 0, len(spec.Agents))
	for _, agent := range spec.Agents {
		var declaration *egress.NetworkDeclaration
		if policy := agent.PolicyBlock(); policy != nil && policy.Network != nil {
			compiled, err := policy.Network.EgressDeclaration()
			if err != nil {
				return nil, fmt.Errorf("agent %q: %w", agent.Name, err)
			}
			declaration = &compiled
		}
		if declaration == nil && global == nil {
			continue
		}
		effective, err := egress.LayerNetworkDeclarations(global, declaration)
		if err != nil {
			return nil, fmt.Errorf("agent %q: %w", agent.Name, err)
		}
		driver := driverForAgent(agent)
		mediated := mediation != nil && mediation.MediatesDriver(driver)
		report, err := driverpkg.ReportAgentNetworkPolicy(agent.Name, driver, effective, mediated, mode)
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	sort.SliceStable(reports, func(i, j int) bool { return reports[i].Agent < reports[j].Agent })
	return reports, nil
}

// driverForAgent resolves the runtime driver an agent's sandboxes use. The
// normalized driver name is authoritative: normalization always names one, and
// defaults an omitted block to docker.
func driverForAgent(agent compose.NormalizedAgentSpec) string {
	if agent.Driver == nil || agent.Driver.Name == "" {
		return driverpkg.RuntimeDriverDocker
	}
	return agent.Driver.Name
}
