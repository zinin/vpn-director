---
paths: "router/test/**/*"
---

# Testing

## Framework

Uses [Bats](https://bats-core.readthedocs.io/) (Bash Automated Testing System) with bats-support and bats-assert libraries.

## Running Tests

```bash
# Four places: a directory argument is not recursive, so unit + integration
# alone miss router/test/*.bats
bats router/test/*.bats router/test/unit router/test/integration

bats -r router/test/           # Run all tests (recursive)
bats router/test/              # Top-level files only
bats router/test/unit/         # Run unit tests only
bats router/test/integration/  # Run integration tests only
bats router/test/common.bats   # Run specific test file
```

## Test Structure

```
router/test/
├── test_helper.bash         # Shared setup, helpers, paths
├── fixtures/                # Test data files
│   ├── hosts                # Mock /etc/hosts
│   ├── vpn-director.json    # Test config
│   ├── vless_servers.b64    # VLESS subscription mock
│   ├── rt_tables            # Mock routing tables
│   └── keenetic/
│       ├── proc_modules     # Mock /proc/modules
│       └── rci/**           # RCI JSON fixtures
├── mocks/                   # Mock executables
│   ├── nvram                # Mock nvram command
│   ├── iptables             # Mock iptables
│   ├── ip6tables            # Mock ip6tables
│   ├── ipset                # Mock ipset
│   ├── nslookup             # Mock DNS resolution
│   ├── logger               # Mock syslog
│   ├── ip                   # Mock ip command
│   ├── lsmod                # Mock kernel module list
│   ├── modprobe             # Mock module loading
│   ├── pgrep                # Mock process lookup
│   ├── insmod               # Mock insmod (Keenetic modules)
│   ├── cru                  # Mock cru (Merlin cron)
│   ├── xray                 # Mock "xray run -test" (XRAY_MOCK_EXIT, XRAY_MOCK_OUTPUT, XRAY_MOCK_LOG)
│   ├── stateful/iptables    # Chains and rules kept per test (use_stateful_iptables)
│   ├── stateful-ipset/ipset # XRAY_CLIENTS, TPROXY_BYPASS and their shadows kept per test (use_stateful_ipset)
│   └── keenetic/
│       └── curl             # RCI fixtures, per-test PATH
├── unit/                    # Unit tests for lib/ modules
│   ├── ipset.bats           # Tests for lib/ipset.sh
│   ├── tunnel.bats          # Tests for lib/tunnel.sh
│   ├── tproxy.bats          # Tests for lib/tproxy.sh
│   ├── platform.bats        # platform_detect and loader
│   ├── platform_merlin.bats
│   ├── platform_keenetic.bats
│   ├── hooks.bats           # NDM hooks
│   ├── install.bats
│   ├── configure.bats
│   ├── subscription.bats    # lib/subscription.sh on the shared cases in testdata/subscription
│   ├── substore.bats        # lib/substore.sh on the shared files in testdata/substore
│   └── xrayconf.bats        # lib/xrayconf.sh
├── integration/             # Integration tests
│   ├── vpn_director.bats    # Tests for vpn-director.sh CLI
│   └── ipset_sources.bats   # Tests for IPSet download sources
├── common.bats              # Tests for lib/common.sh
├── firewall.bats            # Tests for lib/firewall.sh
├── config.bats              # Tests for lib/config.sh
└── import_server_list.bats  # Tests for import_server_list.sh
```

## Platform under test

`VPD_PLATFORM` (default `merlin` from `test_helper.bash`; `platform_keenetic.bats`
and the Keenetic integration test in `vpn_director.bats` export `keenetic`)
selects the contract implementation without probing the machine. `VPD_PROBE_ROOT`
prefixes every path `platform_detect` looks at. Seams for Keenetic unit tests:
`VPD_MODULES_DIR`, `VPD_PROC_MODULES`, `VPD_CRON_D`, `VPD_CRON_INIT`,
`BATS_IP_*`, `BATS_RCI_*`.

## Writing Tests

### Basic Test

```bash
@test "function_name: description of behavior" {
    load_common  # Load utilities with mocks
    run function_name arg1 arg2
    assert_success
    assert_output "expected output"
}
```

### Test Helpers

| Helper | Purpose |
|--------|---------|
| `load_common` | Source lib/common.sh with mocks in PATH |
| `load_firewall` | Source lib/firewall.sh (includes common.sh) |
| `load_config` | Source lib/config.sh with test fixture |
| `load_ipset_module` | Source lib/ipset.sh module (uses `--source-only` flag) |
| `load_tunnel_module` | Source lib/tunnel.sh module (uses `--source-only` flag) |
| `load_tproxy_module` | Source lib/tproxy.sh module (uses `--source-only` flag) |
| `use_stateful_iptables` | For the rest of the test, `mocks/stateful/iptables` ahead of the stateless mock: chains and rules kept under `$BATS_IPT_DIR`, a flush of a live chain appended to `$BATS_IPT_DIR/live_flushes`; call it after the `load_*` helper |
| `use_stateful_ipset` | For the rest of the test, `mocks/stateful-ipset/ipset` ahead of the stateless mock: the members of `XRAY_CLIENTS`, `TPROXY_BYPASS` and their `_NEW` shadows kept under `$BATS_IPSET_DIR` (missing until created), so `ipset test` after an apply reads what the set holds; every other set stays with the stateless mock; call it after the `load_*` helper |

**Note:** Modules support `--source-only` flag for test sourcing without executing main logic.

### Assertions (bats-assert)

| Assertion | Purpose |
|-----------|---------|
| `assert_success` | Exit code 0 |
| `assert_failure` | Exit code != 0 |
| `assert_output "text"` | Exact match |
| `assert_output --partial "text"` | Contains substring |
| `assert_output --regexp "pattern"` | Regex match |
| `assert_line "text"` | Line exists in output |
| `refute_output` | Output is empty |

### Environment Variables

| Variable | Set By | Purpose |
|----------|--------|---------|
| `TEST_MODE=1` | test_helper.bash | Disables syslog, uses fixtures |
| `LOG_FILE` | test_helper.bash | Test-specific log file |
| `HOSTS_FILE` | test_helper.bash | Mock hosts file path |
| `VPD_CONFIG_FILE` | load_config | Test config fixture path |
| `BATS_TEST_DIRNAME` | Bats | Directory containing test file |
| `PROJECT_ROOT` | test_helper.bash | Project root directory |

## Mocks

Mocks are shell scripts in `router/test/mocks/` that simulate router commands:

- **nvram**: Returns predefined values for router settings
- **iptables/ip6tables**: Tracks rule operations
- **stateful/iptables**: Remembers chains and rules per table under `$BATS_IPT_DIR` (`-S`, `-N`, `-F`, `-X`, `-E`, `-A`, `-I`, `-D`, `-C`); records a flush of a chain a rule still jumps to (a whole-table `-F` is not recorded). Turned on per test by `use_stateful_iptables`
- **ipset**: Simulates ipset management
- **stateful-ipset/ipset**: Remembers `XRAY_CLIENTS`, `TPROXY_BYPASS` and their `_NEW` shadows under `$BATS_IPSET_DIR` (`list`, `create`, `add`, `del`, `test`, `flush`, `destroy`, `swap`); a member is compared as written, a `/32` as the bare address; unlike the kernel, `test` does not find a host inside a network the set holds. Everything else goes to the stateless mock. Turned on per test by `use_stateful_ipset`; the processes a test starts inherit it
- **nslookup**: Returns mock DNS responses
- **logger**: Silent (no syslog in tests)
- **ip**: Returns mock interface info
- **insmod**: Records module loads (Keenetic)
- **cru**: Merlin cron helper
- **keenetic/curl**: Serves RCI fixtures; put on PATH per test

Mocks are added to PATH before real commands via `setup()`.

## Adding New Tests

1. Create test file in appropriate directory:
   - `router/test/unit/` for module unit tests
   - `router/test/integration/` for CLI integration tests
   - `router/test/` for utility tests (common, firewall, config)
2. Add `load 'test_helper'` at top (use relative path: `load '../test_helper'` from subdirs)
3. Use appropriate `load_*` helper to source scripts
4. Add mocks to `router/test/mocks/` if needed
5. Run: `bats router/test/unit/new_module.bats` (or the four-place invocation above)

## Gotcha: modules replace the EXIT trap bats reports through

`common.sh` ends with `trap _cleanup_tmp EXIT INT TERM`. Sourcing it replaces
the EXIT trap bats installs to report a test result, so a failing assertion in
any test that called a `load_*` helper used to disappear into

```
# bats warning: Executed 33 instead of expected 36 tests
```

with no test name and no diff. The run still exits non-zero, so CI catches the
failure — it just cannot say which one.

`load_common` now saves the trap before sourcing and restores it afterwards,
and `teardown()` calls `_cleanup_tmp` in its place. If you add a helper that
sources a module some other way, keep the trap.

## Gotcha: a sourced script can replace a bats helper

A script the bats tests source must not define a function named like a bats
helper — `fail`, `run`, `load`, `skip`, `assert_*`, `refute_*`: sourcing it
replaces the helper, and failing assertions lose their diff (bats-assert
reports through `fail`; `import_server_list.bats` printed `$1: unbound
variable` instead). `import_server_list.sh` names its own `abort` for that
reason.

## Shared subscription cases

`testdata/subscription/` at the repository root holds `<case>.in` (a body as served) and
`<case>.want.json` (the error, or `total`, the servers and the skips by name and reason).
`router/test/unit/subscription.bats` runs every case through `lib/subscription.sh`, and
`server/internal/subscription/fixtures_test.go` through the Go decoder: a new case is two files,
and both decoders must pass it. The cases are synthetic — documentation addresses
(192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24, 2001:db8::/32), `example.com` hosts, made-up keys in
the formats Xray checks — because the repository is public and a provider's real hosts would be a
ready blocklist. Entware's jq has no regex builtins; `subscription.bats` fails when
`lib/subscription.sh` uses one.

`testdata/substore/` holds synthetic subscription files. `router/test/unit/substore.bats` and
`server/internal/vpnconfig/substore_test.go` both read them and must list them alike — the same
order, the same names — and both apply the same name rules to the same inputs. Entware's jq has no
regex builtins; `substore.bats` fails when `lib/substore.sh` uses one.
