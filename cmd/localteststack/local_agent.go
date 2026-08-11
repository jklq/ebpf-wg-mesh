package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"ebof-wg-mesh/internal/agent"
	"ebof-wg-mesh/internal/config"
	localstack "ebof-wg-mesh/internal/localteststack"
	"ebof-wg-mesh/internal/mesh"
)

const localAgentID = "localteststack-agent"

func startLocalAgent(ctx context.Context, stackCfg localStackConfig, stateDir string, controlPlaneAddr string, caPEM []byte) (*agent.App, <-chan error, error) {
	agentDir := filepath.Join(stateDir, "local-agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("mkdir local agent dir: %w", err)
	}
	caPath := filepath.Join(agentDir, "controlplane-ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		return nil, nil, fmt.Errorf("write local agent controlplane ca: %w", err)
	}
	privateKey, err := mesh.GeneratePrivateKey()
	if err != nil {
		return nil, nil, fmt.Errorf("local agent: %w", err)
	}
	runtimeImpl, err := localstack.NewDockerRuntime(localstack.DockerRuntimeConfig{
		DataDir:            agentDir,
		VolumesDir:         filepath.Join(agentDir, "volumes"),
		DockerNetwork:      stackCfg.DockerNetwork,
		Runner:             localstack.ExecDockerRunner{},
		ContainerNamePrefx: "localteststack-svc",
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create local docker runtime: %w", err)
	}

	cfg := config.AgentConfig{
		Node: config.NodeConfig{
			ID:            localAgentID,
			Name:          "Local Teststack Agent",
			AdvertiseAddr: "::1",
			Resources: config.NodeResourcesConfig{
				CPUMillis:       8000,
				MemoryMebibytes: 16384,
			},
		},
		ControlPlane: config.ControlPlaneClientConfig{
			Address: controlPlaneAddr,
			TLS: config.ClientTLSConfig{
				CAFile:         caPath,
				ServerName:     "localhost",
				BootstrapToken: "agent-bootstrap-token",
			},
		},
		Runtime: config.RuntimeConfig{
			DataDir:        agentDir,
			VolumesDir:     filepath.Join(agentDir, "volumes"),
			Snapshotter:    "native",
			DisableCgroups: true,
		},
		Mesh: config.MeshConfig{
			Host: config.HostConfig{IPv6: "::1"},
			WireGuard: config.WireGuard{
				InterfaceName: "wg0",
				PrivateKey:    privateKey,
				ListenPort:    51820,
			},
		},
	}
	if err := config.FinalizeAgent(&cfg); err != nil {
		return nil, nil, fmt.Errorf("finalize local agent config: %w", err)
	}

	app, err := agent.New(cfg, agent.WithRuntime(runtimeImpl), agent.WithMeshDisabled())
	if err != nil {
		return nil, nil, fmt.Errorf("create local agent: %w", err)
	}
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- app.Run(ctx)
	}()
	return app, runErrCh, nil
}
