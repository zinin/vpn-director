package vpnconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Group is the kind of server s is when a replacement is picked: its
// outbound's protocol and its stream's security, "vless+reality",
// "trojan+tls", "shadowsocks". Servers of one group fare alike on a censored
// path - a panel's Shadowsocks servers can pass a check and carry nothing
// while its REALITY ones work - so the watch tries the group of the user's
// choice first. The transport is no part of it: tcp, xhttp, ws and grpc of one
// protocol and security would split a provider's servers too finely. A legacy
// record without an outbound, and an outbound that does not parse or names no
// protocol, have no group.
func (s Server) Group() string {
	if len(s.Outbound) == 0 {
		return ""
	}
	if _, err := DecodeOutbound(s.Outbound); err != nil {
		return ""
	}
	var ob struct {
		Protocol       string `json:"protocol"`
		StreamSettings struct {
			Security string `json:"security"`
		} `json:"streamSettings"`
	}
	if err := json.Unmarshal(s.Outbound, &ob); err != nil || ob.Protocol == "" {
		return ""
	}
	if ob.StreamSettings.Security == "" || ob.StreamSettings.Security == "none" {
		return ob.Protocol
	}
	return ob.Protocol + "+" + ob.StreamSettings.Security
}

// IdentityMark is a short fingerprint of s's loose identity - its outbound
// without what a panel picks at random for each download - for the selection
// record: the first 16 hex digits of its SHA-256. A record that keeps it tells
// an entry that changed in place - new keys, a new UUID - from one a refresh
// only renamed. "" for a server without an identity.
func IdentityMark(s Server) string {
	id := looseIdentity(s)
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8])
}
