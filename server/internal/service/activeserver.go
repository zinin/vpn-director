// internal/service/activeserver.go
package service

import (
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// GenerateAndRecordActiveServer writes config.json and the note of which
// server it was built from, both under the config lock.
//
// One lock, because the two have to agree. With the generation outside it, a
// switch from the other daemon can land between this one's write and its
// record: config.json ends up describing one server while the UI names
// another, and both switches report success. configure.sh already holds this
// same lock across the same pair.
//
// The record is written only once the generation has succeeded — an error from
// the closure skips the save — so a server whose parameters Xray rejects, the
// ordinary fate of an incomplete REALITY entry, never gets named as the
// running one.
//
// generated is what the caller must branch on, not the error. False means
// config.json is untouched and the switch did not happen — the generation was
// rejected, or the config could not even be loaded to reach it — so the caller
// has to report the failure and stop. True with a non-nil error means the
// opposite: config.json was written and only the record of it was not, and
// failing the caller over bookkeeping would send the user back to redo a
// switch that worked.
//
// The caller is expected to have persisted xray.servers already: a save
// failure here leaves a new config.json against a config that already lists
// its address in the bypass set, rather than one that does not.
//
// A selection is the user's new choice, so it also ends whatever the
// subscription walk kept of the old one in preferred_server.
func GenerateAndRecordActiveServer(store ConfigStore, xray XrayGenerator, s vpnconfig.Server, ports InboundPorts) (generated bool, err error) {
	generated, _, err = generateAndRecord(store, xray, s, ports, nil, func(cfg *vpnconfig.VPNDirectorConfig) {
		cfg.Xray.PreferredServer = nil
		cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(cfg.Xray.ActiveServer, s)
	})
	return generated, err
}

// GenerateAndRecordWalkedServer is the subscription walk's switch: config.json
// from generate (the walk has replaced Address with a resolved IPv4), identity
// recorded as active_server so the Web UI badge still matches servers.json, and
// the user's own choice kept beside it while the walk is away from it
// (vpnconfig.RecordWalkedServer).
//
// A non-nil guard runs first, under the same lock, on the config that lock
// protects; its error writes nothing and comes back as is. The watch passes one
// so a selection another daemon committed after the watch last read the config
// is refused rather than written over.
// seq is the counter active_server carries in the file once the call returns,
// taken inside the transaction rather than read back after the lock is gone: a
// selection committed in between would otherwise be adopted as the caller's own
// write. It is meaningful only when generated is true.
func GenerateAndRecordWalkedServer(store ConfigStore, xray XrayGenerator, generate, identity vpnconfig.Server, ports InboundPorts, guard func(*vpnconfig.VPNDirectorConfig) error) (generated bool, seq int, err error) {
	return generateAndRecord(store, xray, generate, ports, guard, func(cfg *vpnconfig.VPNDirectorConfig) {
		vpnconfig.RecordWalkedServer(cfg, identity)
	})
}

// generateAndRecord writes config.json from generate and, once that succeeded,
// has record name it in the config, both under the config lock.
func generateAndRecord(store ConfigStore, xray XrayGenerator, generate vpnconfig.Server, ports InboundPorts, guard func(*vpnconfig.VPNDirectorConfig) error, record func(*vpnconfig.VPNDirectorConfig)) (generated bool, seq int, err error) {
	var prev, written int
	err = store.UpdateVPNConfig(func(cfg *vpnconfig.VPNDirectorConfig) error {
		if guard != nil {
			if err := guard(cfg); err != nil {
				return err
			}
		}
		if err := xray.GenerateConfig(generate, ports); err != nil {
			return err
		}
		generated = true
		// Every record moves the counter one write on from what the config
		// named: a reader that remembers the counter can tell this write
		// happened even when it names the server that was already there.
		prev = vpnconfig.ActiveSeq(cfg.Xray.ActiveServer)
		record(cfg)
		written = vpnconfig.ActiveSeq(cfg.Xray.ActiveServer)
		return nil
	})
	if err != nil {
		// config.json may be written, but the record of it is not in the file.
		return generated, prev, err
	}
	return generated, written, nil
}
