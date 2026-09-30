package monitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// errExited is a prober that exited before it listened.
var errExited = errors.New("the prober exited")

const tailBytes = 64 * 1024

// tail keeps the last lines of the prober's output within tailBytes.
type tail struct {
	mu    sync.Mutex
	lines []string
	part  []byte
	max   int
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if len(p) > tailBytes {
		p = p[len(p)-tailBytes:]
		t.lines = nil
		t.part = nil
	}
	t.part = append(t.part, p...)
	if len(t.part) > tailBytes {
		t.part = append([]byte(nil), t.part[len(t.part)-tailBytes:]...)
	}
	for {
		i := bytes.IndexByte(t.part, '\n')
		if i < 0 {
			break
		}
		t.lines = append(t.lines, string(t.part[:i]))
		t.part = t.part[i+1:]
		if len(t.lines) > t.max {
			drop := len(t.lines) - t.max
			clear(t.lines[:drop])
			t.lines = t.lines[drop:]
		}
	}
	size, keep := len(t.part), len(t.lines)
	for i := len(t.lines) - 1; i >= 0; i-- {
		size += len(t.lines[i]) + 1
		if size > tailBytes {
			break
		}
		keep = i
	}
	clear(t.lines[:keep])
	t.lines = t.lines[keep:]
	return n, nil
}

// String is the kept lines and any unfinished one.
func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := append([]string(nil), t.lines...)
	if len(t.part) > 0 {
		lines = append(lines, string(t.part))
	}
	return strings.Join(lines, "\n")
}

// process is a running prober.
type process struct {
	cmd    *exec.Cmd
	out    *tail
	exited chan struct{}
}

// startProcess runs bin with args. The child gets SIGKILL when its parent
// thread dies (Pdeathsig), and Pdeathsig follows the thread that forked the
// child, not the process: the goroutine that starts the child locks its
// thread and holds it until the child is gone.
func startProcess(bin string, args ...string) (*process, error) {
	p := &process{out: &tail{max: 20}, exited: make(chan struct{})}
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		cmd := exec.Command(bin, args...)
		cmd.Stdout = p.out
		cmd.Stderr = p.out
		cmd.SysProcAttr = childAttr()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		p.cmd = cmd
		started <- nil
		_ = cmd.Wait()
		close(p.exited)
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return p, nil
}

// waitListening waits until 127.0.0.1:port accepts, the process exits
// (errExited), ctx ends or timeout passes.
func (p *process) waitListening(ctx context.Context, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for {
		if c, err := net.DialTimeout("tcp4", addr, 100*time.Millisecond); err == nil {
			c.Close()
			return nil
		}
		select {
		case <-p.exited:
			return errExited
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the prober did not listen within %s", timeout)
		}
	}
}

// stop asks the process to end, and kills it 2 s later.
func (p *process) stop() {
	select {
	case <-p.exited:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(2 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
}
