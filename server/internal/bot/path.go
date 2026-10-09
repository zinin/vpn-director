package bot

import "github.com/zinin/vpn-director/server/internal/netpath"

const defaultTunnelTablesPath = "/tmp/tunnel_director/tun_dir_tables"
const defaultFailoverReadyPath = "/tmp/tunnel_director/failover_ready"
const defaultTproxyReadyPath = "/tmp/xray_tproxy/ready"
const defaultStoppedPath = "/tmp/vpn-director/stopped"

type Path = netpath.Path

func selectPath(current Path, directLive, currentLive bool, replacement Path) Path {
	if directLive {
		return Path{Kind: netpath.KindDirect}
	}
	if (current.Kind == netpath.KindSOCKS || current.Kind == netpath.KindTunnel) && currentLive {
		return current
	}
	return replacement
}
