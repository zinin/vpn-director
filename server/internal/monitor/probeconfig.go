package monitor

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// account is the SOCKS login of one endpoint: its traffic goes to its
// outbound alone.
type account struct {
	User string
	Pass string
}

// newAccounts gives each of n endpoints an account e<i> with a random
// password, new at every start, so no local process can use the prober as a
// proxy to every server.
func newAccounts(n int) ([]account, error) {
	accounts := make([]account, n)
	for i := range accounts {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		accounts[i] = account{User: fmt.Sprintf("e%d", i), Pass: hex.EncodeToString(b)}
	}
	return accounts, nil
}

// probeConfig is the prober's Xray config. The first outbound is a blackhole:
// Xray sends whatever matches no rule to its first outbound, so nothing leaves
// the router directly. One outbound per endpoint follows, tagged m<i>. One
// SOCKS inbound on 127.0.0.1:port takes a password and holds accounts[i] for
// eps[i], and a rule sends each account's traffic to its outbound. Xray logs
// to stderr, which the daemon reads; the live Xray's log stays untouched.
func probeConfig(eps []Endpoint, accounts []account, port int) ([]byte, error) {
	outbounds := []interface{}{map[string]interface{}{"tag": "block", "protocol": "blackhole"}}
	users := make([]interface{}, 0, len(eps))
	rules := make([]interface{}, 0, len(eps))
	for i, ep := range eps {
		ob, err := vpnconfig.DecodeOutbound(ep.Outbound)
		if err != nil {
			return nil, fmt.Errorf("outbound of %s: %w", ep.Key, err)
		}
		tag := fmt.Sprintf("m%d", i)
		ob["tag"] = tag
		outbounds = append(outbounds, ob)
		users = append(users, map[string]interface{}{"user": accounts[i].User, "pass": accounts[i].Pass})
		rules = append(rules, map[string]interface{}{"type": "field", "user": []string{accounts[i].User}, "outboundTag": tag})
	}
	return json.MarshalIndent(map[string]interface{}{
		"log": map[string]interface{}{"loglevel": "warning", "access": "none"},
		"inbounds": []interface{}{map[string]interface{}{
			"tag": "probe-in", "listen": "127.0.0.1", "port": port, "protocol": "socks",
			"settings": map[string]interface{}{"auth": "password", "udp": false, "accounts": users},
		}},
		"outbounds": outbounds,
		"routing":   map[string]interface{}{"domainStrategy": "AsIs", "rules": rules},
	}, "", "  ")
}
