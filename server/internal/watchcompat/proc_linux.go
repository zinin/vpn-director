//go:build linux

package watchcompat

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

func identifyExecutable(path string) (executableIdentity, error) {
	info, err := os.Stat(path)
	if err != nil {
		return executableIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return executableIdentity{}, ErrIncompatible
	}
	return executableIdentity{
		device: uint64(stat.Dev),
		inode:  uint64(stat.Ino),
		size:   info.Size(),
		mtime:  info.ModTime().UnixNano(),
	}, nil
}

func runningBots(ctx context.Context, root, botPath string) ([]executableTarget, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, ErrIncompatible
	}
	var targets []executableTarget
	for _, entry := range entries {
		if ctx.Err() != nil {
			return nil, ErrIncompatible
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		cmdline, cmdErr := readProcFile(filepath.Join(dir, "cmdline"), 64<<10)
		exe := filepath.Join(dir, "exe")
		link, exeErr := os.Readlink(exe)
		argv0 := string(bytes.SplitN(cmdline, []byte{0}, 2)[0])
		isBot := botExecutableName(link, botPath) || botExecutableName(argv0, botPath)
		if cmdErr != nil || exeErr != nil {
			if isBot {
				return nil, ErrIncompatible
			}
			if cmdErr == nil && len(cmdline) == 0 && errors.Is(exeErr, os.ErrNotExist) {
				stat, err := readProcFile(filepath.Join(dir, "stat"), 8<<10)
				if err != nil {
					return nil, ErrIncompatible
				}
				comm, state, _, err := parseProcessStat(stat, pid)
				if err != nil || (state != "Z" && state != "X" && botComm(comm, botPath)) {
					return nil, ErrIncompatible
				}
				continue
			}
			return nil, ErrIncompatible
		}
		if !isBot {
			continue
		}
		stat, err := readProcFile(filepath.Join(dir, "stat"), 8<<10)
		if err != nil {
			return nil, ErrIncompatible
		}
		_, state, start, err := parseProcessStat(stat, pid)
		if err != nil {
			return nil, ErrIncompatible
		}
		if state == "Z" || state == "X" {
			continue
		}
		identity, err := identifyExecutable(exe)
		if err != nil {
			return nil, ErrIncompatible
		}
		stat, err = readProcFile(filepath.Join(dir, "stat"), 8<<10)
		if err != nil {
			return nil, ErrIncompatible
		}
		_, state, rechecked, err := parseProcessStat(stat, pid)
		if err != nil || rechecked != start || state == "Z" || state == "X" {
			return nil, ErrIncompatible
		}
		targets = append(targets, executableTarget{path: exe, identity: identity, pid: pid, start: start})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].pid < targets[j].pid })
	return targets, nil
}

func botExecutableName(path, botPath string) bool {
	path = strings.TrimSuffix(path, " (deleted)")
	return path != "" && filepath.Base(path) == filepath.Base(botPath)
}

func botComm(comm, botPath string) bool {
	name := filepath.Base(botPath)
	if len(name) > 15 {
		name = name[:15]
	}
	return comm == name
}

func parseProcessStat(body []byte, pid int) (comm, state string, start uint64, err error) {
	text := string(body)
	open, close := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if open < 0 || close <= open || strings.TrimSpace(text[:open]) != strconv.Itoa(pid) {
		return "", "", 0, ErrIncompatible
	}
	fields := strings.Fields(text[close+1:])
	// The fields after comm start at state (3); starttime is field 22.
	if len(fields) < 20 || len(fields[0]) != 1 {
		return "", "", 0, ErrIncompatible
	}
	start, err = strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return "", "", 0, ErrIncompatible
	}
	return text[open+1 : close], fields[0], start, nil
}

func readProcFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, ErrIncompatible
	}
	return body, nil
}

func prepareCapabilityCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
