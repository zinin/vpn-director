package subwatch

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// orderSub is a subscription whose servers are named <letter>1..<letter>n,
// the letter being the id's first character upper-cased.
func orderSub(id string, n int) vpnconfig.Subscription {
	letter := strings.ToUpper(id[:1])
	s := vpnconfig.Subscription{ID: id, Name: letter}
	for i := 1; i <= n; i++ {
		s.Servers = append(s.Servers, vpnconfig.Server{Name: fmt.Sprintf("%s%d", letter, i), Address: fmt.Sprintf("%s%d.example", id[:1], i), Port: 443})
	}
	return s
}

func chosenOf(sub, name string) *vpnconfig.ActiveServer {
	return &vpnconfig.ActiveServer{Subscription: sub, Name: name, Address: strings.ToLower(name) + ".example", Port: 443}
}

func orderNames(subs []vpnconfig.Subscription, chosen *vpnconfig.ActiveServer) ([]string, bool) {
	order, first := walkOrder(subs, chosen)
	var out []string
	for _, s := range order {
		out = append(out, s.Name)
	}
	return out, first
}

// spec 5.3: the chosen server, two more of its subscription, then one server
// of each subscription in turn, starting after the chosen server's.
func TestWalkOrder_TheChosenServerTwoNeighboursThenOneOfEach(t *testing.T) {
	subs := []vpnconfig.Subscription{orderSub("aaaaaaaa", 7), orderSub("bbbbbbbb", 4)}

	got, first := orderNames(subs, chosenOf("aaaaaaaa", "A5"))

	want := []string{"A5", "A1", "A2", "B1", "A3", "B2", "A4", "B3", "A6", "B4", "A7"}
	if !reflect.DeepEqual(got, want) || !first {
		t.Fatalf("got %v (first %v), want %v", got, first, want)
	}
}

func TestWalkOrder_TheRotationStartsAfterTheChosenSubscription(t *testing.T) {
	subs := []vpnconfig.Subscription{orderSub("aaaaaaaa", 4), orderSub("bbbbbbbb", 4), orderSub("cccccccc", 4)}

	got, _ := orderNames(subs, chosenOf("bbbbbbbb", "B1"))

	want := []string{"B1", "B2", "B3", "C1", "A1", "B4", "C2", "A2", "C3", "A3", "C4", "A4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// With one subscription the order is today's pickOrder: the chosen server,
// then the rest of the list.
func TestWalkOrder_WithOneSubscriptionItIsPickOrder(t *testing.T) {
	subs := []vpnconfig.Subscription{orderSub("aaaaaaaa", 5)}

	got, first := orderNames(subs, chosenOf("aaaaaaaa", "A4"))

	if want := []string{"A4", "A1", "A2", "A3", "A5"}; !reflect.DeepEqual(got, want) || !first {
		t.Fatalf("got %v (first %v), want %v", got, first, want)
	}
}

// Nothing chosen, a record from before subscriptions, and a subscription
// deleted since: the turns start at the first subscription.
func TestWalkOrder_WithoutAChosenSubscriptionTheTurnsStartAtTheFirst(t *testing.T) {
	subs := []vpnconfig.Subscription{orderSub("aaaaaaaa", 2), orderSub("bbbbbbbb", 2)}
	for _, chosen := range []*vpnconfig.ActiveServer{nil, chosenOf("", "A1"), chosenOf("dddddddd", "A1")} {
		got, first := orderNames(subs, chosen)
		if want := []string{"A1", "B1", "A2", "B2"}; !reflect.DeepEqual(got, want) || first {
			t.Errorf("chosen %+v: got %v (first %v), want %v", chosen, got, first, want)
		}
	}
}

// The chosen server left its list, but its subscription is there: the walk
// still gives that provider its OwnFirst servers first.
func TestWalkOrder_AChosenServerThatIsGoneKeepsItsSubscriptionFirst(t *testing.T) {
	subs := []vpnconfig.Subscription{orderSub("aaaaaaaa", 5), orderSub("bbbbbbbb", 2)}

	got, first := orderNames(subs, chosenOf("aaaaaaaa", "A9"))

	if want := []string{"A1", "A2", "A3", "B1", "A4", "B2", "A5"}; !reflect.DeepEqual(got, want) || first {
		t.Fatalf("got %v (first %v), want %v", got, first, want)
	}
}

// Two subscriptions name a server Germany-1. A choice made in Beta, at an
// address its list has since rotated, is Beta's Germany-1 - never Alpha's.
func TestWalkOrder_TheSameNameInAnotherSubscriptionIsNotTheChoice(t *testing.T) {
	a := vpnconfig.Subscription{ID: "aaaaaaaa", Name: "Alpha", Servers: []vpnconfig.Server{
		{Name: "Oslo", Address: "oslo.example", Port: 443},
		{Name: "Germany-1", Address: "de-a.example", Port: 443},
	}}
	b := vpnconfig.Subscription{ID: "bbbbbbbb", Name: "Beta", Servers: []vpnconfig.Server{{Name: "Germany-1", Address: "de-b.example", Port: 443}}}
	chosen := &vpnconfig.ActiveServer{Subscription: "bbbbbbbb", Name: "Germany-1", Address: "de-rotated.example", Port: 443}

	order, first := walkOrder([]vpnconfig.Subscription{a, b}, chosen)

	if !first || order[0].Subscription != "bbbbbbbb" || order[0].Address != "de-b.example" {
		t.Fatalf("first %v, order[0] %+v", first, order[0])
	}
}

func TestWalkOrder_MarksEachServerWithItsSubscription(t *testing.T) {
	subs := []vpnconfig.Subscription{orderSub("aaaaaaaa", 2), orderSub("bbbbbbbb", 1)}

	order, _ := walkOrder(subs, nil)

	for _, s := range order {
		if s.Subscription != strings.ToLower(s.Name[:1])+strings.Repeat(strings.ToLower(s.Name[:1]), 7) {
			t.Fatalf("server %s carries %q", s.Name, s.Subscription)
		}
	}
	if subs[0].Servers[0].Subscription != "" {
		t.Fatal("walkOrder changed the subscriptions it read")
	}
}

func TestDialKey_OneEndpointUnderTwoNamesIsOneServer(t *testing.T) {
	ob := json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"de.example","port":443,"users":[{"id":"u","encryption":"none"}]}]},"streamSettings":{"network":"tcp","security":"tls"}}`)
	a := vpnconfig.Server{Name: "Germany-1", Address: "de.example", Port: 443, IPs: []string{"192.0.2.1"}, Outbound: ob}
	b := a
	b.Name = "Germany-2"
	c := a
	c.IPs = []string{"192.0.2.2"}

	if dialKey(a) != dialKey(b) {
		t.Fatal("two names on one endpoint dial differently")
	}
	if dialKey(a) == dialKey(c) {
		t.Fatal("two addresses dial alike")
	}
	if dialKey(vpnconfig.Server{Name: "Legacy", Address: "l.example", Port: 443}) != "" {
		t.Fatal("a record without an outbound has a key")
	}
}
