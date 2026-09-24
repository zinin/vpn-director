package vpnconfig

import (
	"testing"
)

func TestRecordActiveServer_CountsWrites(t *testing.T) {
	s := Server{Name: "Oslo", Address: "oslo.example", Port: 443}
	first := RecordActiveServer(nil, s)
	if first.Seq != 1 {
		t.Fatalf("Seq %d, want 1", first.Seq)
	}
	again := RecordActiveServer(first, s)
	if again.Seq != 2 {
		t.Fatalf("Seq %d; re-selecting the running server is still a write", again.Seq)
	}
	if again.Name != "Oslo" || again.Address != "oslo.example" || again.Port != 443 {
		t.Fatalf("record %+v", again)
	}
}
