package controlplane

import (
	"errors"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

const (
	defaultServiceCPUMillis       int64 = 250
	defaultServiceMemoryMebibytes int64 = 256
)

func validatePort(port int32) error {
	if port < 1 || port > 65535 {
		return errInvalidPort
	}
	return nil
}

func runtimePortsFromInts(ports []int32) []*platformv1.ServiceRuntimePort {
	out := make([]*platformv1.ServiceRuntimePort, 0, len(ports))
	seen := make(map[int32]struct{}, len(ports))
	for _, port := range ports {
		if validatePort(port) != nil {
			continue
		}
		if _, ok := seen[port]; ok {
			continue
		}
		seen[port] = struct{}{}
		out = append(out, &platformv1.ServiceRuntimePort{
			Port:    port,
			Primary: len(out) == 0,
		})
	}
	return out
}

func directImageServiceSpec(image string, runtime *platformv1.ServiceRuntime) *platformv1.ServiceSpec {
	if runtime == nil {
		runtime = defaultServiceRuntime()
	}
	return &platformv1.ServiceSpec{
		Runtime: runtime,
		Source: &platformv1.ServiceSource{
			Source: &platformv1.ServiceSource_Image{
				Image: &platformv1.DirectImageSource{Image: image},
			},
		},
	}
}

func repositoryServiceSpec(runtime *platformv1.ServiceRuntime, source *platformv1.ServiceSourceSpec) *platformv1.ServiceSpec {
	if runtime == nil {
		runtime = defaultServiceRuntime()
	}
	if source == nil {
		source = &platformv1.ServiceSourceSpec{}
	}
	return &platformv1.ServiceSpec{
		Runtime: runtime,
		Source: &platformv1.ServiceSource{
			Source: &platformv1.ServiceSource_SourceSpec{
				SourceSpec: source,
			},
		},
	}
}

func defaultServiceRuntime() *platformv1.ServiceRuntime {
	return &platformv1.ServiceRuntime{
		CpuMillis:       defaultServiceCPUMillis,
		MemoryMebibytes: defaultServiceMemoryMebibytes,
	}
}

func validateServiceSpecResources(spec *platformv1.ServiceSpec) error {
	if spec == nil || spec.GetRuntime() == nil {
		return errors.New("runtime resources are required")
	}
	runtime := spec.GetRuntime()
	if runtime.GetCpuMillis() < defaultServiceCPUMillis {
		return errors.New("cpu_millis must be at least 250")
	}
	if runtime.GetMemoryMebibytes() < defaultServiceMemoryMebibytes {
		return errors.New("memory_mebibytes must be at least 256")
	}
	return nil
}
