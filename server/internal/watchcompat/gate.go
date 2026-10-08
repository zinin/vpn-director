package watchcompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

var ErrIncompatible = errors.New("bot compatibility is unconfirmed")

const (
	capabilityTimeout = 2 * time.Second
	capabilityLimit   = 4096
)

type executableIdentity struct {
	device uint64
	inode  uint64
	size   int64
	mtime  int64
}

type executableTarget struct {
	path     string
	identity executableIdentity
	pid      int
	start    uint64
}

type Gate struct {
	BotPath  string
	ProcRoot string

	mu       sync.Mutex
	verified []executableTarget
	cache    map[executableTarget]struct{}
	proc     func(context.Context, string, string) ([]executableTarget, error)
	exec     func(context.Context, string) ([]byte, error)
}

func (g *Gate) Check(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	deny := func() error {
		g.cache = nil
		g.verified = nil
		return ErrIncompatible
	}
	// A caller that went away is no evidence against what was verified: the
	// refusal keeps the cache.
	if ctx.Err() != nil {
		return ErrIncompatible
	}
	if !filepath.IsAbs(g.BotPath) {
		return deny()
	}
	before, err := g.targets(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ErrIncompatible
		}
		return deny()
	}
	if !sameTargets(before, g.verified) {
		g.cache = nil
	}
	run := g.exec
	if run == nil {
		run = readCapabilities
	}
	next := make(map[executableTarget]struct{}, len(before))
	for _, target := range before {
		if ctx.Err() != nil {
			return ErrIncompatible
		}
		if _, ok := g.cache[target]; !ok {
			body, err := run(ctx, target.path)
			if err != nil || !compatibleCapabilities(body) {
				if ctx.Err() != nil {
					return ErrIncompatible
				}
				return deny()
			}
		}
		next[target] = struct{}{}
	}
	// Executing capabilities must not bless a replaced file or a reused PID. A
	// process that exited meanwhile adds no unverified code.
	after, err := g.targets(ctx)
	if ctx.Err() != nil {
		return ErrIncompatible
	}
	if err != nil {
		return deny()
	}
	cache := make(map[executableTarget]struct{}, len(after))
	for _, target := range after {
		if _, ok := next[target]; !ok {
			return deny()
		}
		cache[target] = struct{}{}
	}
	g.cache = cache
	g.verified = after
	return nil
}

func (g *Gate) targets(ctx context.Context) ([]executableTarget, error) {
	var targets []executableTarget
	identity, err := identifyExecutable(g.BotPath)
	if err == nil {
		targets = append(targets, executableTarget{path: g.BotPath, identity: identity})
	} else if errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(g.BotPath); !errors.Is(err, os.ErrNotExist) {
			return nil, ErrIncompatible
		}
	} else {
		return nil, ErrIncompatible
	}
	root := g.ProcRoot
	if root == "" {
		root = "/proc"
	}
	if !filepath.IsAbs(root) {
		return nil, ErrIncompatible
	}
	read := g.proc
	if read == nil {
		read = runningBots
	}
	processes, err := read(ctx, root, g.BotPath)
	if err != nil {
		return nil, ErrIncompatible
	}
	return append(targets, processes...), nil
}

func sameTargets(a, b []executableTarget) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func compatibleCapabilities(body []byte) bool {
	if len(body) > capabilityLimit {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return false
	}
	var caps Capabilities
	seen := make(map[string]bool, 2)
	for dec.More() {
		token, err := dec.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return false
		}
		seen[key] = true
		switch key {
		case "protocol_version":
			if err := dec.Decode(&caps.ProtocolVersion); err != nil {
				return false
			}
		case "watch_owner":
			if err := dec.Decode(&caps.WatchOwner); err != nil {
				return false
			}
		default:
			return false
		}
	}
	end, err := dec.Token()
	if err != nil || end != json.Delim('}') {
		return false
	}
	var extra interface{}
	return errors.Is(dec.Decode(&extra), io.EOF) && seen["protocol_version"] && seen["watch_owner"] &&
		caps.ProtocolVersion == ProtocolVersion && caps.WatchOwner == "watchd"
}

// Exposing ReaderFrom would let io.Copy bypass the output limit.
type capabilityOutput struct {
	body   bytes.Buffer
	cancel context.CancelFunc
}

func (w *capabilityOutput) Write(body []byte) (int, error) {
	if len(body) > capabilityLimit-w.body.Len() {
		w.cancel()
		return 0, ErrIncompatible
	}
	return w.body.Write(body)
}

func readCapabilities(parent context.Context, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, capabilityTimeout)
	defer cancel()
	out := &capabilityOutput{cancel: cancel}
	cmd := exec.CommandContext(ctx, path, "--watchd-capabilities")
	cmd.Dir = "/"
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 100 * time.Millisecond
	prepareCapabilityCommand(cmd)
	if err := cmd.Run(); err != nil || ctx.Err() != nil {
		return nil, ErrIncompatible
	}
	return out.body.Bytes(), nil
}
