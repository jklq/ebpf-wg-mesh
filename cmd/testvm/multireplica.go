package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/testutil"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type singletonLease struct {
	Holder string
	Token  int64
}

type crossReplicaFixture struct {
	EnvironmentID string
	ServiceID     string
	Hostname      string
}

func dialPlatform(ctx context.Context, address string, identity clientIdentity) (*grpc.ClientConn, error) {
	caPEM, certPEM, keyPEM, err := identityMaterial(identity)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("append ca pem")
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return grpc.DialContext(dialCtx, address,
		grpc.WithBlock(),
		grpc.WithPerRPCCredentials(vmUserAssertionCredentials{secret: vmUserAssertionSecret, userID: vmUserID}),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{cert},
			ServerName:   "controlplane",
			MinVersion:   tls.VersionTLS13,
		})),
	)
}

func runCrossReplicaNotificationScenario(
	ctx context.Context,
	writeAddress string,
	readAddress string,
	identity clientIdentity,
	sshKeyPath string,
	controlplane hostInfo,
	hosts map[string]hostInfo,
) (crossReplicaFixture, error) {
	writeConn, err := dialPlatform(ctx, writeAddress, identity)
	if err != nil {
		return crossReplicaFixture{}, fmt.Errorf("dial write replica: %w", err)
	}
	defer writeConn.Close()
	readConn, err := dialPlatform(ctx, readAddress, identity)
	if err != nil {
		return crossReplicaFixture{}, fmt.Errorf("dial read replica: %w", err)
	}
	defer readConn.Close()

	writer := platformv1.NewPlatformServiceClient(writeConn)
	reader := platformv1.NewPlatformServiceClient(readConn)
	if _, err := waitForAgents(ctx, ctx, reader, 2, sshKeyPath, hosts); err != nil {
		return crossReplicaFixture{}, err
	}

	project, err := writer.CreateProject(ctx, &platformv1.CreateProjectRequest{
		Name: "vm-multireplica-" + time.Now().UTC().Format("150405"),
	})
	if err != nil {
		return crossReplicaFixture{}, fmt.Errorf("create multi-replica project through write replica: %w", err)
	}
	environments, err := writer.ListEnvironments(ctx, &platformv1.ListEnvironmentsRequest{ProjectId: project.GetId()})
	if err != nil {
		return crossReplicaFixture{}, fmt.Errorf("load multi-replica environment: %w", err)
	}
	if len(environments.GetEnvironments()) != 1 {
		return crossReplicaFixture{}, fmt.Errorf("load multi-replica environment: got %d environments, want 1", len(environments.GetEnvironments()))
	}
	environmentID := environments.GetEnvironments()[0].GetId()
	baseline, err := reader.ListServices(ctx, &platformv1.ListServicesRequest{EnvironmentId: environmentID})
	if err != nil {
		return crossReplicaFixture{}, fmt.Errorf("read service index from second replica: %w", err)
	}

	serviceName := fmt.Sprintf("replica-web-%08x", uint32(time.Now().UnixNano()))
	waitCtx, cancelWait := context.WithTimeout(ctx, 30*time.Second)
	defer cancelWait()
	waitStarted := make(chan struct{})
	waitResult := make(chan error, 1)
	go func() {
		close(waitStarted)
		waitResult <- waitForServiceNameFromIndex(waitCtx, reader, environmentID, serviceName, baseline.GetIndex())
	}()
	<-waitStarted
	select {
	case <-ctx.Done():
		return crossReplicaFixture{}, ctx.Err()
	case <-time.After(250 * time.Millisecond):
	}

	marker := fmt.Sprintf("vm-e2e-multireplica-%08x", uint32(time.Now().UnixNano()))
	service, err := writer.CreateService(ctx, &platformv1.CreateServiceRequest{
		EnvironmentId: environmentID,
		Service: &platformv1.ServiceInput{
			Name: serviceName,
			Spec: inMemoryHTTPServiceSpec(marker),
		},
	})
	if err != nil {
		return crossReplicaFixture{}, fmt.Errorf("create service through write replica: %w", err)
	}
	if err := <-waitResult; err != nil {
		return crossReplicaFixture{}, fmt.Errorf("blocked ListServices on second replica: %w", err)
	}
	infof("scenario: blocked ListServices on the second replica observed the write")

	if _, err := writer.ReleaseEnvironment(ctx, &platformv1.ReleaseEnvironmentRequest{EnvironmentId: environmentID}); err != nil {
		return crossReplicaFixture{}, fmt.Errorf("deploy through write replica: %w", err)
	}
	status, err := waitForServiceHealthy(ctx, ctx, reader, service.GetId(), service.GetSpecRevision(), 1)
	if err != nil {
		return crossReplicaFixture{}, fmt.Errorf("wait for service delivered through second-replica agent streams: %w", err)
	}
	allocatedHost, ok := hosts[status.GetAllocation().GetAgentId()]
	if !ok {
		return crossReplicaFixture{}, fmt.Errorf("no host metadata for multi-replica allocation agent %q", status.GetAllocation().GetAgentId())
	}
	if err := assertHTTPResponseInAllocationNetNS(ctx, sshKeyPath, allocatedHost.PublicIPv4, status.GetAllocation().GetAllocationId(), allocationEndpoint(status.GetAllocation()), "/", marker); err != nil {
		return crossReplicaFixture{}, fmt.Errorf("verify cross-replica workload: %w", err)
	}
	infof("scenario: agents connected to the second replica applied a deployment written through the primary")

	binding, err := writer.GenerateDomainBinding(ctx, &platformv1.GenerateDomainBindingRequest{
		ServiceId:  service.GetId(),
		TargetPort: 8080,
	})
	if err != nil {
		return crossReplicaFixture{}, fmt.Errorf("generate ingress binding through write replica: %w", err)
	}
	if err := waitForRemoteCommand(ctx, sshKeyPath, controlplane.PublicIPv4, fmt.Sprintf("grep -Fq %q /var/lib/ebpf-wg-mesh/ingress-probe/latest.json && grep -Fq %q /var/lib/ebpf-wg-mesh/ingress-probe/latest.json", binding.GetHostname(), allocationEndpoint(status.GetAllocation()))); err != nil {
		return crossReplicaFixture{}, fmt.Errorf("wait for primary ingress publication: %w", err)
	}

	return crossReplicaFixture{
		EnvironmentID: environmentID,
		ServiceID:     service.GetId(),
		Hostname:      binding.GetHostname(),
	}, nil
}

func waitForServiceNameFromIndex(ctx context.Context, client platformv1.PlatformServiceClient, environmentID, serviceName string, index int64) error {
	for {
		response, err := client.ListServices(ctx, &platformv1.ListServicesRequest{
			EnvironmentId:      environmentID,
			WaitIndex:          index,
			WaitTimeoutSeconds: 5,
		})
		if err != nil {
			return err
		}
		index = response.GetIndex()
		for _, service := range response.GetServices() {
			if service.GetName() == serviceName {
				return nil
			}
		}
	}
}

func cleanupCrossReplicaFixture(ctx context.Context, address string, identity clientIdentity, fixture crossReplicaFixture) error {
	conn, err := dialPlatform(ctx, address, identity)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := platformv1.NewPlatformServiceClient(conn)
	if _, err := client.DeleteDomainBinding(ctx, &platformv1.DeleteDomainBindingRequest{Hostname: fixture.Hostname}); err != nil {
		return fmt.Errorf("delete multi-replica domain: %w", err)
	}
	if _, err := client.DeleteService(ctx, &platformv1.DeleteServiceRequest{ServiceId: fixture.ServiceID}); err != nil {
		return fmt.Errorf("delete multi-replica service: %w", err)
	}
	return waitForServiceDeletion(ctx, ctx, client, fixture.EnvironmentID, fixture.ServiceID)
}

func readSingletonLease(ctx context.Context, keyPath, host string) (singletonLease, error) {
	output, err := runRemoteCommand(ctx, keyPath, host, `cockroach sql --insecure --host=127.0.0.1:26257 --format=tsv --execute "SELECT holder_id, fencing_token FROM control_plane_leases WHERE name = 'control-plane-singleton'"`)
	if err != nil {
		return singletonLease{}, err
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		fields := strings.Fields(lines[index])
		if len(fields) != 2 {
			continue
		}
		token, parseErr := strconv.ParseInt(fields[1], 10, 64)
		if parseErr == nil && fields[0] != "holder_id" {
			return singletonLease{Holder: fields[0], Token: token}, nil
		}
	}
	return singletonLease{}, fmt.Errorf("singleton lease row not found in %q", strings.TrimSpace(string(output)))
}

func waitForSingletonLease(ctx context.Context, keyPath, host, previousHolder string) (singletonLease, error) {
	var current singletonLease
	err := testutil.Poll(ctx, testutil.PollConfig{Timeout: time.Minute, Interval: time.Second}, func(ctx context.Context) (bool, error) {
		lease, err := readSingletonLease(ctx, keyPath, host)
		if err != nil {
			return false, nil
		}
		if lease.Holder == "" || lease.Holder == previousHolder {
			return false, nil
		}
		current = lease
		return true, nil
	})
	return current, err
}

func ingressRequestCount(ctx context.Context, keyPath, host string) (int64, error) {
	output, err := runRemoteCommand(ctx, keyPath, host, "test -f /var/lib/ebpf-wg-mesh/ingress-probe/requests.log && wc -l < /var/lib/ebpf-wg-mesh/ingress-probe/requests.log")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(output))
	for index := len(fields) - 1; index >= 0; index-- {
		count, parseErr := strconv.ParseInt(fields[index], 10, 64)
		if parseErr == nil {
			return count, nil
		}
	}
	return 0, fmt.Errorf("parse ingress request count %q", strings.TrimSpace(string(output)))
}

func waitForIngressTakeover(ctx context.Context, keyPath, host string, previousCount int64, hostname string) error {
	command := fmt.Sprintf(
		"test $(wc -l < /var/lib/ebpf-wg-mesh/ingress-probe/requests.log) -gt %d && grep -Fq %q /var/lib/ebpf-wg-mesh/ingress-probe/latest.json",
		previousCount,
		hostname,
	)
	return waitForRemoteCommand(ctx, keyPath, host, command)
}
