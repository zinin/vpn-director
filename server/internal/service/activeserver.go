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

// GuardedXrayGenerator checks permission immediately before the live rename.
type GuardedXrayGenerator interface {
	GenerateConfigGuarded(vpnconfig.Server, InboundPorts, func() error) error
}

// GenerateAndRecordGuardedWalkedServer keeps both guard checks under the config lock.
func GenerateAndRecordGuardedWalkedServer(store ConfigStore, xray GuardedXrayGenerator, generate, identity vpnconfig.Server, ports InboundPorts, guard func(*vpnconfig.VPNDirectorConfig) error) (generated bool, seq int, err error) {
	return recordGenerated(store, func(cfg *vpnconfig.VPNDirectorConfig) error {
		check := func() error {
			if guard == nil {
				return nil
			}
			return guard(cfg)
		}
		if err := check(); err != nil {
			return err
		}
		return xray.GenerateConfigGuarded(generate, ports, check)
	}, func(cfg *vpnconfig.VPNDirectorConfig) {
		vpnconfig.RecordWalkedServer(cfg, identity)
	})
}

func generateAndRecord(store ConfigStore, xray XrayGenerator, generate vpnconfig.Server, ports InboundPorts, guard func(*vpnconfig.VPNDirectorConfig) error, record func(*vpnconfig.VPNDirectorConfig)) (generated bool, seq int, err error) {
	return recordGenerated(store, func(cfg *vpnconfig.VPNDirectorConfig) error {
		if guard != nil {
			if err := guard(cfg); err != nil {
				return err
			}
		}
		return xray.GenerateConfig(generate, ports)
	}, record)
}

// recordGenerated publishes and records one server in the same transaction.
func recordGenerated(store ConfigStore, generate func(*vpnconfig.VPNDirectorConfig) error, record func(*vpnconfig.VPNDirectorConfig)) (generated bool, seq int, err error) {
	var prev, written int
	err = store.UpdateVPNConfig(func(cfg *vpnconfig.VPNDirectorConfig) error {
		if err := generate(cfg); err != nil {
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
