---
paths: "router/opt/vpn-director/lib/platform/keenetic.sh,router/opt/etc/ndm/**/*"
---

# KeeneticOS

Facts behind `lib/platform/keenetic.sh` and the hooks under `router/opt/etc/ndm/`.
Verified on a KN-4521 (KeeneticOS 5.1.5, kernel 4.9-ndm-5, aarch64) unless noted.

## What the firmware has and lacks

| Area | Fact |
|------|------|
| TPROXY | Not in the stock kernel. The firmware component "Kernel modules for Netfilter" (`opkg-kmod-netfilter`) ships `xt_TPROXY.ko`, `xt_socket.ko` and more under `/lib/modules/$(uname -r)/`; installing it rebuilds the firmware and reboots. Nothing autoloads them, there is no `modprobe`; `platform_load_module` does `insmod` when `/proc/modules` does not list the module. |
| ipset, iptables, ip | ipset types are built in; the tools come from Entware: `ipset`, `iptables` 1.4.21, `ip-full` (busybox `ip` cannot address tables above 255). No `/etc/iproute2/rt_tables`: tables are numbers. |
| busybox | No `flock`, `nohup`, `pkill`, `openssl`, `gawk`; its `wget` segfaults on https. `install.sh` installs `flock coreutils-nohup procps-ng-pkill openssl-util gawk …`; `download_file` falls back to curl. |
| Passwords | No `/etc/shadow`. `/opt/etc/passwd` holds Entware root's `$1$` hash; the Web UI login is `root`. |
| Cron | Entware `cron` (Vixie 4.1 "with cron.d addition"): jobs are files in `/opt/etc/cron.d/` (`<schedule> root <cmd>`), the init script `S10cron` runs a process named `cron`. `platform_cron_requirements` names what the user must install before cron runs the job (`opkg install cron` when `S10cron` is missing); the job file is already written and starts running once the package is there. |
| Xray | Entware `xray` (scripts, `S24xray`) + `xray-core`. |
| `/tmp` | tmpfs, no `noexec`: the self-update installer runs from there. |

## NDM and the firewall

- NDM rebuilds `filter`, `mangle` and `nat` from scratch on any configuration or hotspot change and deletes foreign chains and rules. `ip rule`, routing tables and ipsets survive. From 2026-10-25 15:45 to 2026-10-26 12:15 every renewal of the WAN's DHCP lease rebuilt them — 40 of 40, one every 30 minutes with the upstream's 3600 s lease — and so did each OpenVPN client reconnect (2, inactivity timeout) and the LTE modem's ping-check changing state (1). The hook's apply had a client's MARK rule back in `TUN_DIR` 1 to 4 s after the DHCP ACK (second-resolution logs). Until our chains are back the guards route the protected clients (`lib/guard.sh`), and the hook restores the chains from the last apply's snapshot before it queues the apply (`packet-flow.md`, "Left open").
- `mangle PREROUTING`: `_NDM_PREROUTING_MC`, `_NDM_IPSEC_PREROUTING`, `_NDM_VPN_PREROUTING`, `_NDM_PREROUTING_TTL`, `_NDM_HOTSPOT_PREROUTING_MANGL`, then `-m mark --mark 0x0 -j _NDM_DNSRT_PREROUTING_MANGLE` (a test of the whole mark, so our chains must run first: `XRAY_TPROXY` at 1, `TUN_DIR` at 2). A packet that carries any mark skips that jump to the domain-based (DNS) routing. A protected client's direct-marked traffic — its exclusions, everything `XRAY_TPROXY` lets through — reached it while an exclusion was a bare `RETURN`, and no longer follows the domain-based routes; traffic marked for a tunnel never did. On the KN-4521 that jump is the only rule in mangle that tests for a zero mark (`iptables-save -t mangle`, 2026-10-26); other firmware versions may have more.
- `mangle INPUT`: `-p tcp --dport 443 -j _NDM_HTTP_INPUT_TLS_` drops every TLS ClientHello whose SNI is not the router's; TPROXY keeps port 443, so `platform_tproxy_extra_rules apply` puts `-m mark --mark <fwmark> -j ACCEPT` at position 1 of `mangle INPUT`.
- Marks (measured on KeeneticOS 5.1.5): NDM uses `NDMMARK`/`CONNNDMMARK`. Built-in LTE backup stays at prefs **100/101**, fwmark **`0xffffaaa`**, table **4096**. A user connection policy added prefs **102/103**, table **4097**, fwmark **`0xffffaab`**. Pref 200 was free; no tables in the 40s. The claim that connection policies sit at pref 200 with tables 42+ is **not** a fact of 5.1.5. `_tproxy_setup_routing` still only removes rules carrying our mark value or our table, because other firmware versions may still park policies at 200. Our marks (`0x100`, `0x00ff0000`) do not collide. NDM's policy marks also carry bit 9, the direct mark (`0x200`), but NDM's own rules (prefs 100-105 on 2026-10-26) route or blackhole them before the direct rule at 16894. A client under a Keenetic policy must not be in Tunnel Director: NDM's hotspot chain would rewrite its mark.
- Tunnel Director tables: `2000 + idx` (`KEENETIC_TABLE_BASE`), `ip route replace default via <gw> dev ovpn_brN` (OpenVPN; `<gw>` = `tunnel_director.tunnels.<id>.gateway` or the subnet's first host) / `default dev nwgN` (Wireguard), re-installed on every apply.

## Fast path

KeeneticOS binds an established **forwarded** flow to a NAT/route fast path that runs before `mangle` and never returns to it — the conntrack hook sits at priority -200, `mangle` at -150. Once a flow is bound, its packets never reach `TUN_DIR`, so the `MARK` is applied to the first few packets only: before `PPE` (below), the rest missed `ip rule 16384` and left through the WAN carrying the tunnel's source address.

The fast path routes a bound flow by the ip rules alone, with its source, destination and incoming interface — never by its mark. Two experiments on the KN-4521 (2026-10-26) showed it, each on the traffic of one LAN client (on `OpenVPN1`) to one or two test addresses, with `tcpdump` on the upstream router's LAN port counting what went around the tunnel:

- Guards, `from <client> to <addr> iif br0 lookup 2000` and then `prohibit`, while a `RETURN` ahead of the `TUN_DIR` jump imitated a rebuild for 10 s: 73 of 73 new connections in the window went through the VPN, no keep-alive connection broke, a slow download kept its rate (2 MB in 50 s), no packet went around the tunnel. Without them 25 new connections left through the home address, 4 of 4 keep-alive connections opened before the window broke and reconnected through the WAN, the download stalled (457 KB in 50 s), and 3179 packets went around the tunnel, 1591 of them after the window.
- A slow download from `213.180.204.183` (`mirror.yandex.ru`, in the `ru` set, so excluded) with `MARK 0x200` ahead of `TUN_DIR`, `from <client> to <addr> fwmark 0x200/0x200 lookup main` and the guards behind it: without `PPE` it timed out with 0 bytes — the 6 packets of the handshake went direct, then the fast path took the flow and the guard sent it into the tunnel. With `PPE`, 100 KB/s (4 MB in 40 s), the rate it had with nothing added.

`platform_tunnel_offload_target` prints `PPE` on Keenetic (nothing, rc 1 on Merlin). `PPE` sets `ct->fast_ext` (a condition of both the `fastnat` and the `fastroute` entry test) and `FOE_ALG_SKIP`, closing the hardware path too. `tunnel.sh` puts that target at the head of each client's block in `TUN_DIR`, with the `MARK` rule's match and ahead of the exclusions, and `tproxy.sh` right after the direct mark of `XRAY_TPROXY`, which follows rule 1. No flow of a protected client is accelerated, one to its exclusions included: an accelerated connection to an excluded country would follow its guards into the tunnel, or be refused. Every other device keeps its acceleration.

A flow already bound stays bound until its conntrack entry expires. `conntrack-tools` is not installed; a flush would drop every established connection — documented, not fixed. `[FASTNAT]` in `/proc/net/nf_conntrack` prints when `fast_ext == 0` and means *eligible for* acceleration, not *accelerated*. The binding threshold is more than 5 packets each way (that is why `TUN_DIR` counted 6 packets of every stalled flow). A rebuild takes `PPE` away with our chains: a flow opened before they are back can be bound, and keeps the route its guard gave it — a tunnel client's flow to an excluded country stays in the tunnel until it closes.

A flow TPROXY takes needs none of this — it terminates in a local socket, so no forwarded flow is left to accelerate. What `XRAY_TPROXY` lets through is forwarded, and gets `PPE` as above.

## RCI

`_rci_get <path>` is `curl -sf --max-time 5 http://localhost:79/rci/<path>`; a 404 or an empty body is "no answer". `jq` has no regex functions on Keenetic.

| Path | Used for |
|------|----------|
| `show/interface` | tunnel ids (`type` OpenVPN/Wireguard), `remote-endpoint-address`, `wireguard.peer[].remote` (one peer comes as an object) |
| `show/interface/<id>` | `connected` (`yes`/`no`), `address`, `mask`, `description`, `type` |
| `show/system` | `hostname` |
| `show/version` | `model` |

Interface names are a convention RCI does not expose: `OpenVPNN` → `ovpn_brN` (a bridge; the tunnel's `tunN` is enslaved), `WireguardN` → `nwgN`. `platform_tunnel_iface` checks the address while the tunnel is up.

`platform_tunnels` selects by `type` alone, so an OpenVPN or WireGuard *server* the router hosts is listed as a tunnel too, and `platform_tunnel_route` would derive its next hop from the server's own address — a route into the router itself. Which RCI field tells a server from a client is unverified (`tunnel-protocol: tcp-client` and `remote-endpoint-address` are what the client fixture carries); the KN-4521 has no server interface. A known limitation, stated in both READMEs.

## Hooks (`/opt/etc/ndm/*.d/`, run serially by NDM under a 24-second timeout, `/opt/bin/sh` whatever the shebang)

| Directory | Invocation | Our hook |
|-----------|------------|----------|
| `netfilter.d` | `$type` (`iptables`/`ip6tables`), `$table` (`filter`/`nat`/`mangle`), once per table and family | for every IPv4 table: first, waited for, `lib/fwsnap-restore.sh "$table"`, which puts the last apply's rules of that table back from its snapshot (mangle and nat; none for filter) unless the table still holds them; then a detached `vpn-director.sh --wait apply` |
| `wan.d` | `$1` = `start`/`stop`; `$interface`, `$address`, `$mask`, `$gateway` | apply on `start` |
| `iflayerchanged.d` | `$1` = `hook`; `$id`, `$system_name`, `$layer` (`conf`/`link`/`ipv4`/`ipv6`/`ctrl`), `$level` (`running`/`detached`/`disabled`/`pending`) | apply for `$layer = ipv4` of `OpenVPN*`/`Wireguard*`. On 5.1.5 `ifstatechanged.d` also fires (10 invocations per OpenVPN0 down/up, `change=connected` only on the way up); we use `iflayerchanged.d` because it gives one clean `layer=ipv4` event per direction. `ifstatechanged.d` is obsolete since 4.0. |

`--wait` queues an apply behind a running one, so a burst of hook calls converges; `nohup … &` keeps NDM's timeout off the apply. The restore is a few commands under the firmware's `/bin/sh` - no common.sh, no jq, no lock - and it does nothing for a mangle whose PREROUTING still jumps to our chains, so the six mangle calls of one interface cycle leave one jump per chain and interface (`tunnel-director.md`, "State Tracking").
