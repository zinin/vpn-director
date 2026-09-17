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
func GenerateAndRecordActiveServer(store ConfigStore, xray XrayGenerator, s vpnconfig.Server, ports InboundPorts) (generated bool, err error) {
	return GenerateAndRecordDialedServer(store, xray, s, s, ports, nil)
}

// GenerateAndRecordDialedServer writes config.json from generate (the watch
// walk may have replaced Address with a resolved IPv4) and records identity
// as active_server so the Web UI badge still matches servers.json.
//
// A non-nil guard runs first, under the same lock, on the config that lock
// protects; its error writes nothing and comes back as is. The watch passes one
// so a selection another daemon committed after the watch last read the config
// is refused rather than written over.
func GenerateAndRecordDialedServer(store ConfigStore, xray XrayGenerator, generate, identity vpnconfig.Server, ports InboundPorts, guard func(*vpnconfig.VPNDirectorConfig) error) (generated bool, err error) {
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
		// One write on from what the config named: a reader that remembers the
		// counter can tell this write happened even when it names the server
		// that was already there.
		cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(cfg.Xray.ActiveServer, identity)
		return nil
	})
	return generated, err
}
