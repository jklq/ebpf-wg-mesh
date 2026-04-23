package controlplane

import platformv1 "ebof-wg-mesh/api/proto/platformv1"

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
		runtime = &platformv1.ServiceRuntime{}
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
		runtime = &platformv1.ServiceRuntime{}
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
