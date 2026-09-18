#!/bin/sh
# shellcheck shell=bash
# KeeneticOS has no /usr/bin/env and mounts / read-only, so the usual
# "#!/usr/bin/env bash" cannot start this script there. Begin as a POSIX shell
# and hand over to bash by absolute path - Entware's first, a workstation's
# after it. PATH is no help here: Asuswrt-Merlin's /bin/sh has no "command"
# builtin (see .claude/rules/shell-conventions.md) and its /bin/bash is a
# symlink to busybox rather than bash.
if [ -z "${BASH_VERSION:-}" ]; then
    for _vpd_bash in /opt/bin/bash /usr/bin/bash /bin/bash; do
        # Asuswrt-Merlin's /bin/bash is busybox: a POSIX shell that never sets
        # BASH_VERSION, so exec-ing it would re-run this block forever. Only a
        # candidate that proves it is bash gets the script.
        [ -x "$_vpd_bash" ] && "$_vpd_bash" -c '[ -n "$BASH_VERSION" ]' 2>/dev/null &&
            exec "$_vpd_bash" "$0" "$@"
    done
    echo "$0: bash not found; install it (Entware package \"bash\")" >&2
    exit 1
fi
set -euo pipefail

# Debug mode: set DEBUG=1 to enable tracing
if [[ ${DEBUG:-0} == 1 ]]; then
    set -x
    PS4='+${BASH_SOURCE[0]##*/}:${LINENO}:${FUNCNAME[0]:-main}: '
fi

###############################################################################
# VPN Director Configuration Wizard
# Run this after install.sh to configure Xray TPROXY and Tunnel Director
###############################################################################

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Paths (overridable so the wizard can be sourced by tests)
VPD_DIR="${VPD_DIR:-/opt/vpn-director}"
XRAY_CONFIG_DIR="${XRAY_CONFIG_DIR:-/opt/etc/xray}"

# Library: Xray config generator (self-contained pure-jq; no common.sh needed)
. "$VPD_DIR/lib/xrayconf.sh"

# Platform contract: the tunnels this router has (platform_tunnels,
# platform_tunnel_info). platform.sh alone, not common.sh: the contract
# functions never log, and common.sh would install its temp-file EXIT trap
# into an interactive wizard.
. "$VPD_DIR/lib/platform.sh"

# Temporary storage for parsed data
XRAY_CLIENTS_LIST=""
TUN_DIR_TUNNELS_JSON='{}'
SELECTED_SERVER_ADDRESS=""
SELECTED_SERVER_PORT=""
SELECTED_SERVER_JSON=""
XRAY_EXCLUDE_SETS_LIST="ru"

###############################################################################
# Helper functions
###############################################################################

print_header() {
    printf "\n${BLUE}=== %s ===${NC}\n\n" "$1"
}

print_success() {
    printf "${GREEN}[OK]${NC} %s\n" "$1"
}

print_error() {
    printf "${RED}[ERROR]${NC} %s\n" "$1"
}

print_warning() {
    printf "${YELLOW}[WARN]${NC} %s\n" "$1"
}

print_info() {
    printf "${BLUE}[INFO]${NC} %s\n" "$1"
}

# wizard_tunnel_choices - the tunnels this router has, one numbered line each
# ("  N) <id>  <description> (down)"); main is not offered.
wizard_tunnel_choices() {
    local id info desc connected n=0
    while IFS= read -r id; do
        [[ -n $id && $id != main ]] || continue
        n=$((n + 1))
        desc=""
        connected=1
        if info="$(platform_tunnel_info "$id")"; then
            connected="$(printf '%s\n' "$info" | sed -n 2p)"
            desc="$(printf '%s\n' "$info" | sed -n 3p)"
        fi
        printf '  %d) %s%s%s\n' "$n" "$id" "${desc:+  $desc}" "$([[ $connected == 1 ]] || printf ' (down)')"
    done < <(platform_tunnels || true)
}

# wizard_tunnel_by_number <n> - the id at position n of wizard_tunnel_choices,
# nothing for anything else. The awk filter is the one wizard_tunnel_choices
# numbers by, so a number read off the menu cannot name a different tunnel.
# Never fails, whatever the user typed: the caller assigns its output under
# "set -e", so a non-zero return would abort the wizard instead of asking
# again - hence [1-9] rather than [0-9], since sed rejects the line address 0
# with an error, and the trailing "|| true" for a platform_tunnels that fails.
wizard_tunnel_by_number() {
    [[ ${1:-} =~ ^[1-9][0-9]*$ ]] || return 0
    platform_tunnels 2>/dev/null | awk 'length && $0 != "main"' | sed -n "${1}p" || true
}

# shellcheck disable=SC2034  # INPUT_RESULT used by callers
read_input() {
    printf "%s: " "$1"
    read -r INPUT_RESULT
}

confirm() {
    printf "%s [y/n]: " "$1"
    read -r REPLY
    case "$REPLY" in
        [Yy]*) return 0 ;;
        *) return 1 ;;
    esac
}

###############################################################################
# Get data directory and validate servers
###############################################################################

get_data_dir() {
    local config_file="$VPD_DIR/vpn-director.json"

    if [[ ! -f $config_file ]]; then
        config_file="$VPD_DIR/vpn-director.json.template"
    fi

    jq -r '.data_dir // "/opt/vpn-director/data"' "$config_file"
}

check_servers_file() {
    DATA_DIR=$(get_data_dir)
    SERVERS_FILE="$DATA_DIR/servers.json"

    if [[ ! -f $SERVERS_FILE ]]; then
        print_error "Server list not found: $SERVERS_FILE"
        print_info "Run import_server_list.sh first"
        exit 1
    fi

    SERVER_COUNT=$(jq length "$SERVERS_FILE")
    if [[ $SERVER_COUNT -eq 0 ]]; then
        print_error "Server list is empty"
        print_info "Run import_server_list.sh again"
        exit 1
    fi

    print_success "Found $SERVER_COUNT servers in $SERVERS_FILE"
}

###############################################################################
# Step 1: Select Xray server
###############################################################################

step_select_xray_server() {
    print_header "Step 1: Select Xray Server"

    printf "Available servers:\n\n"

    # Read servers from JSON and display
    i=1
    jq -r '.[] | "\(.name)|\(.address)|\((.ips // []) | join(", "))"' "$SERVERS_FILE" | \
    while IFS='|' read -r name address ip; do
        printf "  %2d) %s\n      %s -> %s\n\n" "$i" "$name" "$address" "$ip"
        i=$((i + 1))
    done

    total=$(jq length "$SERVERS_FILE")

    while true; do
        printf "Select server [1-%d]: " "$total"
        read -r choice

        if [[ $choice -ge 1 ]] 2>/dev/null && [[ $choice -le $total ]] 2>/dev/null; then
            break
        fi
        print_error "Invalid choice. Enter a number between 1 and $total"
    done

    # Get selected server data (jq uses 0-based index)
    idx=$((choice - 1))
    SELECTED_SERVER_ADDRESS=$(jq -r ".[$idx].address" "$SERVERS_FILE")
    SELECTED_SERVER_PORT=$(jq -r ".[$idx].port" "$SERVERS_FILE")
    SELECTED_SERVER_JSON=$(jq -c ".[$idx]" "$SERVERS_FILE")
    selected_name=$(jq -r ".[$idx].name" "$SERVERS_FILE")

    print_success "Selected: $selected_name ($SELECTED_SERVER_ADDRESS)"
}

###############################################################################
# Step 2: Configure Xray exclusions
###############################################################################

step_configure_xray_exclusions() {
    print_header "Step 2: Xray Exclusions"

    printf "Traffic to these countries will NOT go through Xray proxy.\n"
    printf "Common choice: your local country to avoid unnecessary proxying.\n\n"

    # List of common countries
    printf "Available countries:\n\n"
    printf "  1) ru - Russia          6) fr - France\n"
    printf "  2) ua - Ukraine         7) nl - Netherlands\n"
    printf "  3) by - Belarus         8) pl - Poland\n"
    printf "  4) kz - Kazakhstan      9) tr - Turkey\n"
    printf "  5) de - Germany        10) il - Israel\n"
    printf "\n"

    printf "Enter country numbers separated by space (e.g., 1 5 7)\n"
    printf "or 0 for none [default: 1]: "
    read -r choice

    # Default to ru (1) if empty
    if [[ -z $choice ]]; then
        choice="1"
    fi

    # Convert numbers to country codes
    XRAY_EXCLUDE_SETS_LIST=""
    for num in $choice; do
        case "$num" in
            0) XRAY_EXCLUDE_SETS_LIST=""; break ;;
            1) code="ru" ;;
            2) code="ua" ;;
            3) code="by" ;;
            4) code="kz" ;;
            5) code="de" ;;
            6) code="fr" ;;
            7) code="nl" ;;
            8) code="pl" ;;
            9) code="tr" ;;
            10) code="il" ;;
            *) print_warning "Unknown option: $num"; continue ;;
        esac
        if [[ -z $XRAY_EXCLUDE_SETS_LIST ]]; then
            XRAY_EXCLUDE_SETS_LIST="$code"
        else
            XRAY_EXCLUDE_SETS_LIST="$XRAY_EXCLUDE_SETS_LIST,$code"
        fi
    done

    if [[ -n $XRAY_EXCLUDE_SETS_LIST ]]; then
        print_success "Excluding: $XRAY_EXCLUDE_SETS_LIST"
    else
        print_info "No countries excluded"
    fi
}

###############################################################################
# Step 3: Configure clients
###############################################################################

# Helper: select exclude countries
select_exclude_countries() {
    printf "\n"
    printf "Select countries to EXCLUDE from tunnel (direct routing):\n"
    printf "  1) ru - Russia          6) fr - France\n"
    printf "  2) ua - Ukraine         7) nl - Netherlands\n"
    printf "  3) by - Belarus         8) pl - Poland\n"
    printf "  4) kz - Kazakhstan      9) tr - Turkey\n"
    printf "  5) de - Germany        10) il - Israel\n"
    printf "\n"
    printf "Enter numbers separated by space (e.g., 1 5 7) or 0 for none [default: 1]: "
    read -r choice

    if [[ -z $choice ]]; then
        choice="1"
    fi

    local exclude_list=""
    for num in $choice; do
        local code=""
        case "$num" in
            0) exclude_list=""; break ;;
            1) code="ru" ;;
            2) code="ua" ;;
            3) code="by" ;;
            4) code="kz" ;;
            5) code="de" ;;
            6) code="fr" ;;
            7) code="nl" ;;
            8) code="pl" ;;
            9) code="tr" ;;
            10) code="il" ;;
            *) print_warning "Unknown option: $num"; continue ;;
        esac
        if [[ -z $exclude_list ]]; then
            exclude_list="$code"
        else
            exclude_list="$exclude_list $code"
        fi
    done

    SELECTED_EXCLUDE="$exclude_list"
}

# Helper: add client to tunnel
add_client_to_tunnel() {
    local tunnel="$1"
    local client="$2"
    local exclude="$3"

    # Add /32 suffix if not present
    if [[ $client != */* ]]; then
        client="${client}/32"
    fi

    # Build exclude JSON array
    local exclude_json='[]'
    if [[ -n $exclude ]]; then
        # shellcheck disable=SC2086  # intentional word splitting
        exclude_json=$(printf '%s\n' $exclude | jq -R . | jq -s .)
    fi

    # Check if tunnel already exists
    if jq -e --arg t "$tunnel" '.[$t]' <<< "$TUN_DIR_TUNNELS_JSON" >/dev/null 2>&1; then
        # Tunnel exists - add client to existing array
        TUN_DIR_TUNNELS_JSON=$(jq --arg t "$tunnel" --arg c "$client" \
            '.[$t].clients += [$c]' <<< "$TUN_DIR_TUNNELS_JSON")
    else
        # New tunnel - create with clients and exclude
        TUN_DIR_TUNNELS_JSON=$(jq --arg t "$tunnel" --arg c "$client" --argjson e "$exclude_json" \
            '.[$t] = {clients: [$c], exclude: $e}' <<< "$TUN_DIR_TUNNELS_JSON")
    fi
}

# wizard_lan_client_ok <addr> - an IPv4 address or CIDR in a private range,
# written the way iptables and ipset read it. The prefix alone let
# 192.168.1.1000 through, which "iptables -s" refuses, and 192.168.1.08, which
# it reads as octal; Tunnel Director carries Xray clients during a failover,
# and one such address used to end the whole apply. The same test as
# is_ipv4_net and is_lan_ip in lib/common.sh, which this wizard does not source.
wizard_lan_client_ok() {
    local addr="${1:-}"
    local re_octet='(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9][0-9]|[0-9])'
    local re_prefix='(3[0-2]|[12][0-9]|[0-9])'
    [[ $addr =~ ^$re_octet\.$re_octet\.$re_octet\.$re_octet(/$re_prefix)?$ ]] || return 1
    case "${addr%%/*}" in
        192.168.*|10.*|172.1[6-9].*|172.2[0-9].*|172.3[0-1].*) return 0 ;;
        *) return 1 ;;
    esac
}

step_configure_clients() {
    print_header "Step 3: Configure Clients"

    printf "Add LAN clients for routing.\n"
    printf "Enter 'done' when finished.\n\n"

    XRAY_CLIENTS_LIST=""
    TUN_DIR_TUNNELS_JSON='{}'

    while true; do
        printf "Client IP or CIDR (or 'done'): "
        read -r client_ip

        if [[ $client_ip == "done" ]]; then
            break
        fi

        if ! wizard_lan_client_ok "$client_ip"; then
            print_error "Invalid LAN IP: $client_ip"
            continue
        fi

        printf "\nWhere to route traffic for %s?\n" "$client_ip"
        printf "  1) Xray (VLESS proxy)\n"
        printf "  2) Tunnel Director (VPN tunnel)\n"
        printf "Choice [1-2]: "
        read -r route_choice

        case "$route_choice" in
            1)
                XRAY_CLIENTS_LIST="${XRAY_CLIENTS_LIST}${client_ip}
"
                print_success "$client_ip -> Xray"
                ;;
            2)
                local choices tunnel tunnel_num
                choices="$(wizard_tunnel_choices)"
                if [[ -z $choices ]]; then
                    print_error "This router has no VPN client tunnel; configure one in the router UI first"
                    continue
                fi
                printf "\nTunnel:\n%s\nChoice: " "$choices"
                read -r tunnel_num
                tunnel="$(wizard_tunnel_by_number "$tunnel_num")"
                if [[ -z $tunnel ]]; then
                    print_error "Invalid tunnel number"
                    continue
                fi

                # Ask for exclude countries only for new tunnels
                if ! jq -e --arg t "$tunnel" '.[$t]' <<< "$TUN_DIR_TUNNELS_JSON" >/dev/null 2>&1; then
                    select_exclude_countries
                    add_client_to_tunnel "$tunnel" "$client_ip" "$SELECTED_EXCLUDE"
                    print_success "$client_ip -> $tunnel (exclude: ${SELECTED_EXCLUDE:-none})"
                else
                    # Tunnel exists - just add client
                    add_client_to_tunnel "$tunnel" "$client_ip" ""
                    print_success "$client_ip -> $tunnel (using existing exclude)"
                fi
                ;;
            *)
                print_error "Invalid choice"
                ;;
        esac

        printf "\n"
    done

    if [[ -z $XRAY_CLIENTS_LIST ]] && [[ $TUN_DIR_TUNNELS_JSON == '{}' ]]; then
        print_warning "No clients configured"
    fi
}

###############################################################################
# Step 4: Show summary
###############################################################################

step_show_summary() {
    print_header "Step 4: Configuration Summary"

    printf "Xray Server:\n"
    printf "  Address: %s\n" "$SELECTED_SERVER_ADDRESS"
    printf "  Port: %s\n" "$SELECTED_SERVER_PORT"
    printf "\n"

    printf "Xray Exclusions: %s\n\n" "$XRAY_EXCLUDE_SETS_LIST"

    printf "Xray Clients:\n"
    if [[ -n $XRAY_CLIENTS_LIST ]]; then
        printf '%s' "$XRAY_CLIENTS_LIST" | while read -r ip; do
            [[ -n $ip ]] && printf "  - %s\n" "$ip"
        done
    else
        printf "  (none)\n"
    fi
    printf "\n"

    printf "Tunnel Director Tunnels:\n"
    if [[ $TUN_DIR_TUNNELS_JSON != '{}' ]]; then
        jq -r 'to_entries[] | "  \(.key):\n    clients: \(.value.clients | join(", "))\n    exclude: \(.value.exclude | join(", "))"' <<< "$TUN_DIR_TUNNELS_JSON"
    else
        printf "  (none)\n"
    fi
    printf "\n"

    if ! confirm "Proceed with configuration?"; then
        print_info "Configuration cancelled"
        exit 0
    fi
}

###############################################################################
# Step 5: Generate config files
###############################################################################

step_generate_configs() {
    print_header "Step 5: Generating Configs"

    if [[ ! -f $VPD_DIR/vpn-director.json.template ]]; then
        print_error "Template not found: $VPD_DIR/vpn-director.json.template"
        print_info "Run install.sh first to download required files"
        exit 1
    fi

    # Build JSON arrays
    xray_clients_json="[]"
    if [[ -n $XRAY_CLIENTS_LIST ]]; then
        xray_clients_json=$(printf '%s' "$XRAY_CLIENTS_LIST" | grep -v '^$' | jq -R . | jq -s .)
    fi

    xray_exclude_json='["ru"]'
    if [[ -n $XRAY_EXCLUDE_SETS_LIST ]]; then
        # shellcheck disable=SC2086  # intentional word splitting
        xray_exclude_json=$(printf '%s\n' ${XRAY_EXCLUDE_SETS_LIST//,/ } | jq -R . | jq -s .)
    fi

    # Build xray servers array from servers.json (unique IPs)
    xray_servers_json="[]"
    if [[ -f "$SERVERS_FILE" ]]; then
        xray_servers_json=$(jq '[.[].ips[]] | unique' "$SERVERS_FILE")
    fi

    # Which server config.json is about to be built from. Nothing else records
    # it, and it cannot be read back out of config.json afterwards: a
    # subscription routinely puts a dozen names behind one address:port. Only
    # the three fields that identify the server to a reader - the Web UI serves
    # this file over /api/config, so the uuid and the REALITY material stay out.
    xray_active_server_json=$(printf '%s' "$SELECTED_SERVER_JSON" \
        | jq -c '{name: (.name // ""), address: (.address // ""), port: (.port // 0)}')

    # The Web UI and the bot serialise their writes on this lock
    # (service.ConfigService.LockPath). The wizard is the third writer over the
    # same file: without the lock a daemon write landing between the read and
    # the rename below is lost. Thirty seconds matches the Go side's timeout.
    # BusyBox flock has no -w, hence the loop.
    exec 9>"$VPD_DIR/.vpn-director.json.lock"
    _cfg_lock_waited=0
    until flock -n 9; do
        if [[ $_cfg_lock_waited -ge ${VPD_CONFIG_LOCK_WAIT:-30} ]]; then
            print_error "Config is locked by the Web UI or the bot; try again"
            exec 9>&-
            exit 1
        fi
        [[ $_cfg_lock_waited -eq 0 ]] && print_info "Waiting for the config lock..."
        sleep 1
        _cfg_lock_waited=$((_cfg_lock_waited + 1))
    done

    # The wizard owns five fields; everything else in the file belongs to the
    # daemons and to the user: the jwt_secret the Web UI generates on its first
    # start, exclude_ips and paused_clients written by both daemons, data_dir
    # and the advanced section. So build on the existing config rather than on
    # the template, and merge the template underneath it so keys added by an
    # update still arrive. A first run has no config and takes the template as
    # it is.
    base_json=$(cat "$VPD_DIR/vpn-director.json.template")
    if [[ -f $VPD_DIR/vpn-director.json ]]; then
        if _merged=$(jq -s '.[0] * .[1]' \
            "$VPD_DIR/vpn-director.json.template" "$VPD_DIR/vpn-director.json"); then
            base_json="$_merged"
        else
            print_warning "Existing config is not valid JSON, falling back to the template"
        fi
    fi

    # The dokodemo-door port has to match advanced.xray.tproxy_port: that is
    # where the TPROXY rules send traffic, and the template's default would
    # leave a preserved custom port without a listener. Same for the socks
    # port, which setup_telegram_bot.sh reads from the config.
    _tproxy_port=$(printf '%s' "$base_json" | jq -r '.advanced.xray.tproxy_port // empty')
    _socks_port=$(printf '%s' "$base_json" | jq -r '.advanced.xray.socks_port // empty')

    # Generate xray/config.json from template
    print_info "Generating Xray config..."

    # Generate to a temp file first, then replace atomically. A '>' redirect would
    # truncate the live config.json before xrayconf_generate runs, so a generator
    # failure (bad params, jq/template error) would leave the router with an empty
    # config. Write-then-mv keeps the existing config intact on any failure.
    _xray_cfg_tmp=$(mktemp "$XRAY_CONFIG_DIR/config.json.XXXXXX")
    if printf '%s' "$SELECTED_SERVER_JSON" \
        | xrayconf_generate "$XRAY_CONFIG_DIR/config.json.template" \
            "$_tproxy_port" "$_socks_port" \
        > "$_xray_cfg_tmp"; then
        mv -f "$_xray_cfg_tmp" "$XRAY_CONFIG_DIR/config.json"
        print_success "Generated $XRAY_CONFIG_DIR/config.json"
    else
        rm -f "$_xray_cfg_tmp"
        flock -u 9
        exec 9>&-
        print_error "Failed to generate Xray config (invalid server params?); kept existing config.json"
        exit 1
    fi

    print_info "Generating vpn-director.json..."

    # Write to a temp file first: a '>' redirect truncates the live config
    # before jq runs, so a generator failure would take the jwt_secret with it.
    _vpd_cfg_tmp=$(mktemp "$VPD_DIR/vpn-director.json.XXXXXX")
    # The wizard's in-memory tunnels are {clients, exclude} only. Copy
    # gateway from the on-disk object for ids that still exist, so a
    # hand-set Keenetic OpenVPN next hop survives a wizard save. Do not
    # write "gateway": "" and do not invent the key on a newly created
    # tunnel. `$existing[.key].gateway // empty` would drop new ids:
    # empty as $gw skips the map item. `//` replaces only null and false,
    # so a tunnels value of the wrong type (an array, say) is mapped to {}
    # by hand: indexing it by key would end the whole save with jq's rc 5.
    if printf '%s' "$base_json" | jq \
        --argjson clients "$xray_clients_json" \
        --argjson exclude "$xray_exclude_json" \
        --argjson tunnels "$TUN_DIR_TUNNELS_JSON" \
        --argjson servers "$xray_servers_json" \
        --argjson active "$xray_active_server_json" \
        '((.tunnel_director.tunnels? // {}) | if type == "object" then . else {} end) as $existing |
         .xray.clients = $clients |
         .xray.exclude_sets = $exclude |
         .xray.servers = $servers |
         .xray.active_server = $active |
         .tunnel_director.tunnels = (
             $tunnels
             | to_entries
             | map(
                 (if ($existing[.key] | type) == "object" and ($existing[.key].gateway | type) == "string"
                  then $existing[.key].gateway
                  else "" end) as $gw
                 | if (.value | has("gateway") | not) and ($gw != "")
                   then .value += {gateway: $gw}
                   else .
                   end
               )
             | from_entries
         )' \
        > "$_vpd_cfg_tmp"; then
        chmod 600 "$_vpd_cfg_tmp"
        mv -f "$_vpd_cfg_tmp" "$VPD_DIR/vpn-director.json"
        flock -u 9
        exec 9>&-
        print_success "Generated $VPD_DIR/vpn-director.json"
    else
        rm -f "$_vpd_cfg_tmp"
        flock -u 9
        exec 9>&-
        print_error "Failed to generate vpn-director.json; kept the existing config"
        exit 1
    fi
}

###############################################################################
# Step 6: Apply rules
###############################################################################

step_apply_rules() {
    print_header "Step 6: Applying Rules"

    print_info "Applying configuration (this may take a while)..."
    if [[ -x $VPD_DIR/vpn-director.sh ]]; then
        "$VPD_DIR/vpn-director.sh" restart || {
            print_warning "vpn-director.sh restart failed"
            return 1
        }
        print_success "Configuration applied"
    else
        print_error "vpn-director.sh not found"
        return 1
    fi
}

###############################################################################
# Main
###############################################################################

main() {
    print_header "VPN Director Configuration"
    printf "This wizard will configure Xray TPROXY and Tunnel Director.\n\n"

    check_servers_file                # Validate servers.json exists
    step_select_xray_server           # Step 1
    step_configure_xray_exclusions    # Step 2
    step_configure_clients            # Step 3
    step_show_summary                 # Step 4
    step_generate_configs             # Step 5
    step_apply_rules                  # Step 6

    print_header "Configuration Complete"
    printf "Check status with: /opt/vpn-director/vpn-director.sh status\n"
}

###############################################################################
# Allow sourcing for testing
###############################################################################

if [[ ${1:-} == "--source-only" ]]; then
    # shellcheck disable=SC2317
    return 0 2>/dev/null || exit 0
fi

main "$@"
