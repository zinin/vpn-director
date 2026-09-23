package vpnconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// DecodeOutbound parses a stored outbound. Numbers stay json.Number, so a
// value the subscription wrote goes back out exactly as it came in.
func DecodeOutbound(raw json.RawMessage) (map[string]interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var ob map[string]interface{}
	if err := dec.Decode(&ob); err != nil {
		return nil, err
	}
	if ob == nil {
		return nil, errors.New("outbound is not an object")
	}
	return ob, nil
}

// OutboundTarget returns the object that holds an outbound's address and
// port: settings.vnext[0] for vless and vmess, settings.servers[0] for trojan
// and shadowsocks, and settings itself for the flat form those four also
// accept, and for hysteria, which has no other. Nil when there is no such
// object.
func OutboundTarget(ob map[string]interface{}) map[string]interface{} {
	settings, _ := ob["settings"].(map[string]interface{})
	if settings == nil {
		return nil
	}
	key := ""
	switch ob["protocol"] {
	case "vless", "vmess":
		key = "vnext"
	case "trojan", "shadowsocks":
		key = "servers"
	}
	if list, ok := settings[key].([]interface{}); ok && len(list) > 0 {
		first, _ := list[0].(map[string]interface{})
		return first
	}
	return settings
}

// Protocol is what a server's outbound dials with: the stored outbound's own
// protocol, "vless" for a legacy record without one - the only protocol the
// legacy builders generate - and "" for an outbound that cannot be read or
// names none.
func (s Server) Protocol() string {
	if len(s.Outbound) == 0 {
		return "vless"
	}
	ob, err := DecodeOutbound(s.Outbound)
	if err != nil {
		return ""
	}
	protocol, _ := ob["protocol"].(string)
	return protocol
}

// Label names a server's protocol for a list: "vless·reality",
// "vless·ws·tls", "trojan·tls", "ss", "hysteria2". A record without an
// outbound is a legacy VLESS one, which the generators build as TLS when it
// names no security. An outbound that is there but cannot be read is "?" -
// including a null one, which is not the legacy record it looks like: the
// generators reject it through DecodeOutbound, so the list says so too. An
// outbound that names no protocol is "?" as well: there is nothing to label,
// and "·ws·tls" is no label. protocol_label in configure.sh answers the same
// three ways.
func (s Server) Label() string {
	if len(s.Outbound) == 0 {
		security := s.Security
		if security == "" {
			security = "tls"
		}
		return protocolLabel("vless", s.Network, security)
	}
	if _, err := DecodeOutbound(s.Outbound); err != nil {
		return "?"
	}
	var ob struct {
		Protocol       string `json:"protocol"`
		StreamSettings struct {
			Network  string `json:"network"`
			Security string `json:"security"`
		} `json:"streamSettings"`
	}
	if err := json.Unmarshal(s.Outbound, &ob); err != nil {
		return "?"
	}
	return protocolLabel(ob.Protocol, ob.StreamSettings.Network, ob.StreamSettings.Security)
}

func protocolLabel(protocol, network, security string) string {
	switch protocol {
	case "":
		return "?"
	case "shadowsocks":
		return "ss"
	case "hysteria":
		return "hysteria2"
	}
	parts := []string{protocol}
	if network != "" && network != "tcp" && network != "raw" {
		parts = append(parts, network)
	}
	if security != "" && security != "none" {
		parts = append(parts, security)
	}
	return strings.Join(parts, "·")
}
