// Package subscription reads a VPN subscription — share links, base64 or
// plain, or Xray JSON — into servers that carry a ready Xray outbound.
//
// import_server_list.sh does the same in lib/subscription.sh, and both answer
// to the cases in testdata/subscription at the repository root: an entry
// either side reads differently fails one of the two test suites.
package subscription

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// Reasons an entry is skipped.
const (
	ReasonUnsupported = "unsupported"
	ReasonComposite   = "composite"
	ReasonInvalid     = "invalid"
	ReasonPlaceholder = "placeholder"
)

// Errors for a body Decode cannot read at all.
var (
	ErrEmpty        = errors.New("empty subscription")
	ErrInvalidJSON  = errors.New("invalid JSON subscription")
	ErrUnrecognized = errors.New("unrecognized subscription format")
)

// Skip is an entry left out of the result.
type Skip struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// Result is a decoded subscription. Total counts its entries - link lines or
// JSON configs - and every one of them is either in Servers, with IPs still
// empty, or in Skipped.
type Result struct {
	Total   int
	Servers []vpnconfig.Server
	Skipped []Skip
}

// entry is one converted server, before the placeholder test and naming.
type entry struct {
	name     string // cleaned; empty when the source has none
	address  string // brackets removed
	port     int
	outbound map[string]interface{}
}

// skipError says why an entry is skipped.
type skipError struct {
	reason string
	detail string
}

func (e *skipError) Error() string { return e.reason + ": " + e.detail }

func unsupported(detail string) error { return &skipError{ReasonUnsupported, detail} }
func invalid(detail string) error     { return &skipError{ReasonInvalid, detail} }
func composite(detail string) error   { return &skipError{ReasonComposite, detail} }

// add records the entry at position n (1-based): a server, or a skip when
// err is set or the address is a placeholder. The placeholder test comes last,
// on an entry that passed every other check (spec 6.6).
func (r *Result) add(n int, e entry, err error) {
	if err == nil && isPlaceholder(e.address) {
		err = &skipError{ReasonPlaceholder, e.address}
	}
	if err != nil {
		var se *skipError
		if !errors.As(err, &se) {
			se = &skipError{ReasonInvalid, err.Error()}
		}
		name := e.name
		if name == "" {
			name = "#" + strconv.Itoa(n)
		}
		r.Skipped = append(r.Skipped, Skip{Name: name, Reason: se.reason, Detail: se.detail})
		return
	}
	name := e.name
	if name == "" {
		name = e.address
	}
	raw, _ := json.Marshal(e.outbound)
	r.Servers = append(r.Servers, vpnconfig.Server{
		Name:     name,
		Address:  e.address,
		Port:     e.port,
		Outbound: raw,
	})
}
