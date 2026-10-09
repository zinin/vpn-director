package netpath

import (
	"bufio"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

type Kind int

const (
	KindNone Kind = iota
	KindDirect
	KindSOCKS
	KindTunnel
)

const defaultSOCKSPort = 12346
const defaultMarkShift = 16

type Path struct {
	Kind      Kind
	ID        string
	Iface     string
	SOCKSPort int
	Mark      uint32
}

func (p Path) String() string {
	switch p.Kind {
	case KindDirect:
		return "direct"
	case KindSOCKS:
		return "socks"
	case KindTunnel:
		return "tunnel:" + p.ID
	default:
		return "none"
	}
}

func (p Path) Same(q Path) bool {
	return p.Kind == q.Kind && p.ID == q.ID
}

func (p Path) ParamsEqual(q Path) bool {
	return p.Same(q) && p.SOCKSPort == q.SOCKSPort && p.Iface == q.Iface && p.Mark == q.Mark
}

func SOCKSPort(cfg *vpnconfig.VPNDirectorConfig) int {
	_, socks := vpnconfig.XrayInboundPorts(cfg)
	if socks <= 0 {
		return defaultSOCKSPort
	}
	return socks
}

// tunnel.sh excludes overflowing slots before writing the applied tables.
// Fractional shifts are invalid in bash arithmetic and cannot be truncated here.
func markShift(cfg *vpnconfig.VPNDirectorConfig) uint {
	if cfg == nil {
		return defaultMarkShift
	}
	td, ok := cfg.Advanced["tunnel_director"].(map[string]interface{})
	if !ok {
		return defaultMarkShift
	}
	v, ok := td["mark_shift"].(float64)
	if !ok || v < 0 || v > 31 || v != float64(uint(v)) {
		return defaultMarkShift
	}
	return uint(v)
}

func tunnelMark(idx int, shift uint) uint32 {
	slot := idx + 1
	return uint32(slot) << shift
}

func parseTunnelTables(r io.Reader) map[string]int {
	out := map[string]int{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		idx, err := strconv.Atoi(fields[0])
		if err != nil || idx < 0 {
			continue
		}
		out[fields[1]] = idx
	}
	return out
}

func LoadTunnelIndexes(path string) map[string]int {
	f, err := os.Open(path)
	if err != nil {
		return map[string]int{}
	}
	defer f.Close()
	return parseTunnelTables(f)
}

func Candidates(cfg *vpnconfig.VPNDirectorConfig, plat vpnconfig.PlatformInfo, socksUp bool, indexes map[string]int) []Path {
	out := []Path{{Kind: KindDirect}}
	if socksUp {
		out = append(out, Path{Kind: KindSOCKS, SOCKSPort: SOCKSPort(cfg)})
	}
	if cfg == nil {
		return out
	}
	shift := markShift(cfg)
	byID := make(map[string]vpnconfig.PlatformTunnel, len(plat.Tunnels))
	for _, t := range plat.Tunnels {
		byID[t.ID] = t
	}
	for _, id := range vpnconfig.TDExits(cfg, plat) {
		pt := byID[id]
		var mark uint32
		if idx, ok := indexes[id]; ok {
			mark = tunnelMark(idx, shift)
		} else {
			slog.Debug("Tunnel candidate has no applied mark", "tunnel", id)
		}
		out = append(out, Path{Kind: KindTunnel, ID: id, Iface: pt.Iface, Mark: mark})
	}
	return out
}

func TunnelPath(cfg *vpnconfig.VPNDirectorConfig, plat vpnconfig.PlatformInfo, id, tablesPath string) Path {
	var iface string
	for _, t := range plat.Tunnels {
		if t.ID == id {
			iface = t.Iface
			break
		}
	}
	indexes := LoadTunnelIndexes(tablesPath)
	var mark uint32
	if idx, ok := indexes[id]; ok {
		mark = tunnelMark(idx, markShift(cfg))
	}
	return Path{Kind: KindTunnel, ID: id, Iface: iface, Mark: mark}
}
