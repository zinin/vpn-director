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
		cmdline, cmdErr := readProcPrefix(filepath.Join(dir, "cmdline"), 64<<10)
		exe := filepath.Join(dir, "exe")
		link, exeErr := os.Readlink(exe)
		argv0 := string(bytes.SplitN(cmdline, []byte{0}, 2)[0])
		isBot := botExecutableName(link, botPath) || botExecutableName(argv0, botPath)
		if cmdErr != nil || exeErr != nil {
			if isBot {
				asRoot, err := runsAsRoot(dir)
				if err != nil {
					return nil, ErrIncompatible
				}
				if !asRoot || exited(dir, cmdErr) || exited(dir, exeErr) {
					continue
				}
				return nil, ErrIncompatible
			}
			if cmdErr == nil && len(cmdline) == 0 && errors.Is(exeErr, os.ErrNotExist) {
				stat, err := readProcFile(filepath.Join(dir, "stat"), 8<<10)
				if err != nil {
					if exited(dir, err) {
						continue
					}
					return nil, ErrIncompatible
				}
				comm, state, _, err := parseProcessStat(stat, pid)
				if err != nil {
					return nil, ErrIncompatible
				}
				if state != "Z" && state != "X" && botComm(comm, botPath) {
					if asRoot, err := runsAsRoot(dir); err != nil || asRoot {
						return nil, ErrIncompatible
					}
				}
				continue
			}
			if exited(dir, cmdErr) || exited(dir, exeErr) {
				continue
			}
			// An unreadable entry (hidepid, for one) could hide a bot, so it stays a refusal.
			return nil, ErrIncompatible
		}
		if !isBot {
			continue
		}
		stat, err := readProcFile(filepath.Join(dir, "stat"), 8<<10)
		if err != nil {
			if exited(dir, err) {
				continue
			}
			return nil, ErrIncompatible
		}
		_, state, start, err := parseProcessStat(stat, pid)
		if err != nil {
			return nil, ErrIncompatible
		}
		if state == "Z" || state == "X" {
			continue
		}
		// Read between the two stat reads, so the owner is the process the
		// start time names and not a reused PID.
		asRoot, err := runsAsRoot(dir)
		if err != nil {
			return nil, ErrIncompatible
		}
		if !asRoot {
			continue
		}
		identity, err := identifyExecutable(exe)
		if err != nil {
			if exited(dir, err) {
				continue
			}
			return nil, ErrIncompatible
		}
		stat, err = readProcFile(filepath.Join(dir, "stat"), 8<<10)
		if err != nil {
			if exited(dir, err) {
				continue
			}
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

// exited reports whether a read failed because the process is gone: a PID the
// listing showed whose directory no longer exists. A directory that is still
// there keeps the read failure a refusal.
func exited(dir string, err error) bool {
	if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ESRCH) {
		return false
	}
	_, err = os.Stat(dir)
	return errors.Is(err, os.ErrNotExist)
}

// runsAsRoot reports whether the process in dir has real UID 0. Only root runs
// the bot: another user's process named like it is neither executed nor allowed
// to close the gate, and one that exited does not run at all. A status that
// cannot be read, or has no Uid: line, is a refusal.
func runsAsRoot(dir string) (bool, error) {
	status, err := readProcPrefix(filepath.Join(dir, "status"), 8<<10)
	if err != nil {
		if exited(dir, err) {
			return false, nil
		}
		return false, ErrIncompatible
	}
	for _, line := range strings.Split(string(status), "\n") {
		rest, ok := strings.CutPrefix(line, "Uid:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			break
		}
		uid, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil {
			break
		}
		return uid == 0, nil
	}
	return false, ErrIncompatible
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
	body, err := readProcPrefix(path, limit+1)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, ErrIncompatible
	}
	return body, nil
}

// readProcPrefix reads at most limit bytes; a longer file is not an error.
func readProcPrefix(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, err
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
