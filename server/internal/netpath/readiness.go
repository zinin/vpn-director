package netpath

import (
	"os"
	"strings"
)

type Readiness struct {
	TablesPath   string
	FailoverPath string
	TPROXYPath   string
	StoppedPath  string
}

// Tables alone are not enough: the marker confirms the route and rule were installed.
func (r Readiness) FallbackReady(id string) bool {
	if id == "" {
		return false
	}
	if _, ok := LoadTunnelIndexes(r.TablesPath)[id]; !ok {
		return false
	}
	b, err := os.ReadFile(r.FailoverPath)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == id
}

func (r Readiness) TPROXYReady() bool {
	b, err := os.ReadFile(r.TPROXYPath)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) != ""
}

func (r Readiness) Stopped() bool {
	_, err := os.Stat(r.StoppedPath)
	return err == nil
}
