package vpnconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

type Server struct {
	Address     string   `json:"address"`
	Port        int      `json:"port"`
	UUID        string   `json:"uuid"`
	Name        string   `json:"name"`
	IPs         []string `json:"ips"`
	Security    string   `json:"security,omitempty"`
	Network     string   `json:"network,omitempty"`
	Flow        string   `json:"flow,omitempty"`
	SNI         string   `json:"sni,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	PublicKey   string   `json:"public_key,omitempty"`
	ShortID     string   `json:"short_id,omitempty"`
	ALPN        []string `json:"alpn,omitempty"`
}

// ServerIPs returns every non-empty IP across servers, de-duplicated and
// sorted. xray.servers feeds TPROXY_BYPASS, so every configured server endpoint
// must be present (otherwise the proxy's own egress could be routed back through
// itself). The result is never nil, so an empty list marshals to [] not null.
func ServerIPs(servers []Server) []string {
	seen := make(map[string]bool)
	ips := make([]string, 0)
	for _, s := range servers {
		for _, ip := range s.IPs {
			if ip != "" && !seen[ip] {
				seen[ip] = true
				ips = append(ips, ip)
			}
		}
	}
	sort.Strings(ips)
	return ips
}

type WebUIConfig struct {
	Port      int    `json:"port,omitempty"`
	CertFile  string `json:"cert_file,omitempty"`
	KeyFile   string `json:"key_file,omitempty"`
	JWTSecret string `json:"jwt_secret,omitempty"`
	LogLevel  string `json:"log_level,omitempty"` // debug, info, warn, error; empty means info
}

type VPNDirectorConfig struct {
	DataDir        string                 `json:"data_dir"`
	WebUI          WebUIConfig            `json:"webui,omitempty"`
	PausedClients  []string               `json:"paused_clients,omitempty"`
	TunnelDirector TunnelDirectorConfig   `json:"tunnel_director"`
	Xray           XrayConfig             `json:"xray"`
	Advanced       map[string]interface{} `json:"advanced,omitempty"`
}

type TunnelConfig struct {
	Clients []string `json:"clients"`
	Exclude []string `json:"exclude"`
	// Gateway is the next hop of the tunnel's default route on Keenetic
	// (spec 12): read by lib/platform/keenetic.sh for OpenVPN tunnels,
	// ignored elsewhere. Kept here so a save by a daemon does not drop it.
	Gateway string `json:"gateway,omitempty"`
}

type TunnelDirectorConfig struct {
	// Note: Go's json.Marshal sorts map keys alphabetically, so tunnel order
	// in saved JSON is deterministic. This matches jq 'keys[]' behavior.
	// If insertion-order priority is needed, this should be changed to a slice.
	Tunnels map[string]TunnelConfig `json:"tunnels"`
}

type XrayConfig struct {
	Clients         []string      `json:"clients"`
	Servers         []string      `json:"servers"`
	ExcludeIPs      []string      `json:"exclude_ips"`
	ExcludeSets     []string      `json:"exclude_sets"`
	ActiveServer    *ActiveServer `json:"active_server,omitempty"`
	SubscriptionURL string        `json:"subscription_url,omitempty"`
	Failover        *XrayFailover `json:"failover,omitempty"`
}

// ActiveServer records which server the generated Xray config was built from.
// Nothing else can answer that: config.json holds only the outbound, and on a
// real subscription many entries share one endpoint - the router this was
// written for has 62 servers behind 9 address:port pairs and a single UUID, so
// eight names fit the running outbound equally well. Absent until something
// selects a server, and omitted from the file rather than written as null: a
// config that predates this field must not start claiming a server is active.
type ActiveServer struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Port    int    `json:"port"`
}

// NewActiveServer records the fields of s that identify it to a reader. The
// rest - UUID, REALITY keys - would put credentials in a file the Web UI hands
// out over /api/config.
func NewActiveServer(s Server) *ActiveServer {
	return &ActiveServer{Name: s.Name, Address: s.Address, Port: s.Port}
}

// ClientInfo represents a VPN client with its route and pause status.
type ClientInfo struct {
	IP     string `json:"ip"`
	Route  string `json:"route"`
	Paused bool   `json:"paused"`
}

// PlatformInfo is the document `vpn-director.sh platform` prints (spec 5.3):
// what the daemons know about the router without reading its firmware.
type PlatformInfo struct {
	Platform     string           `json:"platform"`
	Arch         string           `json:"arch"`
	PasswordFile string           `json:"password_file"`
	LANIfaces    []string         `json:"lan_ifaces"`
	WANIf        string           `json:"wan_if"`
	Tunnels      []PlatformTunnel `json:"tunnels"`
}

// PlatformTunnel is one firmware VPN client tunnel, as the platform lists it.
// ID is what tunnel_director.tunnels is keyed by.
type PlatformTunnel struct {
	ID          string `json:"id"`
	Iface       string `json:"iface"`
	Type        string `json:"type"` // openvpn or wireguard
	Connected   bool   `json:"connected"`
	Description string `json:"description"`
}

// HasTunnel reports whether id is a tunnel the platform lists.
func (p PlatformInfo) HasTunnel(id string) bool {
	for _, t := range p.Tunnels {
		if t.ID == id {
			return true
		}
	}
	return false
}

// CollectClients builds a unified list of all clients from xray and tunnel_director sections.
// XrayInboundPorts reads advanced.xray.tproxy_port and advanced.xray.socks_port.
// A missing, non-numeric or non-positive value comes back as 0, which the Xray
// generator reads as "keep the port the template carries".
func XrayInboundPorts(cfg *VPNDirectorConfig) (tproxy, socks int) {
	if cfg == nil {
		return 0, 0
	}
	xray, ok := cfg.Advanced["xray"].(map[string]interface{})
	if !ok {
		return 0, 0
	}
	read := func(key string) int {
		// Numbers decoded from JSON into interface{} arrive as float64.
		v, ok := xray[key].(float64)
		if !ok || v <= 0 {
			return 0
		}
		return int(v)
	}
	return read("tproxy_port"), read("socks_port")
}

// RepointPausedClients rewrites paused entries onto the spelling the clients
// are stored under. lib/config.sh subtracts paused_clients by exact string, so
// a client rewritten from 1.2.3.4/32 to 1.2.3.4 silently resumes unless its
// paused entry follows. Entries matching no client are kept untouched.
func RepointPausedClients(paused []string, clients []ClientInfo) []string {
	stored := make(map[string]string, len(clients))
	for _, c := range clients {
		key := c.IP
		if norm, err := NormalizeClientAddr(c.IP); err == nil {
			key = norm
		}
		if _, seen := stored[key]; !seen {
			stored[key] = c.IP
		}
	}

	out := make([]string, 0, len(paused))
	seen := make(map[string]bool, len(paused))
	for _, entry := range paused {
		key := entry
		if norm, err := NormalizeClientAddr(entry); err == nil {
			key = norm
		}
		if s, ok := stored[key]; ok {
			entry = s
		}
		if seen[entry] {
			continue
		}
		seen[entry] = true
		out = append(out, entry)
	}
	return out
}

func CollectClients(cfg *VPNDirectorConfig) []ClientInfo {
	paused := make(map[string]bool, len(cfg.PausedClients))
	for _, ip := range cfg.PausedClients {
		paused[ip] = true
	}

	var clients []ClientInfo

	for _, ip := range cfg.Xray.Clients {
		clients = append(clients, ClientInfo{
			IP:     ip,
			Route:  "xray",
			Paused: paused[ip],
		})
	}

	// Sort tunnel names for deterministic order
	tunnelNames := make([]string, 0, len(cfg.TunnelDirector.Tunnels))
	for name := range cfg.TunnelDirector.Tunnels {
		tunnelNames = append(tunnelNames, name)
	}
	sort.Strings(tunnelNames)

	for _, name := range tunnelNames {
		tunnel := cfg.TunnelDirector.Tunnels[name]
		for _, ip := range tunnel.Clients {
			clients = append(clients, ClientInfo{
				IP:     ip,
				Route:  name,
				Paused: paused[ip],
			})
		}
	}

	return clients
}

func LoadServers(path string) ([]Server, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var servers []Server
	return servers, json.Unmarshal(data, &servers)
}

func SaveServers(path string, servers []Server) error {
	data, err := json.MarshalIndent(servers, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

func LoadVPNDirectorConfig(path string) (*VPNDirectorConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg VPNDirectorConfig
	return &cfg, json.Unmarshal(data, &cfg)
}

func SaveVPNDirectorConfig(path string, cfg *VPNDirectorConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

// writeFileAtomic writes data to path through a temp file in the same
// directory, fsyncs it, sets 0600 and renames it over path. A reader (the
// shell scripts, the other daemon) therefore never sees a partial file, a
// crash leaves either the old or the new content, and jwt_secret and server
// UUIDs stop being world-readable.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
