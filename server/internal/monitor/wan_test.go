package monitor

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func TestWANUp_AnyControlAcceptingIsUp(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	closed, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closed.Addr().(*net.TCPAddr).Port
	closed.Close()

	up := []vpnconfig.Server{{Address: "127.0.0.1", Port: closedPort}, {Address: "127.0.0.1", Port: port}}
	if !WANUp(context.Background(), up, time.Second) {
		t.Fatal("one control accepts, yet the WAN reads down")
	}
	down := []vpnconfig.Server{{Address: "127.0.0.1", Port: closedPort}}
	if WANUp(context.Background(), down, time.Second) {
		t.Fatal("no control accepts, yet the WAN reads up: " + strconv.Itoa(closedPort))
	}
}
