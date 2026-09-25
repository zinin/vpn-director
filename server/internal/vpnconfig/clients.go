package vpnconfig

// MoveResult says what MoveClient did.
type MoveResult int

const (
	// ClientMoved means the address now sits on the target route alone.
	ClientMoved MoveResult = iota
	// ClientAlreadyThere means the target route was already the only route
	// holding the address; nothing changed.
	ClientAlreadyThere
	// ClientNotFound means no route holds the address; nothing changed.
	ClientNotFound
)

// ClientRoutes lists the routes that hold addr, in any stored spelling, in the
// order CollectClients reports them: "xray" first, then the tunnels by name.
// An address normally sits on one route; during a staged failover it sits on
// Xray and on the fallback tunnel at once.
func ClientRoutes(cfg *VPNDirectorConfig, addr string) []string {
	key := addrKey(addr)
	var routes []string
	for _, c := range CollectClients(cfg) {
		if addrKey(c.IP) == key && !contains(routes, c.Route) {
			routes = append(routes, c.Route)
		}
	}
	return routes
}

// MoveClient puts addr on route - "xray" or a tunnel id the caller has
// checked - and takes it off every other route, in one change of cfg. The
// caller writes the change under the config lock and applies once, and that
// apply moves the client make-before-break: lib/tproxy.sh keeps a client that
// leaves Xray intercepted until lib/tunnel.sh has taken it, so none of its
// packets leaves through the WAN in between. A delete and an add left it there
// for as long as the user took between the two.
//
// The address goes on in its normalized spelling. A tunnel new to the config
// inherits xray.exclude_sets, as an added client's does: an empty exclude
// would carry the client's local-country traffic through the tunnel too.
// paused_clients follows the new spelling, since the shell subtracts it by
// exact string: a paused client moves paused. The address leaves the failover
// record, so a restore cannot undo the user's choice; one moved to Xray while
// Xray is down joins the failover afresh, as an added one does. A tunnel whose
// last client leaves stays in the config with an empty list, as after a
// delete. Only the routes that hold the address are rewritten.
func MoveClient(cfg *VPNDirectorConfig, addr, route string) MoveResult {
	on := ClientRoutes(cfg, addr)
	switch {
	case len(on) == 0:
		return ClientNotFound
	case len(on) == 1 && on[0] == route:
		return ClientAlreadyThere
	}

	for _, r := range on {
		if r == "xray" {
			cfg.Xray.Clients = withoutAddr(cfg.Xray.Clients, addr)
			continue
		}
		t := cfg.TunnelDirector.Tunnels[r]
		t.Clients = withoutAddr(t.Clients, addr)
		cfg.TunnelDirector.Tunnels[r] = t
	}

	key := addrKey(addr)
	if route == "xray" {
		cfg.Xray.Clients = append(cfg.Xray.Clients, key)
	} else {
		if cfg.TunnelDirector.Tunnels == nil {
			cfg.TunnelDirector.Tunnels = make(map[string]TunnelConfig)
		}
		t, ok := cfg.TunnelDirector.Tunnels[route]
		if !ok {
			t = TunnelConfig{Clients: []string{}, Exclude: append([]string{}, cfg.Xray.ExcludeSets...)}
		}
		t.Clients = append(t.Clients, key)
		cfg.TunnelDirector.Tunnels[route] = t
	}
	cfg.PausedClients = RepointPausedClients(cfg.PausedClients, CollectClients(cfg))
	DetachFailoverClient(cfg, addr)
	return ClientMoved
}
