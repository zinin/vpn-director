package webapi

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// handleLogs returns a handler that reads log files listed in deps.LogPaths.
// Query params: source (one of the map keys), lines (default 50, max 500).
// If source is specified, returns that single log. Otherwise returns all.
func handleLogs(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		extendWriteDeadline(w, logsDeadline(deps))

		source := r.URL.Query().Get("source")
		linesStr := r.URL.Query().Get("lines")

		lines := 50
		if linesStr != "" {
			n, err := strconv.Atoi(linesStr)
			if err != nil || n < 1 {
				jsonError(w, http.StatusBadRequest, "lines must be a positive integer")
				return
			}
			if n > 500 {
				n = 500
			}
			lines = n
		}

		if source != "" {
			path, ok := deps.LogPaths[source]
			if !ok {
				jsonError(w, http.StatusBadRequest,
					"unknown source: valid values are "+strings.Join(logSourceNames(deps.LogPaths), ", "))
				return
			}

			output, err := deps.Logs.Read(path, lines)
			if err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to read log file")
				return
			}

			jsonOK(w, map[string]string{"output": output, "source": source})
			return
		}

		// No source specified: return all logs.
		result := make(map[string]string, len(deps.LogPaths))
		for name, path := range deps.LogPaths {
			output, err := deps.Logs.Read(path, lines)
			if err != nil {
				result[name] = "error: " + err.Error()
			} else {
				result[name] = output
			}
		}

		jsonOK(w, result)
	}
}

// logSourceNames returns the log source names in sorted order for messages.
func logSourceNames(logPaths map[string]string) []string {
	names := make([]string, 0, len(logPaths))
	for name := range logPaths {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// handleConfig returns a handler that returns the VPN Director configuration
// with sensitive fields redacted.
func handleConfig(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		cfg, err := deps.Config.LoadVPNConfig()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to load configuration")
			return
		}

		// Make a shallow copy to avoid mutating the original.
		redacted := *cfg
		redacted.WebUI.JWTSecret = ""
		redacted.Xray.SubscriptionURL = ""

		jsonOK(w, &redacted)
	}
}
