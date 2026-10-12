//go:build linux

package continuity

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func nativeResources(cmd *exec.Cmd, l Launch) (func(), error) {
	if !l.EnforceResources {
		return func() {}, nil
	}
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil, err
	}
	path := ""
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "0::") {
			path = strings.TrimPrefix(line, "0::")
		}
	}
	if path == "" || path == "/" {
		return nil, fmt.Errorf("continuity requires a delegated systemd cgroup")
	}
	base := filepath.Join("/sys/fs/cgroup", path)
	if filepath.Base(base) == "listener-owner" {
		base = filepath.Dir(base)
	} else {
		owner := filepath.Join(base, "listener-owner")
		if err = os.MkdirAll(owner, 0700); err != nil {
			return nil, err
		}
		if err = os.WriteFile(filepath.Join(owner, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			return nil, err
		}
	}
	if err = os.WriteFile(filepath.Join(base, "cgroup.subtree_control"), []byte("+cpu +memory"), 0600); err != nil {
		return nil, fmt.Errorf("enable delegated resource controllers: %w", err)
	}
	group, err := os.MkdirTemp(base, "generation-")
	if err != nil {
		return nil, err
	}
	for name, value := range map[string]string{"memory.max": strconv.FormatInt(l.MemoryMiB<<20, 10), "memory.swap.max": "0", "cpu.max": fmt.Sprintf("%d 100000", l.CPUMillis*100)} {
		if err = os.WriteFile(filepath.Join(group, name), []byte(value), 0600); err != nil {
			os.Remove(group)
			return nil, err
		}
	}
	fd, err := os.Open(group)
	if err != nil {
		os.Remove(group)
		return nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(fd.Fd())}
	return func() { fd.Close(); os.Remove(group) }, nil
}

func storageAvailable(path string, required int64) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return err
	}
	if stat.Ffree < 64 || uint64(required) > stat.Bavail*uint64(stat.Bsize) {
		return fmt.Errorf("insufficient available storage or inodes for candidate configuration")
	}
	return nil
}
