package watchcompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

	mu sync.Mutex
	// cache holds the executables whose capabilities were verified, by
	// identity: a process running one runs the verified bytes, whatever its PID.
	cache    map[executableIdentity]struct{}
	refused  executableTarget // the last refusal logged; zero once a Check succeeds
	reported string           // the last refusal without a run logged; empty once a Check succeeds
	proc     func(context.Context, string, string) ([]executableTarget, error)
	exec     func(context.Context, string) ([]byte, error)
}

func (g *Gate) Check(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	deny := func() error {
		g.cache = nil
		return ErrIncompatible
	}
	// A refusal that runs no executable is named once while it keeps the gate
	// closed, as a refusing executable is: the owner needs to know what does.
	report := func(msg string, args ...any) error {
		if key := msg + fmt.Sprint(args...); key != g.reported {
			g.reported = key
			slog.Warn(msg, args...)
		}
		return deny()
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
		return report("Bot compatibility check failed; automation stays incompatible", "reason", err.Error())
	}
	run := g.exec
	if run == nil {
		run = readCapabilities
	}
	next := make(map[executableIdentity]struct{}, len(before))
	for _, target := range before {
		if ctx.Err() != nil {
			return ErrIncompatible
		}
		_, cached := g.cache[target.identity]
		_, verified := next[target.identity]
		if !cached && !verified {
			body, err := run(ctx, target.path)
			if err != nil || !compatibleCapabilities(body) {
				if ctx.Err() != nil {
					return ErrIncompatible
				}
				// Once per target while it keeps the gate closed: the owner
				// needs to know which executable does.
				if target != g.refused {
					g.refused = target
					slog.Warn("Bot compatibility refused; automation stays incompatible while this executable runs",
						"pid", target.pid, "path", executablePath(target))
				}
				return deny()
			}
		}
		next[target.identity] = struct{}{}
	}
	// Executing capabilities must not bless a replaced file: a process that
	// started meanwhile passes only if it runs an executable this check
	// verified, as the bot's own child does between fork and exec. One that
	// exited meanwhile adds no unverified code.
	after, err := g.targets(ctx)
	if ctx.Err() != nil {
		return ErrIncompatible
	}
	if err != nil {
		return report("Bot compatibility check failed; automation stays incompatible", "reason", err.Error())
	}
	cache := make(map[executableIdentity]struct{}, len(after))
	for _, target := range after {
		if _, ok := next[target.identity]; !ok {
			return report("Bot compatibility refused: a bot process started during the check; automation stays incompatible",
				"pid", target.pid, "path", executablePath(target))
		}
		cache[target.identity] = struct{}{}
	}
	g.cache = cache
	g.refused = executableTarget{}
	g.reported = ""
	return nil
}

// executablePath names a target in the log: a running process by what its
// /proc/<pid>/exe link points to, the installed bot by its path.
func executablePath(target executableTarget) string {
	if target.pid != 0 {
		if link, err := os.Readlink(target.path); err == nil {
			return link
		}
	}
	return target.path
}

func (g *Gate) targets(ctx context.Context) ([]executableTarget, error) {
	var targets []executableTarget
	identity, err := identifyExecutable(g.BotPath)
	if err == nil {
		targets = append(targets, executableTarget{path: g.BotPath, identity: identity})
	} else if errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(g.BotPath); !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: the installed bot path does not resolve", ErrIncompatible)
		}
	} else {
		return nil, fmt.Errorf("%w: the installed bot cannot be checked", ErrIncompatible)
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
		if !errors.Is(err, ErrIncompatible) {
			err = fmt.Errorf("%w: %v", ErrIncompatible, err)
		}
		return nil, err
	}
	return append(targets, processes...), nil
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
