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

- NDM rebuilds `filter`, `mangle` and `nat` from scratch on any configuration or hotspot change and deletes foreign chains and rules. `ip rule`, routing tables and ipsets survive.
- `mangle PREROUTING`: `_NDM_PREROUTING_MC`, `_NDM_IPSEC_PREROUTING`, `_NDM_VPN_PREROUTING`, `_NDM_PREROUTING_TTL`, `_NDM_HOTSPOT_PREROUTING_MANGL`, then `-m mark --mark 0x0 -j _NDM_DNSRT_PREROUTING_MANGLE` (a full-mark guard, so our chains must run first: `XRAY_TPROXY` at 1, `TUN_DIR` at 2).
- `mangle INPUT`: `-p tcp --dport 443 -j _NDM_HTTP_INPUT_TLS_` drops every TLS ClientHello whose SNI is not the router's; TPROXY keeps port 443, so `platform_tproxy_extra_rules apply` puts `-m mark --mark <fwmark> -j ACCEPT` at position 1 of `mangle INPUT`.
- Marks (measured on KeeneticOS 5.1.5): NDM uses `NDMMARK`/`CONNNDMMARK`. Built-in LTE backup stays at prefs **100/101**, fwmark **`0xffffaaa`**, table **4096**. A user connection policy added prefs **102/103**, table **4097**, fwmark **`0xffffaab`**. Pref 200 was free; no tables in the 40s. The claim that connection policies sit at pref 200 with tables 42+ is **not** a fact of 5.1.5. `_tproxy_setup_routing` still only removes rules carrying our mark value or our table, because other firmware versions may still park policies at 200. Our marks (`0x100`, `0x00ff0000`) do not collide. A client under a Keenetic policy must not be in Tunnel Director: NDM's hotspot chain would rewrite its mark.
- Tunnel Director tables: `2000 + idx` (`KEENETIC_TABLE_BASE`), `ip route replace default via <gw> dev ovpn_brN` (OpenVPN; `<gw>` = `tunnel_director.tunnels.<id>.gateway` or the subnet's first host) / `default dev nwgN` (Wireguard), re-installed on every apply.

## Fast path

KeeneticOS binds an established **forwarded** flow to a NAT/route fast path that runs before `mangle` and never returns to it — the conntrack hook sits at priority -200, `mangle` at -150. Once a flow is bound, its packets never reach `TUN_DIR`, so the `MARK` is applied to the first few packets only and the rest miss `ip rule 16384` and leave through the WAN carrying the tunnel's source address.

`platform_tunnel_offload_target` prints `PPE` on Keenetic (nothing, rc 1 on Merlin). `PPE` sets `ct->fast_ext` (a condition of both the `fastnat` and the `fastroute` entry test) and `FOE_ALG_SKIP`, closing the hardware path too. `tunnel.sh` places that target inside `TUN_DIR` with the identical match immediately before `MARK`, after the exclusion `RETURN`s, so excluded destinations keep acceleration.

A flow already bound stays bound until its conntrack entry expires. `conntrack-tools` is not installed; a flush would drop every established connection — documented, not fixed. `[FASTNAT]` in `/proc/net/nf_conntrack` prints when `fast_ext == 0` and means *eligible for* acceleration, not *accelerated*. The binding threshold is more than 5 packets each way (that is why `TUN_DIR` counted 6 packets of every stalled flow).

Xray needs none of this — TPROXY terminates the connection in a local socket, so no forwarded flow is left to accelerate.

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
| `netfilter.d` | `$type` (`iptables`/`ip6tables`), `$table` (`filter`/`nat`/`mangle`), once per table and family | for every IPv4 table: first, waited for, `lib/fwsnap-restore.sh "$table"`, which puts the last apply's rules back from its snapshot unless the table still holds them; then a detached `vpn-director.sh --wait apply` |
| `wan.d` | `$1` = `start`/`stop`; `$interface`, `$address`, `$mask`, `$gateway` | apply on `start` |
| `iflayerchanged.d` | `$1` = `hook`; `$id`, `$system_name`, `$layer` (`conf`/`link`/`ipv4`/`ipv6`/`ctrl`), `$level` (`running`/`detached`/`disabled`/`pending`) | apply for `$layer = ipv4` of `OpenVPN*`/`Wireguard*`. On 5.1.5 `ifstatechanged.d` also fires (10 invocations per OpenVPN0 down/up, `change=connected` only on the way up); we use `iflayerchanged.d` because it gives one clean `layer=ipv4` event per direction. `ifstatechanged.d` is obsolete since 4.0. |

`--wait` queues an apply behind a running one, so a burst of hook calls converges; `nohup … &` keeps NDM's timeout off the apply. The restore is a few commands under the firmware's `/bin/sh` - no common.sh, no jq, no lock - and it does nothing for a mangle whose PREROUTING still jumps to our chains, so the six mangle calls of one interface cycle leave one jump per chain and interface (`tunnel-director.md`, "State Tracking").
