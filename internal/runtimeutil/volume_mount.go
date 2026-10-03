package runtimeutil

import (
	"errors"
	"fmt"
	"path"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

// DefaultVolumeMountPath is where a volume mounts when the service does not
// choose a path.
const DefaultVolumeMountPath = "/data"

const maxVolumeMountPathLength = 255

// systemMountRoots are image and kernel paths a volume may not cover. Mounting
// over them would hide the image's own files or shadow the runtime's mounts.
var systemMountRoots = []string{
	"/bin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/libx32",
	"/proc", "/run", "/sbin", "/sys", "/usr", "/var/run",
}

var ErrInvalidVolumeMountPath = errors.New("invalid volume mount path")

// ValidateVolumeMountPath accepts a clean absolute path that is neither the
// root nor inside a system directory.
func ValidateVolumeMountPath(mountPath string) error {
	if mountPath == "" {
		return fmt.Errorf("%w: path is required", ErrInvalidVolumeMountPath)
	}
	if len(mountPath) > maxVolumeMountPathLength {
		return fmt.Errorf("%w: path exceeds %d characters", ErrInvalidVolumeMountPath, maxVolumeMountPathLength)
	}
	if !strings.HasPrefix(mountPath, "/") {
		return fmt.Errorf("%w: %q must be absolute", ErrInvalidVolumeMountPath, mountPath)
	}
	for _, char := range mountPath {
		if char < 0x21 || char == 0x7f || char == ',' || char == ':' || char == '\\' {
			return fmt.Errorf("%w: %q contains an unsupported character", ErrInvalidVolumeMountPath, mountPath)
		}
	}
	if path.Clean(mountPath) != mountPath {
		return fmt.Errorf("%w: %q must be a clean path without trailing slashes or dot segments", ErrInvalidVolumeMountPath, mountPath)
	}
	if mountPath == "/" {
		return fmt.Errorf("%w: cannot mount over the container root", ErrInvalidVolumeMountPath)
	}
	for _, root := range systemMountRoots {
		if mountPath == root || strings.HasPrefix(mountPath, root+"/") {
			return fmt.Errorf("%w: %q is inside system directory %s", ErrInvalidVolumeMountPath, mountPath, root)
		}
	}
	return nil
}

// VolumeMountPath returns the validated container path for svc's volume.
// Agents re-validate what the control plane accepted: the mount destination
// is an input to the sandbox.
func VolumeMountPath(runtime *platformv1.ServiceRuntime) (string, error) {
	mountPath := runtime.GetVolume().GetMountPath()
	if mountPath == "" {
		mountPath = DefaultVolumeMountPath
	}
	if err := ValidateVolumeMountPath(mountPath); err != nil {
		return "", err
	}
	return mountPath, nil
}

// VolumeEnv describes the attached volume to the workload.
func VolumeEnv(volumeName, mountPath string) []string {
	return []string{"PLATFORM_VOLUME_NAME=" + volumeName, "PLATFORM_VOLUME_MOUNT_PATH=" + mountPath}
}
