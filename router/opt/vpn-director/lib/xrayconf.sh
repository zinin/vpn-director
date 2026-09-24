#!/usr/bin/env bash
###############################################################################
# lib/xrayconf.sh - Build Xray proxy-out outbound + config.json from a server
# JSON object (as a subscription file stores it), and have Xray test the
# result.
# Pure jq transforms; no side effects on source. Used by configure.sh;
# unit-tested via bats.
# Mirrors the Go generator in server/internal/service/xray.go — keep both in sync.
###############################################################################

# xrayconf_build_outbound: reads one server JSON object on stdin,
# prints the proxy-out outbound JSON object on stdout. A server an import
# stored with its outbound gets that outbound, tagged: the import
# (lib/subscription.sh) checked it. It fails (rc=1) all the same when an xhttp
# downloadSettings in it names no address, as it does in serverOutbound in
# service/xray.go: Xray leaves that destination nil and panics on the first
# dial through it, and "xray run -test" never dials, so the test cannot catch
# it. Both names are looked up as Xray reads them: it loads its config with
# Go's encoding/json, which matches a key to a field whatever its case, so a
# "DownloadSettings" is the download stream to it and an "Address" its
# address - and a record stored before the importers refused such spellings
# can hold either. fold is the one in lib/subscription.sh, the long s and the
# Kelvin sign included.
# It fails (rc=1) as well on an xhttp stream Xray would dial packet-up with a
# scMaxEachPostBytes of 8192 or less, as smallPacketUpPosts in service/xray.go
# does: splithttp's dialer panics on that range ("scMaxEachPostBytes should
# be bigger than 8192" in 26.2.6) the first time it dials packet-up, after
# "xray run -test" has passed the config. When xhttpSettings (or
# splithttpSettings, which it takes precedence over) holds an extra, Xray
# builds the stream from extra alone, with the outer host, path and mode
# copied onto it: the range counts from extra, and the mode is the outer one.
# An empty or "auto" mode is packet-up unless the stream is REALITY;
# stream-up and stream-one never reach the panic, and a range whose upper end
# is 0 gives way to Xray's default. The keys are read folded too; where one
# key is spelled several ways, any spelling that would panic is enough.
# An outbound that is there but is no object - a string, a null - fails
# (rc=1), as serverOutbound rejects it through DecodeOutbound: it is no legacy
# record.
# A record from before outbounds were stored is built from its flat VLESS
# fields, and fails (rc=1) on an unsupported network (non-tcp) or security
# (not tls/reality) instead of emitting a silently-broken outbound.
# Self-contained: pure jq; errors to stderr.
xrayconf_build_outbound() {
    local server_json net sec
    local jq_fold='def fold: explode | map(if . == 383 then 115 elif . == 8490 then 107 elif . >= 65 and . <= 90 then . + 32 else . end) | implode;'
    server_json="$(cat)"
    if printf '%s' "$server_json" | jq -e 'type == "object" and (.outbound | type) == "object"' >/dev/null 2>&1; then
        if printf '%s' "$server_json" | jq -e "$jq_fold"'
                [.outbound | .. | objects | to_entries[] | select(.key | fold == "downloadsettings") | .value | objects
                 | select([to_entries[] | select(.key | fold == "address") | .value | strings | select(. != "")] | length == 0)]
                | length > 0' >/dev/null 2>&1; then
            printf 'xrayconf: stored outbound: xhttp downloadSettings without an address\n' >&2
            return 1
        fi
        # at($n): every value an object holds under a key Xray reads as $n;
        # strs($n): those values as strings, "" for one that is not, [""] when
        # there is none; atoi: strconv.Atoi, a sign and digits; xray_range:
        # Xray's Int32Range, [from, to] ordered, nothing for a shape its Build
        # refuses.
        if printf '%s' "$server_json" | jq -e "$jq_fold"'
                def at($n): objects | to_entries[] | select(.key | fold == $n) | .value;
                def strs($n): [at($n) | if type == "string" then . else "" end] | if length == 0 then [""] else . end;
                def atoi: explode | (if .[0] == 43 or .[0] == 45 then .[1:] else . end) as $d
                          | if ($d | length) > 0 and ($d | all(. >= 48 and . <= 57))
                            then (reduce ($d[] - 48) as $x (0; . * 10 + $x)) * (if .[0] == 45 then -1 else 1 end)
                            else empty end;
                def xray_range: if type == "number" then select(. == floor) | [., .]
                                elif type == "string" then
                                  . as $s
                                  | if $s == "" then [0, 0]
                                    elif ([$s | atoi] | length) == 1 then [($s | atoi), ($s | atoi)]
                                    else ($s | split("-")) as $p
                                         | (if ($s | startswith("-")) then ["-" + $p[1], ($p[2:] | join("-"))]
                                            else [$p[0], ($p[1:] | join("-"))] end)
                                         | [(.[0] | atoi), (.[1] | atoi)] | select(length == 2)
                                    end
                                else empty end
                                | sort;
                [.outbound | at("streamsettings") | objects as $ss
                 | select($ss | strs("network") | any(ascii_downcase | . == "xhttp" or . == "splithttp"))
                 | ($ss | strs("security") | all(ascii_downcase == "reality")) as $reality
                 | ([$ss | at("xhttpsettings") | objects] | if length > 0 then . else [$ss | at("splithttpsettings") | objects] end)[] as $x
                 | select($x | strs("mode") | any(. == "packet-up" or ((. == "" or . == "auto") and ($reality | not))))
                 | ([$x | at("extra")] | if length > 0 then map(objects) else [$x] end)[]
                 | at("scmaxeachpostbytes") | xray_range
                 | select(.[1] != 0 and .[0] <= 8192)]
                | length > 0' >/dev/null 2>&1; then
            printf 'xrayconf: stored outbound: xhttp scMaxEachPostBytes of 8192 or less in packet-up mode\n' >&2
            return 1
        fi
        printf '%s' "$server_json" | jq '.outbound + {tag: "proxy-out"}'
        return
    fi
    if printf '%s' "$server_json" | jq -e 'type == "object" and has("outbound")' >/dev/null 2>&1; then
        printf 'xrayconf: stored outbound is not an object\n' >&2
        return 1
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
    # kind the test runs unbounded, as it did before. Xray prints its version
    # banner before it loads the config, so a killed xray has said something
    # too: the timeout's exit code - 124 or 143, depending on which timeout
    # ran - not the output, tells a timeout from a rejection.
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
        if (( ${#bound[@]} > 0 )) && [[ $rc == 124 || $rc == 143 ]]; then
            printf 'xrayconf: xray config test timed out after 15s\n' >&2
            return 1
        fi
        local detail
        detail=$(printf '%s\n' "$out" | awk '
            NF { line[++n] = $0 }
            END { for (i = (n > 3 ? n - 2 : 1); i <= n; i++) printf "%s%s", line[i], (i < n ? "; " : "") }')
        printf 'xrayconf: xray rejected the config: %s\n' "${detail:-no output, exit $rc}" >&2
        return 1
    fi
}
