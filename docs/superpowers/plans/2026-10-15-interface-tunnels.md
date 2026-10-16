# Interface Tunnels and the Tunnel Kill Switch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Tunnel Director routes LAN clients through any Linux interface the configuration lists (first: the AmneziaWG `awg1` on the owner's RT-AX86U), refuses a tunnel's traffic whenever the tunnel's table has no route instead of letting it leak to the WAN, and takes IPv4 addresses and CIDRs in a tunnel's `exclude`.

**Architecture:** A new library, `lib/iftunnel.sh`, owns interface tunnels - the checked list, table `2000 + idx`, the default route, NAT and MSS rules, a state file - and holds the `vpd_*` layer through which the core reaches every tunnel: a listed interface goes to `iftunnel_*`, any other id to the unchanged platform contract (Tasks 1-3). `lib/tunnel.sh` routes through the layer (Task 4), adds a `prohibit` ip rule per slot right after the lookup range - the kill switch (Task 5) - gives a tunnel the platform does not know a slot with that rule (Task 6) and returns on address exclusions (Task 7). `vpn-director.sh` gets `ensure-routes` and the cron job that runs it, and Asuswrt-Merlin a `nat-start` hook (Task 8). The Go config keeps the new list and the bot and Web UI stop promising the WAN (Task 9). Documentation follows (Task 10), then whole-branch verification (Task 11) and the owner's acceptance on the router (Task 12).

**Tech Stack:** Bash 5 (on the router Entware's bash, BusyBox 1.25 tools, iproute2, iptables), Bats 1.13 with bats-support and bats-assert; Go 1.25 (module `github.com/zinin/vpn-director/server`), standard `testing`; Vue 3 + TypeScript (`web/`).

**Spec:** `docs/superpowers/specs/2026-10-15-interface-tunnels-design.md`

Out of scope: everything the spec lists in section 13, and its section 12 ("To do in wireguard-network"), which the owner does in that repository.

## Global Constraints

- The platform contract does not change: `router/opt/vpn-director/lib/platform.sh`, `lib/platform/merlin.sh`, `lib/platform/keenetic.sh` and their tests are not edited. `lib/tunnel.sh`, `cmd_platform` and `configure.sh` reach tunnels only through the `vpd_*` functions of `lib/iftunnel.sh`.
- `IFTUNNEL_TABLE_BASE=2000`: an interface tunnel's routing table is `2000 + idx`.
- An interface name is kept when it is 1-15 characters of `A-Za-z0-9_.-`, is not `.` or `..`, is not `main`, is not an id `platform_tunnels` lists, is not a `platform_lan_ifaces` name and is not `platform_wan_if` (that last check is skipped when the platform cannot answer). Each rejected name logs exactly one WARN: `tunnel_director.interfaces: <reason>; ignored`.
- Block rule: preference `TUN_DIR_PREF_BASE + _tunnel_mark_field_max + idx`, selector `fwmark <mark>/<mask>`, action `prohibit`, for every slot except `main`'s. With the defaults the lookups take 16384-16638 and the blocks 16639-16893. When `TUN_DIR_PREF_BASE + 2 * _tunnel_mark_field_max > 32766`, `tunnel_apply` logs an ERROR and returns 1 before it touches a rule.
- NAT and MSS rules, exactly as `iptables -S` prints them, each inserted at position 1:
  - `nat POSTROUTING`: `-o <iface> -j MASQUERADE`
  - `mangle FORWARD`: `-o <iface> -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu`
  - `mangle FORWARD`: `-i <iface> -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss <MTU - 40>`
- State: `IFTUNNEL_STATE_FILE`, default `/tmp/tunnel_director/iftunnels`, one `<iface> <table>` line per interface tunnel with a slot; `IFTUNNEL_CRON_MARKER`, default `/tmp/tunnel_director/routes_cron`. Both are overridable from the environment.
- Cron job: name `vpn_director_routes`, schedule `* * * * *`, command `$SCRIPT_DIR/vpn-director.sh ensure-routes`.
- Hash: with `TUN_DIR_INTERFACES` empty the hash input is exactly what it is today; otherwise the line `interfaces: $TUN_DIR_INTERFACES` follows `TUN_DIR_TUNNELS_JSON`.
- Log texts, exactly:
  - WARN `Tunnel '<t>' is not on the platform's list: <reason>; its clients are refused`
  - WARN `Tunnel '<t>' is not a tunnel this platform knows; its clients are refused`
  - WARN `Tunnel '<t>': route not installed: <reason>; its clients are refused`
  - WARN `Tunnel '<t>': route not installed (interface down or not mapped?); its clients are refused until it is`
  - ERROR `Tunnel '<t>': the block rule did not go in (pref=<p> fwmark=<m>); while its table has no route its clients fall through to main`
  - ERROR `Tunnel Director: the block rules would reach main's rule (32766) with pref_base <b> and <F> slots; rules not applied`
  - WARN `Tunnel '<t>': exclusion '<x>' is IPv6; Tunnel Director routes IPv4 only, skipping it`
  - INFO `Interface tunnel back without its route: <ifaces>; applying Tunnel Director`
  - INFO `Scheduled the route check of the interface tunnels (every minute)`
  - WARN `Could not schedule the route check of the interface tunnels; an interface re-created by its own scripts keeps its clients refused until the next apply`
  - INFO `Dropped the route check of the interface tunnels`
  - Unchanged for a tunnel the platform does not list and that has no clients: `Tunnel '<t>' is skipped: <reason>` and `Tunnel '<t>' is not a tunnel this platform knows; skipping`.
- Bot texts (`moveQuestion`), exactly:
  - `%s is down: until it is up, Tunnel Director blocks the traffic %s would send through it (excluded destinations still go direct). Move anyway?`
  - `%s is not on the router's tunnel list: until it is, Tunnel Director blocks the traffic %s would send through it (excluded destinations still go direct). Move anyway?`
- Web UI texts (`ClientsTab.vue`), exactly:
  - `` `${route} is down: until it is up, Tunnel Director blocks the traffic this client would send through it (excluded destinations still go direct). ${verb} anyway?` ``
  - `` `${route} is not on the router's tunnel list: until it is, Tunnel Director blocks the traffic this client would send through it (excluded destinations still go direct). ${verb} anyway?` ``
- Shell rules (`.claude/rules/shell-conventions.md`): never `cmd | grep -q` - read an output whole, then search it; no `[:upper:]`/`[:lower:]`; no jq regex builtins (`test`, `match`, `capture`, `scan`, `splits`, `sub`, `gsub`) in a jq program; a bash regex that holds `<` or `>` lives in a variable; `[[ ]]`, not `[ ]`, in bash files.
- `shellcheck -s bash router/opt/vpn-director/lib/iftunnel.sh` prints nothing.
- The repository is public: tests and docs use documentation addresses (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24) and RFC1918 LAN addresses, never a real server's address.
- English for code, comments, tests, commit messages and docs. Comments follow the voice of `lib/tunnel.sh`: full sentences that say why.
- Commits: `type(scope): summary`, a body that explains why, no trailers. Stage files by name only. Never stage `.claude/settings.local.json`, `docs/session-transfer-*.md`, `docs/superpowers/2026-*.md`, `docs/superpowers/plans/*-prompt.md`, `review.diff`, `test_exit.sh`.
- TDD: every new test is written first and seen failing for the reason its step names. A test marked **guard** pins behaviour that must not change and passes before and after.
- An existing test changes only where this plan says so, and only because the rule it pinned changed. Any other test that fails is a stop-and-ask, not a fix.
- Bats runs as CI runs it, from the repository root: `bats router/test/*.bats router/test/unit router/test/integration`. Go runs from `server/`: `go vet ./...`, `go test ./... -count=1`, `gofmt -l .` prints nothing. The web check is `cd web && npm ci && npm test && npm run build`.
- In the main session, builds and tests go through the `build-forge:build-runner` agent, model `opus`; subagents are `opus` too. Never Fable.
- No command touches the router before Task 12, and Task 12 runs each step only after the owner's yes. home2 (192.168.1.3) is the agent's own machine and reaches the Claude API through the tunnel.

## Review Focus

1. **Two interfaces whose names share a prefix** (`awg1`, `awg10`): releasing one, or replacing its inbound MSS rule, must leave the other's rules alone. Test: `iftunnel_table_release: leaves another interface's rules alone, a longer name included` (Task 2).
2. **An interface taken off `tunnel_director.interfaces` while its tunnel keeps clients:** the clients are refused, not routed on through the old table; the lookup rule, the route, NAT and MSS go. Test: `tunnel_apply: an interface taken off the list stops routing and its clients are refused` (Task 6).
3. **The firmware's rebuilds:** Merlin's `iptables -t mangle -F` takes the MSS clamps with the marks, and a rebuild of `nat` takes `MASQUERADE`; the next apply puts them back. Tests: `tunnel_apply: after Merlin emptied mangle, the rebuild puts the MSS clamps back` and `tunnel_apply: after the firmware rebuilt nat, the up-to-date path puts MASQUERADE back` (Task 4).
4. **An interface that exists but is not UP yet** (`S51awg1` sets it UP last): no route through it, and `ensure-routes` waits for UP. Tests: `iftunnel_route_ensure: no route through an interface that is not UP` (Task 2), `vpn-director: ensure-routes waits for an interface that is not UP yet` (Task 8).
5. **KeeneticOS while RCI is silent:** `platform_tunnels` answers `main` alone and every tunnel is unknown; a tunnel whose table the platform still names keeps routing instead of being cut off. Test: `tunnel_apply: a tunnel whose table the platform names keeps routing while the platform does not list it` (Task 6).

---

## File Structure

| File | Responsibility | Tasks |
|---|---|---|
| `router/opt/vpn-director/lib/iftunnel.sh` (new) | The interface list and its check, table, route, NAT/MSS, state file, status; the `vpd_*` layer | 1, 2, 3 |
| `router/opt/vpn-director/lib/common.sh` | Sources `iftunnel.sh` after `platform.sh` | 1 |
| `router/opt/vpn-director/lib/config.sh` | `TUN_DIR_INTERFACES` | 1 |
| `router/opt/vpn-director/lib/tunnel.sh` | Calls through the layer, hash, state, status; block rules; unknown tunnels; address exclusions | 4, 5, 6, 7 |
| `router/opt/vpn-director/vpn-director.sh` | `cmd_platform` through the layer; `ensure-routes`; the cron job; help | 3, 8 |
| `router/opt/vpn-director/configure.sh` | The wizard's tunnel menu through the layer | 3 |
| `router/jffs/scripts/nat-start` (new) | Merlin hook after a rebuild of `nat` | 8 |
| `router/files.manifest` | Ships `lib/iftunnel.sh` and `jffs/scripts/nat-start` | 1, 8 |
| `router/test/test_helper.bash` | `ip_links` helper | 1 |
| `router/test/mocks/ip` | `link show`, a route store, `prohibit` rules | 1, 2, 5 |
| `router/test/mocks/stateful/iptables` | Knows the `MASQUERADE` and `TCPMSS` targets | 2 |
| `router/test/unit/iftunnel.bats` (new) | The module and the layer | 1, 2, 3 |
| `router/test/unit/tunnel.bats` | New tests and the updates this plan names | 4, 5, 6, 7 |
| `router/test/config.bats` | Loading `tunnel_director.interfaces` | 1 |
| `router/test/integration/vpn_director.bats` | `platform`, `ensure-routes`, the cron job, help | 3, 8 |
| `router/test/unit/configure.bats` | The wizard's menu | 3 |
| `router/test/unit/hooks.bats` | `nat-start` | 8 |
| `server/internal/vpnconfig/vpnconfig.go`, `vpnconfig_test.go` | `TunnelDirectorConfig.Interfaces`; `PlatformTunnel.Type` comment | 9 |
| `server/internal/handler/clients.go`, `clients_test.go` | Bot texts | 9 |
| `web/src/components/ClientsTab.vue` | Web UI texts | 9 |
| `.claude/rules/tunnel-director.md`, `.claude/rules/packet-flow.md`, `.claude/rules/telegram-bot.md`, `.claude/rules/testing.md`, `README.md`, `README.ru.md`, `CLAUDE.md` | Documentation | 10 |

Helpers already in the suite that the tests below use: `load_common`, `load_firewall`, `load_config`, `load_tunnel_module`, `use_stateful_iptables` (`router/test/test_helper.bash`); `load_tunnel_module_with '<tunnels JSON>'` (sources the modules without `config.sh`, so a test may reassign `TUN_DIR_TUNNELS_JSON`), `marks_in_place`, `ovpn_mode <client> <rgw>`, `tun11_addressed` (`router/test/unit/tunnel.bats`); `run_stubbed_cli`, `run_cli` (`router/test/integration/vpn_director.bats`); `load_wizard` (`router/test/unit/configure.bats`); `setup_merlin_hook_env`, `run_hook`, `MERLIN_HOOKS` (`router/test/unit/hooks.bats`). The `ip` mock logs every call to `/tmp/bats_ip_calls.log` and keeps ip rules in `/tmp/bats_test_ip_rules`; the stateless `iptables` mock logs to `/tmp/bats_iptables_calls.log` and answers every `-C` with "missing". The fixture `rt_tables` names `wgc1`, `wgc2`, `ovpnc1`, `ovpnc2`; the `nvram` mock makes `eth0` the WAN and both OpenVPN clients VPN Director mode. Teardown removes `/tmp/bats_test_*` and `/tmp/tunnel_director/*`.

---

### Task 1: The interface list, its check and the table

✅ Done — see commit(s): `8abff82`

---

### Task 2: An interface tunnel's route, NAT and MSS

✅ Done — see commit(s): `fa573cc`

---

### Task 3: The `vpd_*` layer; `platform` and the wizard list interface tunnels

✅ Done — see commit(s): `6edf444`

---

### Task 4: Tunnel Director routes interface tunnels

✅ Done — see commit(s): `7f06bff`, `62bf595`

---

### Task 5: A tunnel with no route refuses its clients' traffic

✅ Done — see commit(s): `fe5cf37`, `5b3482a`

---

### Task 6: A tunnel the platform does not know refuses its clients

✅ Done — see commit(s): `9085215`

---

### Task 7: Addresses and CIDRs in a tunnel's `exclude`

✅ Done — see commit(s): `03a8774`

---

### Task 8: `ensure-routes`, its cron job and the `nat-start` hook

✅ Done — see commit(s): `8e40a15`

---

### Task 9: Keep the list in Go; the bot and the Web UI stop promising the WAN

✅ Done — see commit(s): `4c0c5c7`

---

### Task 10: Documentation

✅ Done — see commit(s): `cea83ce`, `ff11ab6`, `ed035b7`

---

### Task 11: Whole-branch verification

✅ Done — verified at `69f4874` (Bats 872/872, shellcheck clean, go vet/test/gofmt clean, npm test/build ok); no commit of its own. Final whole-branch review fix wave: `fb51d72`, `154b5cf`, `a57a6d9`, `69f4874`

---

### Task 12: Acceptance on the RT-AX86U (owner-gated)

Not for a subagent. Each step runs only after the owner's explicit yes for that step, and only after the owner's own check of 2026-10-16 (`awg1` comes back by itself after a reboot and holds for a day). Read-only commands on the router need no yes (`ssh -p 2222 admin@192.168.1.1`); never read `/opt/etc/amneziawg/awg1.conf`, never run `awg showconf`. home2 (192.168.1.3) runs the agent and reaches the Claude API through the tunnel: a step that moves it or stops its tunnel is announced first.

- [ ] **Step 1: Install the branch and apply.** `ip rule` shows a `prohibit` rule at 16639 beside the lookup rule at 16384; `vpn-director.sh status tunnel` reads clean; the clients of `ovpnc2` work as before.
- [ ] **Step 2: The kill switch on `ovpnc2`.** With the notebook (192.168.1.30) on `ovpnc2`, `service stop_vpnclient2`: the notebook's traffic to the tunnel is refused, not sent out through the WAN (`curl -4 ifconfig.me` from the notebook fails rather than showing the home address); excluded destinations still work. `service start_vpnclient2`: back. home2 loses its exit for that time as well - announce it.
- [ ] **Step 3: `awg1` as a tunnel.** Add `"interfaces": ["awg1"]` to `tunnel_director` in `/opt/vpn-director/vpn-director.json`, apply, move the notebook onto `awg1` in the Web UI. Its external address is yop's; `status tunnel` shows table `2000+idx`, the route and the three rules; `cru l` shows `vpn_director_routes`; `/tmp/tunnel_director/iftunnels` names `awg1`.
- [ ] **Step 4: Re-creation.** `/opt/etc/init.d/S51awg1 restart`: the notebook is refused during the restart, and within a minute `ensure-routes` logs `Interface tunnel back without its route: awg1` and the route is back.
- [ ] **Step 5: Firewall restart.** `service restart_firewall`: the marks, `MASQUERADE` and both MSS rules are back.
- [ ] **Step 6: nat rebuild.** A rebuild of `nat` by the firmware (the owner picks the trigger): `/tmp/syslog.log` shows the `nat-start` apply, and `MASQUERADE -o awg1` is back.
- [ ] **Step 7: The overlay channels.** Add yop's address to the `exclude` of `awg1` and apply: the router's conntrack shows the notebook's flow to yop:8443 through `eth0`, and the notebook's overlay channel to yop is alive.
- [ ] **Step 8: home2.** Move home2 onto `awg1` - announced; its overlay channel to yop is alive. The owner then decides what happens to `ovpnc2`.
- [ ] **Step 9: Hand over.** Tell the owner the "To do in wireguard-network" items of the spec (section 12): the background `vpn-director.sh --unless-stopped --wait apply tunnel` call in `S51awg1` after a successful `do_start`, with FD 9 closed; `docs/nodes/asus.md`; the two backlog items.

Before the PR (the owner's workflow): `git rm` every file under `docs/superpowers/` that the branch added (the spec and this plan) and commit, so that neither appears in the PR diff; they stay in the branch history.
