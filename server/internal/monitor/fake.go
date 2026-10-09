package monitor

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// FakeLauncher stands in for Xray in dev mode: every endpoint answers the same
// way every time, by the first byte of its key - most alive with a latency of
// 50-560 ms, some dead, a few refused.
type FakeLauncher struct{}

// Ready is always nil.
func (FakeLauncher) Ready() error { return nil }

// Start refuses the first endpoint whose key starts below 0x0d.
func (FakeLauncher) Start(_ context.Context, eps []Endpoint) (Session, error) {
	for _, ep := range eps {
		if fakeByte(ep.Key) < 0x0d {
			return nil, &RefusedError{Key: ep.Key, Reason: "fake: Xray refused the outbound"}
		}
	}
	return &fakeSession{exited: make(chan struct{})}, nil
}

// Test is always nil.
func (FakeLauncher) Test(context.Context, []Endpoint) error { return nil }

type fakeSession struct {
	exited chan struct{}
	once   sync.Once
}

// Check answers after the key's latency; a key below 0x33 fails.
func (s *fakeSession) Check(ctx context.Context, key string) (time.Duration, error) {
	b := fakeByte(key)
	latency := time.Duration(50+2*int(b)) * time.Millisecond
	select {
	case <-time.After(latency):
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	if b < 0x33 {
		return 0, errors.New("fake: connection closed")
	}
	return latency, nil
}

func (s *fakeSession) Exited() <-chan struct{} { return s.exited }

func (s *fakeSession) Stop() { s.once.Do(func() { close(s.exited) }) }

func fakeByte(key string) byte {
	if len(key) < 2 {
		return 0xff
	}
	b, err := hex.DecodeString(key[:2])
	if err != nil {
		return 0xff
	}
	return b[0]
}
