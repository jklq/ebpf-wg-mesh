package delivery

import "errors"

var (
	ErrVolumeInUse              = errors.New("volume still referenced by service")
	ErrVolumeNotFound           = errors.New("volume not found")
	ErrVolumeAlreadyExists      = errors.New("volume already exists")
	ErrInvalidVolume            = errors.New("volume name and positive size are required")
	ErrSealedSecretsUnavailable = errors.New("sealed secrets are not configured")
	ErrInvalidSealedName        = errors.New("sealed secret name is invalid")
	ErrSealedNameConflict       = errors.New("name is already used by the other variable kind")
	ErrLeaseLost                = errors.New("control-plane lease lost")
	ErrVolumeAgentMismatch      = errors.New("volume bound to different agent")
	ErrConcurrentUpdate         = errors.New("concurrent service update")
	ErrDomainAlreadyExists      = errors.New("domain binding already exists")
	ErrInvalidPort              = errors.New("port must be an integer between 1 and 65535")
	ErrNoPlacementAvailable     = errors.New("no healthy agent satisfies placement")
	ErrInvalidReplicaCount      = errors.New("desired replica count is invalid")
	ErrVolumeReplicaUnsupported = errors.New("volume-backed services support a single replica")
)
