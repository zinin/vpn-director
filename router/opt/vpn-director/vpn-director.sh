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

###################################################################################################
# vpn-director.sh - Unified CLI for VPN Director
# -------------------------------------------------------------------------------------------------
# Usage:
#   vpn-director status [tunnel|xray|ipset]  - Show status
#   vpn-director apply [tunnel|xray]         - Apply configuration
#   vpn-director stop [tunnel|xray]          - Stop components
#   vpn-director restart [tunnel|xray]       - Restart components
#   vpn-director update                      - Update ipsets and reapply all
#   vpn-director platform                    - Print platform facts as JSON (for the daemons)
#   vpn-director cron install|remove         - Schedule or drop the daily ipset update
#
# Options:
#   -f, --force    Force operation (ignore hash checks)
#   -q, --quiet    Minimal output
#   -v, --verbose  Debug output
#   --dry-run      Show what would be done
#   --wait[=SEC]   Wait up to SEC seconds (default 120) for a running instance instead of exiting
#   -h, --help     Show this help
###################################################################################################

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Support --source-only for testing (must be before any parsing)
[[ "${1:-}" == "--source-only" ]] && { shift; _SOURCE_ONLY=1; } || _SOURCE_ONLY=0

###################################################################################################
# Parse options (supports both pre-command and post-command placement)
# vpn-director --force apply     # works
# vpn-director apply --force     # works
# vpn-director apply tunnel -v   # works
###################################################################################################
FORCE=0
QUIET=0
VERBOSE=0
DRY_RUN=0
COMMAND=""
COMPONENT=""

parse_option() {
    case $1 in
        -f|--force)   FORCE=1; return 0 ;;
        -q|--quiet)   QUIET=1; return 0 ;;
        -v|--verbose) VERBOSE=1; export DEBUG=1; return 0 ;;
        --dry-run)    DRY_RUN=1; return 0 ;;
        --wait)       export VPD_LOCK_WAIT=120; return 0 ;;
        --wait=*)
            local secs="${1#--wait=}"
            [[ $secs =~ ^[0-9]+$ ]] || { echo "Invalid --wait value: $secs (expected seconds)" >&2; exit 1; }
            export VPD_LOCK_WAIT="$secs"; return 0 ;;
        -h|--help)    COMMAND="help"; return 0 ;;
        -*)           echo "Unknown option: $1" >&2; exit 1 ;;
        *)            return 1 ;;
    esac
}

# Phase 1: Parse pre-command options and extract command/component
while [[ $# -gt 0 ]]; do
    if parse_option "$1"; then
        shift
    elif [[ -z $COMMAND ]]; then
        COMMAND="$1"; shift
    elif [[ -z $COMPONENT ]]; then
        COMPONENT="$1"; shift
    else
        # Extra positional argument
        echo "Unexpected argument: $1" >&2; exit 1
    fi
done

# Phase 2: Default command
COMMAND="${COMMAND:-help}"

# Debug mode
if [[ $VERBOSE -eq 1 ]]; then
    set -x
    PS4='+${BASH_SOURCE[0]##*/}:${LINENO}:${FUNCNAME[0]:-main}: '
fi

###################################################################################################
# Help (shown before loading modules - no config required)
###################################################################################################
show_help() {
    cat <<'EOF'
VPN Director - Unified traffic routing for Asuswrt-Merlin and KeeneticOS

Usage:
  vpn-director <command> [component] [options]

Commands:
  status [tunnel|xray|ipset]  Show status (all or specific component)
  apply [tunnel|xray]         Apply configuration
  stop [tunnel|xray]          Stop components
  restart [tunnel|xray]       Restart (stop + apply)
  update                      Download fresh ipsets and reapply all
  platform                    Print platform facts as JSON
  cron install|remove         Schedule or drop the daily "update" job

Options:
  -f, --force    Force operation (ignore hash checks)
  -q, --quiet    Minimal output
  -v, --verbose  Debug output
  --dry-run      Show what would be done
  --wait[=SEC]   Wait up to SEC seconds (default 120) for a running instance instead of exiting
  -h, --help     Show this help

Examples:
  vpn-director status              # Show all status
  vpn-director apply               # Apply all (ipsets + tunnel + xray)
  vpn-director restart tunnel      # Restart only Tunnel Director
  vpn-director update              # Update ipsets from IPdeny

EOF
}

###################################################################################################
# Load modules (deferred until after help check)
###################################################################################################
# common.sh alone: a command that mutates state has to take the lock before
# lib/config.sh reads the JSON. Reading first applies the file as it was when
# the process started, and a --wait apply that waited out another one would
# then commit a snapshot its writer has already superseded - with two queued
# applies, in either order.
_load_common() {
    [[ ${_COMMON_LOADED:-0} -eq 1 ]] && return 0
    . "$SCRIPT_DIR/lib/common.sh"
    _COMMON_LOADED=1
}

_load_modules() {
    [[ ${_MODULES_LOADED:-0} -eq 1 ]] && return 0
    _load_common
    . "$SCRIPT_DIR/lib/firewall.sh"
    . "$SCRIPT_DIR/lib/config.sh"
    . "$SCRIPT_DIR/lib/ipset.sh" --source-only
    . "$SCRIPT_DIR/lib/tunnel.sh" --source-only
    . "$SCRIPT_DIR/lib/tproxy.sh" --source-only
    _MODULES_LOADED=1
}

###################################################################################################
# Helper to ensure ipsets (handles space-separated list)
###################################################################################################
_ensure_ipsets() {
    local set
    for set in "$@"; do
        [[ -z $set ]] && continue
        ipset_ensure "$set" || return 1
    done
}

###################################################################################################
# Commands
###################################################################################################

cmd_status() {
    _load_modules
    case "$COMPONENT" in
        ""|all)
            ipset_status
            echo ""
            tunnel_status
            echo ""
            tproxy_status
            ;;
        ipset)
            ipset_status
            ;;
        tunnel)
            tunnel_status
            ;;
        xray|tproxy)
            tproxy_status
            ;;
        *)
            echo "Unknown component: $COMPONENT" >&2
            exit 1
            ;;
    esac
}

cmd_apply() {
    _load_common
    acquire_lock "vpn-director"
    _load_modules

    # Handle --dry-run: show plan without applying (skip boot wait and lock)
    if [[ $DRY_RUN -eq 1 ]]; then
        case "$COMPONENT" in
            ""|all)
                log "DRY-RUN: would apply all components"
                log "DRY-RUN: tunnel ipsets needed: $(tunnel_get_required_ipsets)"
                log "DRY-RUN: tproxy ipsets needed: $(tproxy_get_required_ipsets)"
                ;;
            tunnel)
                log "DRY-RUN: would apply tunnel"
                log "DRY-RUN: tunnel ipsets needed: $(tunnel_get_required_ipsets)"
                ;;
            xray|tproxy)
                log "DRY-RUN: would apply tproxy"
                log "DRY-RUN: tproxy ipsets needed: $(tproxy_get_required_ipsets)"
                ;;
            *)
                echo "Unknown component: $COMPONENT" >&2
                exit 1
                ;;
        esac
        return 0
    fi

    # stop's marker pauses the Telegram bot's subscription watch, and an apply
    # hands routing back to it - a real one only, so not above the dry run.
    rm -f "${VPD_STOPPED_FILE:-/tmp/vpn-director/stopped}"

    # Wait for network if system just booted (before any downloads)
    _ipset_boot_wait

    # Handle --force: pass to ipset module
    [[ $FORCE -eq 1 ]] && export IPSET_FORCE_UPDATE=1

    local required_ipsets=""

    case "$COMPONENT" in
        ""|all)
            required_ipsets="$(tunnel_get_required_ipsets) $(tproxy_get_required_ipsets)"
            required_ipsets=$(echo $required_ipsets | xargs -n1 | sort -u | xargs)

            if [[ -n $required_ipsets ]]; then
                log "Ensuring ipsets: $required_ipsets"
                # shellcheck disable=SC2086
                _ensure_ipsets $required_ipsets
            fi

            # Xray first. tproxy_apply soft-fails (a WARN and rc 0) while tunnel_apply
            # hard-fails under errexit, so this order lets a Tunnel Director failure -
            # a malformed tunnels object, say - end the run without stripping Xray
            # clients of their TPROXY rules. The PREROUTING positions: XRAY_TPROXY always
            # inserts at 1; TUN_DIR at the platform's base position, never ahead of the
            # XRAY_TPROXY jumps already in place - tunnel_apply counts them - so the
            # order is [XRAY_TPROXY, TUN_DIR] on both platforms. Neither call is wrapped
            # in `||` or `if !`: that would run its whole body with errexit off and let
            # an unguarded failure inside pass as success.
            tproxy_apply
            tunnel_apply
            ;;
        tunnel)
            required_ipsets=$(tunnel_get_required_ipsets)
            if [[ -n $required_ipsets ]]; then
                # shellcheck disable=SC2086
                _ensure_ipsets $required_ipsets
            fi
            tunnel_apply
            ;;
        xray|tproxy)
            required_ipsets=$(tproxy_get_required_ipsets)
            if [[ -n $required_ipsets ]]; then
                # shellcheck disable=SC2086
                _ensure_ipsets $required_ipsets
            fi
            tproxy_apply
            ;;
        *)
            echo "Unknown component: $COMPONENT" >&2
            exit 1
            ;;
    esac
}

cmd_stop() {
    _load_common
    acquire_lock "vpn-director"
    _load_modules

    case "$COMPONENT" in
        ""|all)
            tproxy_stop
            tunnel_stop
            ;;
        tunnel)
            tunnel_stop
            ;;
        xray|tproxy)
            tproxy_stop
            ;;
        *)
            echo "Unknown component: $COMPONENT" >&2
            exit 1
            ;;
    esac
    case "$COMPONENT" in
        ""|all)
            mkdir -p "$(dirname "${VPD_STOPPED_FILE:-/tmp/vpn-director/stopped}")"
            printf '1\n' > "${VPD_STOPPED_FILE:-/tmp/vpn-director/stopped}"
            ;;
    esac
}

cmd_restart() {
    # Lock first for the same reason: cmd_stop and cmd_apply below would find
    # the modules already loaded and reuse the config read before the wait.
    _load_common
    acquire_lock "vpn-director"
    _load_modules
    case "$COMPONENT" in
        ""|all)
            tproxy_restart_process
            cmd_stop
            cmd_apply
            ;;
        tunnel)
            COMPONENT=tunnel cmd_stop
            COMPONENT=tunnel cmd_apply
            ;;
        xray|tproxy)
            tproxy_restart_process
            COMPONENT=xray cmd_stop
            COMPONENT=xray cmd_apply
            ;;
        *)
            echo "Unknown component: $COMPONENT" >&2
            exit 1
            ;;
    esac
}

cmd_update() {
    _load_common
    acquire_lock "vpn-director"
    _load_modules
    rm -f "${VPD_STOPPED_FILE:-/tmp/vpn-director/stopped}"

    # Wait for network if system just booted (before any downloads)
    _ipset_boot_wait

    local required_ipsets
    required_ipsets="$(tunnel_get_required_ipsets) $(tproxy_get_required_ipsets)"
    required_ipsets=$(echo $required_ipsets | xargs -n1 | sort -u | xargs)

    if [[ -n $required_ipsets ]]; then
        log "Updating ipsets: $required_ipsets"
        # Force update: download fresh data even if cache exists
        export IPSET_FORCE_UPDATE=1
        # shellcheck disable=SC2086
        _ensure_ipsets $required_ipsets
    fi

    # Xray first, for the reason spelled out in cmd_apply.
    tproxy_apply
    tunnel_apply

    log "Update complete"
}

# -------------------------------------------------------------------------------------------------
# cmd_platform - print the platform facts the daemons need, as one JSON document
# -------------------------------------------------------------------------------------------------
# Tunnels are listed without "main"; a tunnel whose interface or info lookup
# fails is omitted with a WARN so the rest of the document stays usable. The
# document is one line: the daemons read the command's combined output, WARN
# lines included, and take the last non-empty line as the document.
# -------------------------------------------------------------------------------------------------
cmd_platform() {
    _load_common

    local arch wan pw_file lan_json tunnels_json
    arch="$(uname -m)"
    wan="$(platform_wan_if)" || wan=""
    pw_file="$(platform_password_file)"
    lan_json="$(platform_lan_ifaces | jq -R . | jq -s -c .)"

    tunnels_json="[]"
    local id iface info type connected desc
    while IFS= read -r id; do
        [[ -n $id ]] || continue
        [[ $id == main ]] && continue
        if ! iface="$(platform_tunnel_iface "$id")"; then
            log -l WARN "platform: cannot map tunnel '$id' to an interface; omitted"
            continue
        fi
        if ! info="$(platform_tunnel_info "$id")"; then
            log -l WARN "platform: no info for tunnel '$id'; omitted"
            continue
        fi
        type="$(printf '%s\n' "$info" | sed -n 1p)"
        connected="$(printf '%s\n' "$info" | sed -n 2p)"
        desc="$(printf '%s\n' "$info" | sed -n 3p)"
        tunnels_json="$(jq -c --argjson list "$tunnels_json" \
            --arg id "$id" --arg iface "$iface" --arg type "$type" \
            --argjson connected "$([[ $connected == 1 ]] && echo true || echo false)" \
            --arg desc "$desc" \
            -n '$list + [{id:$id, iface:$iface, type:$type, connected:$connected, description:$desc}]')"
    done < <(platform_tunnels || true)

    jq -c -n \
        --arg platform "$(platform_name)" \
        --arg arch "$arch" \
        --arg password_file "$pw_file" \
        --argjson lan_ifaces "$lan_json" \
        --arg wan_if "$wan" \
        --argjson tunnels "$tunnels_json" \
        '{platform:$platform, arch:$arch, password_file:$password_file,
          lan_ifaces:$lan_ifaces, wan_if:$wan_if, tunnels:$tunnels}'
}

# -------------------------------------------------------------------------------------------------
# cmd_cron - schedule or drop the daily "update" job through the platform's cron
# -------------------------------------------------------------------------------------------------
cmd_cron() {
    _load_common
    case "$COMPONENT" in
        install)
            # update_ipsets is the job name of the versions before the unified CLI. It is dropped
            # on every install so a router upgraded from one of them does not keep two jobs.
            # `|| true`: the job usually does not exist, and a platform whose cron tool reports
            # that with an error must not abort the install.
            platform_cron_del update_ipsets || true
            # Bare, this call took errexit with it: a platform whose cron tool
            # is missing or refuses ended the CLI with that exit status and no
            # output at all. The platform says what is wrong with it; say that
            # the schedule is the thing that did not happen, and still fail.
            if ! platform_cron_add vpn_director_update "0 3 * * *" "$SCRIPT_DIR/vpn-director.sh update"; then
                log -l ERROR "Failed to schedule the daily ipset update"
                # The platform states what it needs; it does not log (the
                # contract in lib/platform.sh). A platform that needs nothing
                # installed says nothing, and the line above stands alone.
                local cron_req
                if cron_req="$(platform_cron_requirements)"; then
                    log -l ERROR "$cron_req"
                fi
                exit 1
            fi
            log "Scheduled daily ipset update"
            ;;
        remove)
            # Removing a job that is not there is not an error, and S99 stop must not abort here.
            platform_cron_del vpn_director_update || true
            # The legacy job as well, for the reason spelled out in install.
            platform_cron_del update_ipsets || true
            log "Removed daily ipset update"
            ;;
        *)
            echo "Usage: vpn-director cron install|remove" >&2
            exit 1
            ;;
    esac
}

###################################################################################################
# Main
###################################################################################################

# Exit early if sourced for testing
[[ $_SOURCE_ONLY -eq 1 ]] && return 0 2>/dev/null || true

case "$COMMAND" in
    help|--help|-h)
        show_help
        ;;
    status)
        cmd_status
        ;;
    apply)
        cmd_apply
        ;;
    stop)
        cmd_stop
        ;;
    restart)
        cmd_restart
        ;;
    update)
        cmd_update
        ;;
    platform)
        cmd_platform
        ;;
    cron)
        cmd_cron
        ;;
    *)
        echo "Unknown command: $COMMAND" >&2
        echo "Run 'vpn-director --help' for usage" >&2
        exit 1
        ;;
esac
