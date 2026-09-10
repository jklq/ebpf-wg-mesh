package runtimeutil

import (
	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func RuntimePortNumbers(runtime *platformv1.ServiceRuntime) []int32 {
	if runtime == nil {
		return nil
	}
	seen := make(map[int32]struct{}, len(runtime.GetPorts()))
	var out []int32
	for _, item := range runtime.GetPorts() {
		port := item.GetPort()
		if port < 1 || port > 65535 {
			continue
		}
		if _, ok := seen[port]; ok {
			continue
		}
		seen[port] = struct{}{}
		out = append(out, port)
	}
	return out
}

func ReadinessCheckPort(runtime *platformv1.ServiceRuntime, check *platformv1.HealthCheck) int32 {
	if check.GetPort() > 0 {
		return check.GetPort()
	}
	for _, item := range runtime.GetPorts() {
		if item.GetPrimary() {
			return item.GetPort()
		}
	}
	ports := RuntimePortNumbers(runtime)
	if len(ports) > 0 {
		return ports[0]
	}
	return 0
}

func ReadinessPorts(svc *agentv1.DesiredService) []int32 {
	runtime := svc.GetSpec().GetRuntime()
	ports := RuntimePortNumbers(runtime)
	if len(ports) == 0 {
		if check := runtime.GetHealthCheck(); check != nil && check.GetPort() > 0 {
			return []int32{check.GetPort()}
		}
	}
	return ports
}

func IndexDesiredVolumes(items []*agentv1.DesiredVolume) map[string]*agentv1.DesiredVolume {
	out := make(map[string]*agentv1.DesiredVolume, len(items))
	for _, item := range items {
		out[item.GetVolumeId()] = item
	}
	return out
}

func IndexDesiredServices(items []*agentv1.DesiredService) map[string]*agentv1.DesiredService {
	out := make(map[string]*agentv1.DesiredService, len(items))
	for _, item := range items {
		out[item.GetAllocationId()] = item
	}
	return out
}
