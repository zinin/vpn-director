package monitor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Stamp names the state of the files the endpoint set is built from: every
// *.json in dir and each of files, by name, inode, size and modification
// time. Every writer replaces those files by a rename, which gives a new
// inode, so equal stamps mean nothing was written - JFFS2 keeps modification
// times to the second, and a size can repeat. A file that is missing stamps
// as missing.
func Stamp(dir string, files ...string) string {
	var parts []string
	add := func(path string) {
		info, err := os.Stat(path)
		if err != nil {
			parts = append(parts, path+":missing")
			return
		}
		var ino uint64
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			ino = uint64(st.Ino)
		}
		parts = append(parts, fmt.Sprintf("%s:%d:%d:%d", path, ino, info.Size(), info.ModTime().UnixNano()))
	}
	names, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.Strings(names)
	for _, name := range names {
		add(name)
	}
	for _, f := range files {
		add(f)
	}
	return strings.Join(parts, "|")
}
