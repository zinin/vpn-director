#!/usr/bin/env bash
###############################################################################
# lib/xrayconf.sh - Build Xray proxy-out outbound + config.json from a server
# JSON object (as stored in servers.json), and have Xray test the result.
# Pure jq transforms; no side effects on source. Used by configure.sh;
# unit-tested via bats.
# Mirrors the Go generator in server/internal/service/xray.go — keep both in sync.
###############################################################################

# xrayconf_build_outbound: reads one server JSON object on stdin,
# prints the proxy-out outbound JSON object on stdout. A server an import
# stored with its outbound gets that outbound, tagged: the import
# (lib/subscription.sh) checked it. A record from before outbounds were stored
# is built from its flat VLESS fields, and fails (rc=1) on an unsupported
# network (non-tcp) or security (not tls/reality) instead of emitting a
# silently-broken outbound. Self-contained: pure jq; errors to stderr.
xrayconf_build_outbound() {
    local server_json net sec
    server_json="$(cat)"
    if printf '%s' "$server_json" | jq -e 'type == "object" and (.outbound | type) == "object"' >/dev/null 2>&1; then
        printf '%s' "$server_json" | jq '.outbound + {tag: "proxy-out"}'
        return
    fi
    if ! printf '%s' "$server_json" | jq -e 'type == "object" and (.address // "") != "" and (.uuid // "") != ""' >/dev/null 2>&1; then
        printf 'xrayconf: invalid/empty server JSON (need object with address and uuid)\n' >&2
        return 1
    fi
    net="$(printf '%s' "$server_json" | jq -r '.network // ""')"
    sec="$(printf '%s' "$server_json" | jq -r '.security // ""')"
    case "$net" in ""|tcp) ;; *) printf 'xrayconf: unsupported network "%s" (only tcp)\n' "$net" >&2; return 1 ;; esac
    case "$sec" in ""|tls|reality) ;; *) printf 'xrayconf: unsupported security "%s" (only tls/reality)\n' "$sec" >&2; return 1 ;; esac
    # REALITY needs these to handshake; fail loudly instead of emitting an
    # incomplete realitySettings. shortId is optional (Xray allows empty).
    if [[ "$sec" == "reality" ]]; then
        local _missing
        _missing="$(printf '%s' "$server_json" | jq -r '
            [ if (.public_key // "") == "" then "public_key" else empty end,
              if (.sni // "") == "" then "sni" else empty end,
              if (.fingerprint // "") == "" then "fingerprint" else empty end ] | join(" ")')"
        if [[ -n "$_missing" ]]; then
            printf 'xrayconf: reality requires non-empty: %s\n' "$_missing" >&2
            return 1
        fi
    fi
    printf '%s' "$server_json" | jq '
      def trimempty: with_entries(select(.value != null and .value != "" and .value != []));
      ((.network // "") | if . == "" then "tcp" else . end) as $net
      | (.security // "") as $sec
      | {
          protocol: "vless",
          settings: { vnext: [ {
            address: .address,
            port: .port,
            users: [ ( { id: .uuid, encryption: "none" }
                       + (if (.flow // "") != "" and (.security // "") != "" then { flow: .flow } else {} end) ) ]
          } ] },
          streamSettings: (
            if $sec == "reality" then
              { network: $net, security: "reality",
                realitySettings: ( { serverName: .sni, fingerprint: .fingerprint,
                                     publicKey: .public_key, shortId: .short_id } | trimempty ) }
            elif $sec == "tls" then
              ( (if (.sni // "") != "" then .sni else .address end) as $sn
                | { network: $net, security: "tls",
                    tlsSettings: ( { serverName: $sn, fingerprint: .fingerprint, alpn: .alpn } | trimempty ) } )
            else
              # Legacy: the sni when the server names one, as the Go generator
              # does - the subscription watch dials a resolved IP and keeps the
              # hostname there.
              ( (if (.sni // "") != "" then .sni else .address end) as $sn
                | { network: $net, security: "tls",
                    tlsSettings: { alpn: ["h2"], serverName: $sn } } )
            end
          ),
          tag: "proxy-out"
        }
    '
}

# xrayconf_generate <template_path> [tproxy_port] [socks_port]: reads one
# server JSON object on stdin, prints the full config.json (template with
# outbounds replaced) on stdout. The ports are optional; given, they replace
# the template's inbound ports, because the dokodemo-door port has to match
# advanced.xray.tproxy_port - that is the port the TPROXY rules send traffic
# to, and a mismatch leaves it with no listener.
xrayconf_generate() {
    local template="$1"
    local tproxy_port="${2:-}"
    local socks_port="${3:-}"
    local server_json outbound
    server_json="$(cat)"
    outbound="$(printf '%s' "$server_json" | xrayconf_build_outbound)" || return 1
    jq --argjson ob "$outbound" --arg tp "$tproxy_port" --arg sp "$socks_port" \
        '.outbounds = [$ob]
         | if $tp == "" then . else
             (.inbounds[] | select(.tag == "tproxy-in") | .port) = ($tp | tonumber)
           end
         | if $sp == "" then . else
             (.inbounds[] | select(.tag == "socks-in") | .port) = ($sp | tonumber)
           end' \
        "$template"
}

# xrayconf_validate <file>: has Xray load a generated config without starting
# it, as XrayService.GenerateConfig does (service/xray.go). An outbound can come
# verbatim from a subscription and name a protocol the installed Xray lacks or
# a key it refuses - allowInsecure stops Xray from loading any config since
# 2026-06-01 - and a config Xray rejects takes every Xray client offline at the
# next restart. S24xray runs "xray run -confdir", which loads only *.json, so a
# mktemp name needs -format json. Without an xray there is nothing to test
# with: that is said on stderr, and the config passes.
xrayconf_validate() {
    local file="$1" xray out rc=0
    xray=$(type -P xray 2>/dev/null) || xray=""
    if [[ -z $xray && -x /opt/sbin/xray ]]; then
        xray=/opt/sbin/xray
    fi
    if [[ -z $xray ]]; then
        printf 'xrayconf: xray not found; %s was not tested\n' "$file" >&2
        return 0
    fi
    # The caller holds the config lock while this runs, and every other writer
    # waits 30 s for that lock, so the test is bounded at half of it: an xray
    # that never answers then lets the others take their turn instead of using
    # up their whole wait. XrayService.GenerateConfig bounds the same test the
    # same way (xrayTestTimeout in service/xray.go). Without a timeout of any
    # kind the test runs unbounded, as it did before. A killed xray says nothing, and
    # its exit code is 124 or 143 depending on which timeout ran, so the message
    # names the code rather than reading it.
    #
    # Which form a router's timeout takes cannot be read off its name: coreutils
    # and busybox from 1.30 take "timeout SECS PROG", busybox before it - Merlin
    # ships 1.25 - only "timeout -t SECS PROG", and there the seconds would be
    # taken for the program, failing every config with exit 127. So the form is
    # probed, at the cost of one run of true.
    local -a test_cmd=("$xray" run -test -format json -c "$file") bound=()
    if type -P timeout >/dev/null 2>&1; then
        if timeout 1 true >/dev/null 2>&1; then
            bound=(timeout 15)
        elif timeout -t 1 true >/dev/null 2>&1; then
            bound=(timeout -t 15)
        fi
    fi
    test_cmd=("${bound[@]}" "${test_cmd[@]}")
    out=$("${test_cmd[@]}" 2>&1) || rc=$?
    if (( rc != 0 )); then
        local detail
        detail=$(printf '%s\n' "$out" | awk '
            NF { line[++n] = $0 }
            END { for (i = (n > 3 ? n - 2 : 1); i <= n; i++) printf "%s%s", line[i], (i < n ? "; " : "") }')
        printf 'xrayconf: xray rejected the config: %s\n' "${detail:-no output, exit $rc}" >&2
        return 1
    fi
}
