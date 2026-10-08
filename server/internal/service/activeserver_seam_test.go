package service

import (
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// GenerateAndRecordWalkedServer is a test helper over the production path,
// GenerateAndRecordGuardedWalkedServer, which watchd's walk calls: with a
// GuardedXrayGenerator it is that function, otherwise the same transaction over
// a plain XrayGenerator. config.json comes from generate, identity is recorded
// as active_server and the user's own choice kept beside it while the walk is
// away from it (vpnconfig.RecordWalkedServer).
//
// A non-nil guard runs first, under the same lock, on the config that lock
// protects; its error writes nothing and comes back as is. seq is the counter
// active_server carries in the file once the call returns, taken inside the
// transaction; it is meaningful only when generated is true.
func GenerateAndRecordWalkedServer(store ConfigStore, xray XrayGenerator, generate, identity vpnconfig.Server, ports InboundPorts, guard func(*vpnconfig.VPNDirectorConfig) error) (generated bool, seq int, err error) {
	if guarded, ok := xray.(GuardedXrayGenerator); ok {
		return GenerateAndRecordGuardedWalkedServer(store, guarded, generate, identity, ports, guard)
	}
	return generateAndRecord(store, xray, generate, ports, guard, func(cfg *vpnconfig.VPNDirectorConfig) {
		vpnconfig.RecordWalkedServer(cfg, identity)
	})
}
