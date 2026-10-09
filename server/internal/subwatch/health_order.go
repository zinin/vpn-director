package subwatch

import (
	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/monitor"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// healthOrder requires validated evidence and preserves walk order within groups.
func healthOrder(servers []vpnconfig.Server, evidence monitor.Evidence) []vpnconfig.Server {
	alive := make([]vpnconfig.Server, 0, len(servers))
	other := make([]vpnconfig.Server, 0, len(servers))
	for _, s := range servers {
		switch evidence.Endpoints[endpoint.Key(s)].Status {
		case watchdapi.StatusAlive:
			alive = append(alive, s)
		case watchdapi.StatusRejected:
			continue
		default:
			other = append(other, s)
		}
	}
	return append(alive, other...)
}

func (w *Watch) healthWalkOrder(servers []vpnconfig.Server, cfg *vpnconfig.VPNDirectorConfig) []vpnconfig.Server {
	if len(servers) == 0 || w.Health == nil ||
		(cfg != nil && cfg.Monitor != nil && cfg.Monitor.Enabled != nil && !*cfg.Monitor.Enabled) {
		return servers
	}
	evidence := w.Health.Evidence()
	if evidence.State != watchdapi.StateOK {
		return servers
	}
	keys := make([]string, 0, len(servers))
	seen := make(map[string]bool, len(servers))
	for _, s := range servers {
		key := endpoint.Key(s)
		if _, ok := evidence.Endpoints[key]; ok && !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	// Missing keys stay unknown; an empty validation request means the whole monitor set.
	if len(keys) == 0 || w.Health.ValidateEvidence(evidence, keys) != nil {
		return servers
	}
	return healthOrder(servers, evidence)
}
