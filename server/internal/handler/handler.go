// internal/handler/handler.go
package handler

import (
	"errors"
	"fmt"

	"github.com/zinin/vpn-director/server/internal/paths"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/telegram"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// Deps holds dependencies for all handlers
type Deps struct {
	Sender      telegram.MessageSender
	Config      service.ConfigStore   // interface from service/
	VPN         service.VPNDirector   // interface from service/
	Xray        service.XrayGenerator // interface from service/
	Network     service.NetworkInfo   // interface from service/
	Logs        service.LogReader     // interface from service/
	Paths       paths.Paths
	Version     string // Clean version for semver parsing (v1.2.0)
	VersionFull string // Full git describe output (v1.2.0-5-gabc1234)
	Commit      string // Git commit hash
	BuildDate   string // Build date
	DevMode     bool   // Development mode flag
	// TelegramPath is the bot's current Telegram API path, nil when there is
	// none: dev mode, and the Web UI, which runs no path manager.
	TelegramPath func() string
	// Monitor is vpn-director-watchd's server monitor; nil means no marks.
	Monitor watchdapi.API
	// Watch is the daemon's independent subscription automation status.
	Watch watchdapi.WatchAPI
}

// configUpdateError phrases an UpdateVPNConfig failure the way the bot has
// always reported the two halves of a save: read problems and write problems.
func configUpdateError(err error) string {
	if errors.Is(err, service.ErrConfigLoad) {
		return fmt.Sprintf("Config load error: %v", err)
	}
	return fmt.Sprintf("Save error: %v", err)
}
