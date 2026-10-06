package watchcompat

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

type capabilityErrorWriter struct {
	err error
}

func (w capabilityErrorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestCapabilities_JSONContract(t *testing.T) {
	var out bytes.Buffer
	if err := WriteCapabilities(&out); err != nil {
		t.Fatal(err)
	}
	if got := string(bytes.TrimSpace(out.Bytes())); got != `{"protocol_version":1,"watch_owner":"watchd"}` {
		t.Fatalf("capabilities %q, want the exact version-1 watchd contract", got)
	}
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	dec.DisallowUnknownFields()
	var caps Capabilities
	if err := dec.Decode(&caps); err != nil {
		t.Fatal(err)
	}
	if caps.ProtocolVersion != 1 || caps.WatchOwner != "watchd" {
		t.Fatalf("decoded capabilities %+v, want protocol 1 owned by watchd", caps)
	}
	var extra interface{}
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("after capabilities: %v, want EOF", err)
	}
	refused := errors.New("writer is closed")
	if err := WriteCapabilities(capabilityErrorWriter{err: refused}); !errors.Is(err, refused) {
		t.Fatalf("writer error %v, want the write failure without panic or success", err)
	}
}
