// Package watchdapi is the contract between vpn-director-watchd and the
// daemons that ask it: the state of the server monitor, served over a unix
// socket, a client for it, and Health, which folds the states of a server's
// endpoints into the server's status.
package watchdapi

import (
	"errors"
	"time"
)

// Status is what the monitor knows of one endpoint.
type Status string

const (
	// StatusUnknown is an endpoint not checked yet.
	StatusUnknown Status = "unknown"
	// StatusAlive is an endpoint whose last check got its answer.
	StatusAlive Status = "alive"
	// StatusDead is an endpoint whose last check failed, after its retry.
	StatusDead Status = "dead"
	// StatusRejected is an endpoint the prober does not hold: the generator or
	// Xray refused its outbound, or it crashed Xray.
	StatusRejected Status = "rejected"
)

// State is what the monitor as a whole is doing.
type State string

const (
	StateOK          State = "ok"
	StateStopped     State = "stopped"      // VPN Director is stopped
	StateDisabled    State = "disabled"     // monitor.enabled is false
	StateNoXray      State = "no_xray"      // no xray on PATH
	StateWANDown     State = "wan_down"     // no control address answers
	StateProberError State = "prober_error" // the prober cannot start; Message says why
	// StateNotRunning is no answer of the daemon's: the Web UI and the bot
	// report it when the socket does not answer.
	StateNotRunning State = "not_running"
)

// EndpointState is the monitor's record of one endpoint.
type EndpointState struct {
	Status Status `json:"status"`
	// LatencyMS is the time to the response headers of the last success.
	LatencyMS int64     `json:"latency_ms"`
	CheckedAt time.Time `json:"checked_at"`
	NextAt    time.Time `json:"next_at"`
	// Since is when the endpoint took its status.
	Since time.Time `json:"since"`
	// Fails counts failed checks in a row.
	Fails int `json:"fails"`
	// Error is why the last check failed, or why the endpoint is rejected.
	Error string `json:"error,omitempty"`
}

// Snapshot is GET /v1/monitor: the whole state, endpoints by key.
type Snapshot struct {
	State           State                    `json:"state"`
	Message         string                   `json:"message,omitempty"`
	IntervalSeconds int                      `json:"interval_seconds"`
	LagSeconds      int                      `json:"lag_seconds"`
	UpdatedAt       time.Time                `json:"updated_at"`
	Endpoints       map[string]EndpointState `json:"endpoints"`
}

// ErrNotActive is a check asked of a monitor that is stopped or disabled.
var ErrNotActive = errors.New("the monitor is stopped or disabled")
