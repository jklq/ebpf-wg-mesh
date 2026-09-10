//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strings"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	"github.com/vishvananda/netns"
)

func probeServiceHealthInNamespace(ctx context.Context, namespacePath, allocationIP string, svc *agentv1.DesiredService) serviceHealthProbe {
	return probeServiceHealthWithDialer(ctx, allocationIP, svc, workloadNamespaceDialer(namespacePath))
}

func workloadNamespaceDialer(namespacePath string) dialContextFunc {
	namespacePath = strings.TrimSpace(namespacePath)
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if namespacePath == "" {
			return nil, errors.New("workload network namespace is unavailable")
		}
		return dialInNetworkNamespace(ctx, namespacePath, network, address)
	}
}

func dialInNetworkNamespace(ctx context.Context, namespacePath, network, address string) (net.Conn, error) {
	runtime.LockOSThread()
	unlockThread := true
	defer func() {
		if unlockThread {
			runtime.UnlockOSThread()
		}
	}()

	hostNS, err := netns.Get()
	if err != nil {
		return nil, fmt.Errorf("capture agent network namespace: %w", err)
	}
	defer hostNS.Close()
	targetNS, err := netns.GetFromPath(namespacePath)
	if err != nil {
		return nil, fmt.Errorf("open workload network namespace: %w", err)
	}
	defer targetNS.Close()
	if err := netns.Set(targetNS); err != nil {
		return nil, fmt.Errorf("enter workload network namespace: %w", err)
	}

	conn, dialErr := (&net.Dialer{}).DialContext(ctx, network, address)
	if restoreErr := netns.Set(hostNS); restoreErr != nil {
		if conn != nil {
			_ = conn.Close()
		}
		unlockThread = false
		return nil, fmt.Errorf("restore agent network namespace: %w", restoreErr)
	}
	if dialErr != nil {
		return nil, dialErr
	}
	return conn, nil
}
