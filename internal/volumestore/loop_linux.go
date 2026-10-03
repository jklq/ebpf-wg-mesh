//go:build linux

package volumestore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// loopBackend stores each volume as an ext4 image mounted through a loop
// device. Layout:
//
//	<root>/volume.img   sparse ext4 image sized to the volume
//	<root>/mnt/         mountpoint of the image
//	<root>/mnt/data/    directory bound into workloads
//
// The filesystem boundary is the size limit: a full volume returns ENOSPC to
// the workload and never consumes host disk beyond the image size. Growing
// extends the image and resizes the mounted filesystem online.
type loopBackend struct{}

const (
	loopImageName  = "volume.img"
	loopMountDir   = "mnt"
	loopDataDir    = "data"
	commandTimeout = 2 * time.Minute
)

func (loopBackend) ensure(root string, sizeBytes int64) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create volume root: %w", err)
	}
	image := filepath.Join(root, loopImageName)
	mountpoint := filepath.Join(root, loopMountDir)
	info, err := os.Stat(image)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := formatImage(root, image, sizeBytes); err != nil {
			return err
		}
	case err != nil:
		return fmt.Errorf("stat volume image: %w", err)
	case !info.Mode().IsRegular():
		return fmt.Errorf("volume image %s is not a regular file", image)
	}
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return fmt.Errorf("create volume mountpoint: %w", err)
	}
	mounted, err := isMountpoint(mountpoint)
	if err != nil {
		return err
	}
	if !mounted {
		if err := run("mount", "-t", "ext4", "-o", "loop,nosuid,nodev,noatime", image, mountpoint); err != nil {
			return fmt.Errorf("mount volume image: %w", err)
		}
	}
	if info, err = os.Stat(image); err != nil {
		return fmt.Errorf("stat volume image: %w", err)
	}
	if info.Size() < sizeBytes {
		if err := growImage(image, sizeBytes); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Join(mountpoint, loopDataDir), 0o755); err != nil {
		return fmt.Errorf("create volume data directory: %w", err)
	}
	return nil
}

func (loopBackend) dataPath(root string) (string, error) {
	mountpoint := filepath.Join(root, loopMountDir)
	mounted, err := isMountpoint(mountpoint)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotProvisioned
	}
	if err != nil {
		return "", err
	}
	if !mounted {
		return "", ErrNotProvisioned
	}
	path := filepath.Join(mountpoint, loopDataDir)
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return "", ErrNotProvisioned
	}
	return path, nil
}

func (loopBackend) usage(root string, _ int64) (int64, int64, int64, error) {
	mountpoint := filepath.Join(root, loopMountDir)
	mounted, err := isMountpoint(mountpoint)
	if err != nil || !mounted {
		// An orphaned image that is not mounted still occupies its file size.
		if info, statErr := os.Stat(filepath.Join(root, loopImageName)); statErr == nil {
			return 0, info.Size(), 0, nil
		}
		if err == nil {
			err = ErrNotProvisioned
		}
		return 0, 0, 0, err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(mountpoint, &stat); err != nil {
		return 0, 0, 0, fmt.Errorf("statfs volume: %w", err)
	}
	blockSize := int64(stat.Bsize)
	capacity := int64(stat.Blocks) * blockSize
	used := (int64(stat.Blocks) - int64(stat.Bfree)) * blockSize
	available := int64(stat.Bavail) * blockSize
	return used, capacity, available, nil
}

func (loopBackend) destroy(root string) error {
	mountpoint := filepath.Join(root, loopMountDir)
	if mounted, err := isMountpoint(mountpoint); err == nil && mounted {
		// A busy mount means a workload still holds the volume; fail and retry
		// on the next reconciliation rather than detaching it underneath.
		if err := unix.Unmount(mountpoint, 0); err != nil {
			return fmt.Errorf("unmount volume: %w", err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.RemoveAll(root)
}

// formatImage builds the filesystem beside its final name and renames it in,
// so a crash mid-format never leaves a half-made image that looks provisioned.
func formatImage(root, image string, sizeBytes int64) error {
	staging := filepath.Join(root, loopImageName+".staging")
	_ = os.Remove(staging)
	file, err := os.OpenFile(staging, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create volume image: %w", err)
	}
	if err := file.Truncate(sizeBytes); err != nil {
		file.Close()
		return fmt.Errorf("size volume image: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close volume image: %w", err)
	}
	// -m 0: the workload is not root on the host, so no blocks are reserved.
	if err := run("mkfs.ext4", "-q", "-F", "-m", "0", "-E", "lazy_itable_init=1,lazy_journal_init=1", staging); err != nil {
		_ = os.Remove(staging)
		return fmt.Errorf("format volume image: %w", err)
	}
	if err := os.Rename(staging, image); err != nil {
		return fmt.Errorf("install volume image: %w", err)
	}
	return nil
}

func growImage(image string, sizeBytes int64) error {
	device, err := loopDevice(image)
	if err != nil {
		return err
	}
	if err := os.Truncate(image, sizeBytes); err != nil {
		return fmt.Errorf("extend volume image: %w", err)
	}
	if err := run("losetup", "--set-capacity", device); err != nil {
		return fmt.Errorf("refresh loop device capacity: %w", err)
	}
	if err := run("resize2fs", device); err != nil {
		return fmt.Errorf("grow volume filesystem: %w", err)
	}
	return nil
}

func loopDevice(image string) (string, error) {
	out, err := output("losetup", "--noheadings", "--output", "NAME", "--associated", image)
	if err != nil {
		return "", fmt.Errorf("find loop device: %w", err)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if device := strings.TrimSpace(line); device != "" {
			return device, nil
		}
	}
	return "", fmt.Errorf("no loop device backs %s", image)
}

func isMountpoint(path string) (bool, error) {
	var self, parent unix.Stat_t
	if err := unix.Lstat(path, &self); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, os.ErrNotExist
		}
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	if err := unix.Lstat(filepath.Dir(path), &parent); err != nil {
		return false, fmt.Errorf("stat %s: %w", filepath.Dir(path), err)
	}
	return self.Dev != parent.Dev, nil
}

func run(name string, args ...string) error {
	_, err := output(name, args...)
	return err
}

func output(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return "", fmt.Errorf("%s: %w: %s", name, err, message)
		}
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return stdout.String(), nil
}
