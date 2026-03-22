package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

const defaultVolumeMount = "/data"

var jsonEncoderPool = sync.Pool{
	New: func() interface{} {
		return &bytes.Buffer{}
	},
}

type serviceEngine interface {
	EnsureService(context.Context, *agentv1.DesiredService) (serviceStatus, bool, error)
	RemoveService(context.Context, string) error
	Close() error
}

type serviceStatus struct {
	AppliedSpecRevision      int64
	AppliedRolloutGeneration int64
	Endpoint                 string
}

type ContainerdRuntime struct {
	cfg    config.AgentConfig
	engine serviceEngine
}

func NewContainerdRuntime(cfg config.AgentConfig) (*ContainerdRuntime, error) {
	if err := os.MkdirAll(filepath.Join(cfg.Runtime.DataDir, "desired"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir desired dir: %w", err)
	}
	if err := os.MkdirAll(cfg.Runtime.VolumesDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir volumes dir: %w", err)
	}
	engine, err := newContainerdEngine(cfg)
	if err != nil {
		return nil, err
	}
	return &ContainerdRuntime{cfg: cfg, engine: engine}, nil
}

func (r *ContainerdRuntime) Close() error {
	if r == nil || r.engine == nil {
		return nil
	}
	return r.engine.Close()
}

func (r *ContainerdRuntime) Reconcile(ctx context.Context, state *agentv1.DesiredNodeState) (*agentv1.StatusReport, error) {
	report := &agentv1.StatusReport{AgentId: state.GetAgentId()}
	desiredVolumes := indexDesiredVolumes(state.GetVolumes())
	desiredServices := indexDesiredServices(state.GetServices())

	if err := r.pruneStaleServices(ctx, desiredServices); err != nil {
		return nil, err
	}
	if err := r.pruneStaleVolumes(desiredVolumes); err != nil {
		return nil, err
	}

	for _, vol := range state.GetVolumes() {
		path := filepath.Join(r.cfg.Runtime.VolumesDir, vol.GetVolumeId())
		if err := os.MkdirAll(path, 0o755); err != nil {
			report.Volumes = append(report.Volumes, &agentv1.VolumeCondition{
				VolumeId: vol.GetVolumeId(),
				Phase:    "Error",
				Message:  err.Error(),
			})
			continue
		}
		report.Volumes = append(report.Volumes, &agentv1.VolumeCondition{
			VolumeId: vol.GetVolumeId(),
			Phase:    "Ready",
			Message:  path,
		})
	}

	for _, svc := range state.GetServices() {
		cond := &agentv1.ServiceCondition{
			AllocationId:             svc.GetAllocationId(),
			ServiceId:                svc.GetServiceId(),
			DesiredSpecRevision:      svc.GetDesiredSpecRevision(),
			DesiredRolloutGeneration: svc.GetDesiredRolloutGeneration(),
			Phase:                    "Pending",
		}
		if err := r.persistDesiredService(svc); err != nil {
			cond.Phase = "Error"
			cond.Message = err.Error()
			report.Services = append(report.Services, cond)
			continue
		}
		status, created, err := r.engine.EnsureService(ctx, svc)
		if err != nil {
			cond.Phase = "Error"
			cond.Message = err.Error()
			report.Services = append(report.Services, cond)
			continue
		}
		cond.AppliedSpecRevision = status.AppliedSpecRevision
		cond.AppliedRolloutGeneration = status.AppliedRolloutGeneration
		cond.EndpointAddr = status.Endpoint
		cond.Healthy = probeHealth(status.Endpoint, svc)
		switch {
		case cond.Healthy:
			cond.Phase = "Healthy"
			cond.Message = "container healthy"
		case created:
			cond.Phase = "Starting"
			cond.Message = "container created or replaced"
		default:
			cond.Phase = "Running"
			cond.Message = "container reconciled"
		}
		report.Services = append(report.Services, cond)
	}
	return report, nil
}

func (r *ContainerdRuntime) pruneStaleServices(ctx context.Context, desired map[string]*agentv1.DesiredService) error {
	paths, err := filepath.Glob(filepath.Join(r.cfg.Runtime.DataDir, "desired", "*.json"))
	if err != nil {
		return fmt.Errorf("glob desired files: %w", err)
	}
	for _, path := range paths {
		allocationID := strings.TrimSuffix(filepath.Base(path), ".json")
		if _, ok := desired[allocationID]; ok {
			continue
		}
		if err := r.engine.RemoveService(ctx, allocationID); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove desired file %s: %w", path, err)
		}
	}
	return nil
}

func (r *ContainerdRuntime) pruneStaleVolumes(desired map[string]*agentv1.DesiredVolume) error {
	entries, err := os.ReadDir(r.cfg.Runtime.VolumesDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read volumes dir: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, ok := desired[entry.Name()]; ok {
			continue
		}
		if err := os.RemoveAll(filepath.Join(r.cfg.Runtime.VolumesDir, entry.Name())); err != nil {
			return fmt.Errorf("remove stale volume %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func (r *ContainerdRuntime) persistDesiredService(svc *agentv1.DesiredService) error {
	path := filepath.Join(r.cfg.Runtime.DataDir, "desired", svc.GetAllocationId()+".json")
	buf := jsonEncoderPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer jsonEncoderPool.Put(buf)
	enc := json.NewEncoder(buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(svc); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func probeHealth(endpoint string, svc *agentv1.DesiredService) bool {
	runtime := svc.GetSpec().GetRuntime()
	if endpoint == "" || runtime.GetContainerPort() == 0 {
		return false
	}
	check := runtime.GetHealthCheck()
	if check == nil || check.GetType() == platformv1.HealthCheck_TYPE_UNSPECIFIED {
		conn, err := net.DialTimeout("tcp", endpoint, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return true
		}
		return false
	}
	switch check.GetType() {
	case platformv1.HealthCheck_TYPE_HTTP:
		client := http.Client{Timeout: time.Duration(maxInt32(check.GetTimeoutSeconds(), 2)) * time.Second}
		resp, err := client.Get("http://" + endpoint + check.GetPath())
		if err == nil && resp.StatusCode < 500 {
			resp.Body.Close()
			return true
		}
		if resp != nil {
			resp.Body.Close()
		}
	case platformv1.HealthCheck_TYPE_TCP:
		conn, err := net.DialTimeout("tcp", endpoint, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return true
		}
	}
	return false
}

func containerName(allocationID string) string {
	return "platform-" + allocationID
}

func maxInt32(v int32, fallback int32) int32 {
	if v <= 0 {
		return fallback
	}
	return v
}

func indexDesiredVolumes(items []*agentv1.DesiredVolume) map[string]*agentv1.DesiredVolume {
	out := make(map[string]*agentv1.DesiredVolume, len(items))
	for _, item := range items {
		out[item.GetVolumeId()] = item
	}
	return out
}

func indexDesiredServices(items []*agentv1.DesiredService) map[string]*agentv1.DesiredService {
	out := make(map[string]*agentv1.DesiredService, len(items))
	for _, item := range items {
		out[item.GetAllocationId()] = item
	}
	return out
}
