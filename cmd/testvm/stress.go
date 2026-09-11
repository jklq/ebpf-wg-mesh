package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type stressOptions struct {
	Seed             uint64        `json:"seed"`
	Stages           int           `json:"stages"`
	StartStage       int           `json:"start_stage"`
	ServicesPerStage int           `json:"services_per_stage"`
	MaxConcurrency   int           `json:"max_concurrency"`
	HTTPTargets      int           `json:"http_targets"`
	Tenants          int           `json:"tenants"`
	StageDuration    time.Duration `json:"stage_duration_ns"`
	Recovery         time.Duration `json:"recovery_timeout_ns"`
	RPCTimeout       time.Duration `json:"rpc_timeout_ns"`
	MaxP99           time.Duration `json:"max_p99_ns"`
	MaxErrorRate     float64       `json:"max_error_rate"`
}

func registerStressFlags() *stressOptions {
	o := &stressOptions{}
	flag.Uint64Var(&o.Seed, "seed", 1, "replayable stress workload/fault seed")
	flag.IntVar(&o.Stages, "stress-stages", 8, "number of increasing load stages")
	flag.IntVar(&o.StartStage, "stress-start-stage", 1, "skip the load/fault phases of earlier stages, provisioning only their services")
	flag.IntVar(&o.ServicesPerStage, "services-per-stage", 4, "new services per stage")
	flag.IntVar(&o.MaxConcurrency, "max-concurrency", 128, "maximum concurrency for each RPC and HTTP load generator")
	flag.IntVar(&o.HTTPTargets, "http-targets", 4, "concurrent workload HTTP flows, each targeting a distinct service")
	flag.IntVar(&o.Tenants, "tenants", 2, "number of projects/environments")
	flag.DurationVar(&o.StageDuration, "stage-duration", 30*time.Second, "duration of each healthy and fault load window")
	flag.DurationVar(&o.Recovery, "recovery-timeout", 3*time.Minute, "deadline for all invariants to recover after each phase")
	flag.DurationVar(&o.RPCTimeout, "rpc-timeout", 5*time.Second, "individual RPC/HTTP request deadline")
	flag.DurationVar(&o.MaxP99, "max-p99", 2*time.Second, "healthy-window p99 RPC/HTTP latency breaking point")
	flag.Float64Var(&o.MaxErrorRate, "max-error-rate", 0.01, "healthy-window RPC/HTTP error fraction breaking point")
	return o
}

func (o stressOptions) validate() error {
	if o.Stages < 1 || o.Stages > 32 || o.ServicesPerStage < 1 || o.ServicesPerStage > 1024 || o.Stages*o.ServicesPerStage > 8192 || o.MaxConcurrency < 1 || o.MaxConcurrency > 4096 || o.HTTPTargets < 1 || o.HTTPTargets > 256 || o.Tenants < 2 || o.Tenants > 128 {
		return errors.New("stress limits: stages 1..32, services/stage 1..1024 (total <=8192), concurrency 1..4096, http targets 1..256, tenants 2..128")
	}
	if o.StageDuration < time.Second || o.StageDuration > 10*time.Minute || o.Recovery < time.Second || o.Recovery > 15*time.Minute || o.RPCTimeout <= 0 || o.RPCTimeout > time.Minute || o.MaxP99 <= 0 || o.MaxErrorRate < 0 || o.MaxErrorRate > 1 || math.IsNaN(o.MaxErrorRate) {
		return errors.New("invalid stress duration or error threshold")
	}
	if o.StartStage < 1 || o.StartStage > o.Stages {
		return fmt.Errorf("stress-start-stage must be between 1 and %d", o.Stages)
	}
	return nil
}

type stressFault struct {
	Kind   string `json:"kind"`
	Target string `json:"target,omitempty"`
}

type stressStage struct {
	Number      int           `json:"number"`
	Services    int           `json:"services"`
	Concurrency int           `json:"concurrency"`
	Faults      []stressFault `json:"faults,omitempty"`
	Updates     []int         `json:"update_order"`
}

// stressSchedule produces a seeded, replayable ramp. The first stage is
// fault-free. Each later stage runs one primary fault. From the third stage on,
// every third stage compounds a second fault from a different failure domain
// (never two faults on the same host), so multi-fault recovery is exercised by
// the default campaign.
func stressSchedule(o stressOptions, agents []string) []stressStage {
	rng := rand.New(rand.NewPCG(o.Seed, 0x737472657373))
	agents = append([]string(nil), agents...)
	sort.Strings(agents)
	var core, extended []stressFaultKind
	for _, kind := range stressFaultKinds {
		if stressCoreFaults[kind.Name] {
			core = append(core, kind)
		} else {
			extended = append(extended, kind)
		}
	}
	shuffleKinds := func(kinds []stressFaultKind) {
		rng.Shuffle(len(kinds), func(i, j int) { kinds[i], kinds[j] = kinds[j], kinds[i] })
	}
	shuffleKinds(core)
	shuffleKinds(extended)
	kinds := append(append([]stressFaultKind(nil), core...), extended...)
	stages := make([]stressStage, o.Stages)
	concurrency := 1
	for i := range stages {
		stage := stressStage{Number: i + 1, Services: (i + 1) * o.ServicesPerStage, Concurrency: concurrency}
		if i > 0 {
			primary := kinds[(i-1)%len(kinds)]
			primaryTarget := stressFaultTarget(primary, agents, rng)
			stage.Faults = append(stage.Faults, stressFault{Kind: primary.Name, Target: primaryTarget})
			if (i+1)%3 == 0 {
				if companion, ok := stressCompanion(kinds, primary, primaryTarget, agents, rng); ok {
					stage.Faults = append(stage.Faults, companion)
				}
			}
		}
		stage.Updates = rng.Perm(stage.Services)
		stages[i] = stage
		if concurrency < o.MaxConcurrency {
			concurrency = min(concurrency*2, o.MaxConcurrency)
		}
	}
	return stages
}

func stressFaultTarget(kind stressFaultKind, agents []string, rng *rand.Rand) string {
	if kind.HostScope != "agent" || len(agents) == 0 {
		return "controlplane"
	}
	return agents[rng.IntN(len(agents))]
}

// stressFaultTargetAvoiding picks an agent-scoped target that is not the given
// host so a companion fault lands in a different failure domain.
func stressFaultTargetAvoiding(kind stressFaultKind, agents []string, avoid string, rng *rand.Rand) string {
	if kind.HostScope != "agent" {
		return "controlplane"
	}
	candidates := make([]string, 0, len(agents))
	for _, agent := range agents {
		if agent != avoid {
			candidates = append(candidates, agent)
		}
	}
	if len(candidates) == 0 {
		return stressFaultTarget(kind, agents, rng)
	}
	return candidates[rng.IntN(len(candidates))]
}

// stressFaultResolvedHost maps a fault and its target to the host it actually
// runs against. Control-plane and database faults are colocated; agent faults
// run on the specific target agent.
func stressFaultResolvedHost(kind stressFaultKind, target string) string {
	if kind.HostScope != "agent" || target == "" {
		return "controlplane"
	}
	return target
}

func stressCompanion(kinds []stressFaultKind, primary stressFaultKind, primaryTarget string, agents []string, rng *rand.Rand) (stressFault, bool) {
	primaryHost := stressFaultResolvedHost(primary, primaryTarget)
	start := rng.IntN(len(kinds))
	for offset := 0; offset < len(kinds); offset++ {
		candidate := kinds[(start+offset)%len(kinds)]
		if candidate.Scope == primary.Scope {
			continue
		}
		if candidate.HostScope != "agent" {
			if primaryHost == "controlplane" {
				continue
			}
			return stressFault{Kind: candidate.Name, Target: "controlplane"}, true
		}
		if len(agents) == 0 {
			continue
		}
		target := stressFaultTargetAvoiding(candidate, agents, primaryHost, rng)
		if stressFaultResolvedHost(candidate, target) == primaryHost {
			continue
		}
		return stressFault{Kind: candidate.Name, Target: target}, true
	}
	return stressFault{}, false
}

type stressService struct {
	Index        int
	ID           string
	Environment  string
	Marker       string
	Alternatives []string
	Revision     int64
}

func (s stressService) spec(marker string) *platformv1.ServiceSpec {
	if s.Index%2 == 1 {
		return dualStackHTTPServiceSpec(marker)
	}
	return inMemoryHTTPServiceSpec(marker)
}

type stressEvent struct {
	At     time.Time `json:"at"`
	Stage  int       `json:"stage"`
	Kind   string    `json:"kind"`
	Detail any       `json:"detail"`
}
type stressTrace struct {
	mu   sync.Mutex
	file *os.File
	err  error
}

func (t *stressTrace) record(stage int, kind string, detail any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err == nil {
		t.err = json.NewEncoder(t.file).Encode(stressEvent{time.Now().UTC(), stage, kind, detail})
	}
}

func (t *stressTrace) writeError() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// A bounded histogram retains every sample without memory growth during saturation.
// Buckets are 1ms through 60s; percentiles are rounded upward to the next millisecond.
type stressStats struct {
	Requests         int64            `json:"requests"`
	Errors           int64            `json:"errors"`
	Codes            map[string]int64 `json:"codes"`
	Seconds          float64          `json:"seconds"`
	RPS              float64          `json:"requests_per_second"`
	P50MS            int              `json:"p50_ms"`
	P95MS            int              `json:"p95_ms"`
	P99MS            int              `json:"p99_ms"`
	WatchRegressions int64            `json:"watch_index_regressions,omitempty"`
	WatchForeign     int64            `json:"watch_foreign_services,omitempty"`
	buckets          [60001]int64
	mu               sync.Mutex
}

func (s *stressStats) add(elapsed time.Duration, err error) {
	s.addOperation("", elapsed, err)
}

// addOperation records a sample under an operation label so a latency or error
// breaking point can be attributed to a specific RPC kind.
func (s *stressStats) addOperation(operation string, elapsed time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests++
	s.buckets[min(60000, max(0, int(math.Ceil(float64(elapsed)/float64(time.Millisecond)))))]++
	if err != nil {
		s.Errors++
	}
	if s.Codes == nil {
		s.Codes = make(map[string]int64)
	}
	code := status.Code(err).String()
	if operation != "" {
		code = operation + "/" + code
	}
	s.Codes[code]++
}

func (s *stressStats) addWatchAnomaly(regression, foreign bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if regression {
		s.WatchRegressions++
	}
	if foreign {
		s.WatchForeign++
	}
}

// fold merges a remote workload report, including its histogram, so aggregated
// percentiles are computed over every sample rather than over per-flow maxima.
func (s *stressStats) fold(report stressHTTPReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests += report.Requests
	s.Errors += report.Errors
	if s.Codes == nil {
		s.Codes = make(map[string]int64)
	}
	for code, count := range report.Codes {
		s.Codes[code] += count
	}
	for i, count := range report.Buckets {
		if i < len(s.buckets) {
			s.buckets[i] += count
		}
	}
}

func (s *stressStats) finish(elapsed time.Duration) {
	s.Seconds = elapsed.Seconds()
	if s.Seconds > 0 {
		s.RPS = float64(s.Requests) / s.Seconds
	}
	percentile := func(p float64) int {
		target := int64(math.Ceil(float64(s.Requests) * p))
		var n int64
		for i, count := range s.buckets {
			n += count
			if n >= target {
				return i
			}
		}
		return 60000
	}
	s.P50MS = percentile(.50)
	s.P95MS = percentile(.95)
	s.P99MS = percentile(.99)
}

func (s *stressStats) check(o stressOptions) error {
	if s.Requests == 0 {
		return errors.New("load produced no requests")
	}
	if s.WatchRegressions > 0 || s.WatchForeign > 0 {
		return fmt.Errorf("watch consistency violations: index regressions=%d foreign services=%d", s.WatchRegressions, s.WatchForeign)
	}
	if float64(s.Errors)/float64(s.Requests) > o.MaxErrorRate {
		return fmt.Errorf("request errors %d/%d exceed %.2f%%", s.Errors, s.Requests, 100*o.MaxErrorRate)
	}
	if time.Duration(s.P99MS)*time.Millisecond > o.MaxP99 {
		return fmt.Errorf("p99 %dms exceeds %s", s.P99MS, o.MaxP99)
	}
	return nil
}

type stressStageResult struct {
	Plan            stressStage  `json:"plan"`
	Healthy         *stressStats `json:"healthy,omitempty"`
	HealthyHTTP     *stressStats `json:"healthy_http,omitempty"`
	Fault           *stressStats `json:"fault,omitempty"`
	FaultHTTP       *stressStats `json:"fault_http,omitempty"`
	OwnerToken      int64        `json:"owner_fencing_token"`
	RecoverySeconds float64      `json:"recovery_seconds"`
	Error           string       `json:"error,omitempty"`
}
type stressReport struct {
	Options     stressOptions        `json:"options"`
	Started     time.Time            `json:"started_at"`
	Finished    time.Time            `json:"finished_at"`
	LastPassed  int                  `json:"last_passed_stage"`
	FirstFailed int                  `json:"first_failed_stage,omitempty"`
	Error       string               `json:"error,omitempty"`
	Stages      []*stressStageResult `json:"stages"`
}

func runStressScenario(ctx context.Context, o stressOptions, artifacts string, identity clientIdentity, key string, hosts map[string]hostInfo, repoRoot string) (runErr error) {
	traceFile, err := os.OpenFile(filepath.Join(artifacts, "stress-events.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer traceFile.Close()
	trace := &stressTrace{file: traceFile}
	report := stressReport{Options: o, Started: time.Now().UTC()}
	defer func() {
		report.Finished = time.Now().UTC()
		if runErr != nil {
			report.Error = runErr.Error()
		}
		if traceErr := trace.writeError(); traceErr != nil {
			runErr = errors.Join(runErr, traceErr)
			report.Error = runErr.Error()
		}
		if err := writeJSON(filepath.Join(artifacts, "stress-summary.json"), report); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}()
	var agents []string
	for name := range hosts {
		if name != "controlplane" {
			agents = append(agents, name)
		}
	}
	if len(agents) < 2 {
		return fmt.Errorf("stress scenario requires at least two agent hosts, have %d", len(agents))
	}
	// Verify the configured data-plane path before attributing cross-agent
	// failures to workload behavior.
	if err := verifyStressUnderlay(ctx, key, hosts, agents); err != nil {
		return err
	}
	schedule := stressSchedule(o, agents)
	if err := writeJSON(filepath.Join(artifacts, "stress-plan.json"), schedule); err != nil {
		return err
	}
	cp := hosts["controlplane"]
	primary, err := dialPlatform(ctx, cp.PublicIPv4+":"+primaryControlPlanePort, identity)
	if err != nil {
		return err
	}
	defer primary.Close()
	replica, err := dialPlatform(ctx, cp.PublicIPv4+":"+replicaControlPlanePort, identity)
	if err != nil {
		return err
	}
	defer replica.Close()
	peers := map[string]grpc.ClientConnInterface{cp.PublicIPv4 + ":" + primaryControlPlanePort: primary, cp.PublicIPv4 + ":" + replicaControlPlanePort: replica}
	clients := []platformv1.PlatformServiceClient{platformv1.NewPlatformServiceClient(stressConnection{primary, peers}), platformv1.NewPlatformServiceClient(stressConnection{replica, peers})}
	rawClients := []platformv1.PlatformServiceClient{platformv1.NewPlatformServiceClient(primary), platformv1.NewPlatformServiceClient(replica)}
	if _, err := waitForAgents(ctx, ctx, clients[0], len(agents), key, hosts); err != nil {
		return err
	}
	var environments []string
	for i := 0; i < o.Tenants; i++ {
		callCtx, cancel := context.WithTimeout(ctx, o.RPCTimeout)
		project, err := clients[0].CreateProject(callCtx, &platformv1.CreateProjectRequest{Name: fmt.Sprintf("stress-%d-%d", o.Seed, i)})
		cancel()
		if err != nil {
			return err
		}
		callCtx, cancel = context.WithTimeout(ctx, o.RPCTimeout)
		envs, err := clients[0].ListEnvironments(callCtx, &platformv1.ListEnvironmentsRequest{ProjectId: project.GetId()})
		cancel()
		if err != nil {
			return err
		}
		if len(envs.GetEnvironments()) != 1 {
			return errors.New("expected one default environment")
		}
		environments = append(environments, envs.GetEnvironments()[0].GetId())
	}
	var services []stressService
	var maxOwnerToken int64
	for _, stage := range schedule {
		result := &stressStageResult{Plan: stage}
		report.Stages = append(report.Stages, result)
		infof("stress seed=%d stage=%d services=%d concurrency=%d faults=%v", o.Seed, stage.Number, stage.Services, stage.Concurrency, stage.Faults)
		trace.record(stage.Number, "stage-start", stage)
		err := func() error {
			for len(services) < stage.Services {
				i := len(services)
				marker := fmt.Sprintf("seed-%d-service-%d-v0", o.Seed, i)
				callCtx, cancel := context.WithTimeout(ctx, o.RPCTimeout)
				s := &stressService{Index: i}
				created, err := clients[i%len(clients)].CreateService(callCtx, &platformv1.CreateServiceRequest{EnvironmentId: environments[i%len(environments)], Service: &platformv1.ServiceInput{Name: fmt.Sprintf("web-%d", i), Spec: s.spec(marker)}})
				cancel()
				trace.record(stage.Number, "create", map[string]any{"ordinal": i, "id": created.GetId(), "code": status.Code(err).String()})
				if err != nil {
					return fmt.Errorf("create service %d: %w", i, err)
				}
				services = append(services, stressService{Index: i, ID: created.GetId(), Environment: created.GetEnvironmentId(), Marker: marker, Revision: created.GetSpecRevision()})
			}
			if err := releaseStress(ctx, o, clients[0], environments); err != nil {
				return err
			}
			if stage.Number < o.StartStage {
				// Provision-only stages exist so a later stage has the accumulated
				// services; their load, fault, and invariant phases are skipped.
				trace.record(stage.Number, "stage-provisioned", map[string]any{"services": len(services)})
				return nil
			}
			if err := verifyStress(ctx, o, rawClients, services, key, hosts); err != nil {
				return fmt.Errorf("initial convergence: %w", err)
			}
			// Healthy window: RPC read/watch fanout and the workload data plane
			// run together across every tenant.
			var loadWG sync.WaitGroup
			loadWG.Add(1)
			go func() {
				defer loadWG.Done()
				result.Healthy = stressLoad(ctx, o, stage, clients, environments, services, trace)
			}()
			healthyFlows, planErr := planStressHTTP(ctx, o, clients[0], services, false)
			if planErr != nil {
				loadWG.Wait()
				return fmt.Errorf("plan HTTP load: %w", planErr)
			}
			var httpErr error
			result.HealthyHTTP, httpErr = runStressHTTPFlows(ctx, o, stage, healthyFlows, key, hosts, trace, false, repoRoot)
			loadWG.Wait()
			if httpErr != nil {
				return fmt.Errorf("HTTP load breaking point: %w", httpErr)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := result.Healthy.check(o); err != nil {
				return fmt.Errorf("healthy load breaking point: %w", err)
			}
			if err := result.HealthyHTTP.check(o); err != nil {
				return fmt.Errorf("healthy HTTP breaking point: %w", err)
			}
			if stage.Number > 1 {
				if err := churnStressService(ctx, o, stage, clients, services, trace); err != nil {
					return err
				}
				if err := releaseStress(ctx, o, clients[0], environments); err != nil {
					return err
				}
				if err := verifyStress(ctx, o, rawClients, services, key, hosts); err != nil {
					return fmt.Errorf("churn convergence: %w", err)
				}
			}
			// Concurrent-writer contention on a dedicated service exercises
			// revision conflicts without racing the per-service mutation oracle.
			contendIndex := len(services) - 1
			if len(stage.Faults) == 0 {
				if err := contendStressService(ctx, o, stage, clients, &services[contendIndex], trace); err != nil {
					return err
				}
				if err := releaseStress(ctx, o, clients[0], environments); err != nil {
					return err
				}
				if err := verifyStress(ctx, o, rawClients, services, key, hosts); err != nil {
					return fmt.Errorf("contention convergence: %w", err)
				}
			}
			if len(stage.Faults) > 0 {
				faultStats, faultHTTP, err := runStressFault(ctx, o, stage, clients, services, environments, contendIndex, key, hosts, cp.PublicIPv4, trace, repoRoot)
				result.Fault = faultStats
				result.FaultHTTP = faultHTTP
				if err != nil {
					return err
				}
			}
			recoveryStarted := time.Now()
			if err := releaseStress(ctx, o, clients[0], environments); err != nil {
				return err
			}
			if err := verifyStress(ctx, o, rawClients, services, key, hosts); err != nil {
				return fmt.Errorf("recovery invariant: %w", err)
			}
			lease, err := waitForStressLease(ctx, key, cp.PublicIPv4)
			if err != nil {
				return fmt.Errorf("singleton lease: %w", err)
			}
			if lease.Token < maxOwnerToken {
				return fmt.Errorf("fencing token regressed from %d to %d (holder %s)", maxOwnerToken, lease.Token, lease.Holder)
			}
			maxOwnerToken = lease.Token
			result.OwnerToken = lease.Token
			if err := assertSingleOwner(ctx, o, rawClients, key, cp.PublicIPv4, lease); err != nil {
				return fmt.Errorf("ownership: %w", err)
			}
			if err := verifyStressWatch(ctx, o, rawClients, services); err != nil {
				return fmt.Errorf("watch consistency: %w", err)
			}
			if err := watchDeliveryProbe(ctx, o, stage, rawClients, clients[0], services, 0); err != nil {
				return fmt.Errorf("watch delivery: %w", err)
			}
			// The probe stages its write; discarding it records another revision.
			// Release so the durable revision the next check observes is applied
			// rather than left pending.
			if err := releaseStress(ctx, o, clients[0], environments); err != nil {
				return err
			}
			if err := verifyStress(ctx, o, rawClients, services, key, hosts); err != nil {
				return fmt.Errorf("post-probe invariant: %w", err)
			}
			if err := checkStressLeaks(ctx, o, key, hosts, clients, services); err != nil {
				return fmt.Errorf("resource leak: %w", err)
			}
			result.RecoverySeconds = time.Since(recoveryStarted).Seconds()
			return nil
		}()
		captureStressResources(ctx, key, hosts, artifacts, stage.Number)
		if err != nil {
			result.Error = err.Error()
			report.FirstFailed = stage.Number
			trace.record(stage.Number, "invariant-failed", err.Error())
			return fmt.Errorf("seed %d stage %d (%d services, concurrency %d): %w", o.Seed, stage.Number, stage.Services, stage.Concurrency, err)
		}
		report.LastPassed = stage.Number
		trace.record(stage.Number, "stage-passed", result)
		if err := writeJSON(filepath.Join(artifacts, "stress-summary.json"), report); err != nil {
			return err
		}
	}
	return nil
}

// verifyStressUnderlay waits for every configured WireGuard peer to complete a
// handshake. This tests the actual endpoint family and UDP path selected by the
// agent instead of using ICMP reachability as a proxy.
func verifyStressUnderlay(ctx context.Context, key string, hosts map[string]hostInfo, agents []string) error {
	sort.Strings(agents)
	expectedPeers := len(agents) - 1
	for _, name := range agents {
		host := hosts[name]
		command := fmt.Sprintf("wg show wg0 latest-handshakes | awk -v expected=%d 'NF == 2 { peers++; if ($2 > 0) ready++ } END { exit !(peers == expected && ready == expected) }'", expectedPeers)
		if err := waitForRemoteCommand(ctx, key, host.PublicIPv4, command); err != nil {
			return fmt.Errorf("wireguard mesh did not establish on %s with %d peers: %w; treating as infrastructure failure", name, expectedPeers, err)
		}
	}
	return nil
}

// runStressFault injects every fault in a stage, runs read/watch and workload
// load plus per-service mutations and contention, heals, and waits for each
// fault's readiness condition. Data-plane-safe faults must keep workload HTTP
// within threshold; agent-targeting faults may disrupt it but must recover.
func runStressFault(ctx context.Context, o stressOptions, stage stressStage, clients []platformv1.PlatformServiceClient, services []stressService, environments []string, contendIndex int, key string, hosts map[string]hostInfo, controlPlaneIP string, trace *stressTrace, repoRoot string) (faultStats, faultHTTP *stressStats, err error) {
	ownerService, err := stressOwnerService(ctx, key, controlPlaneIP)
	if err != nil {
		return nil, nil, err
	}
	dataPlaneSafe := true
	for _, fault := range stage.Faults {
		if !stressFaultIsDataPlaneSafe(fault.Kind) {
			dataPlaneSafe = false
		}
	}
	// Resolve the workload plan while the control plane is still healthy, then
	// keep its source/target services out of the mutation set so the content
	// check stays valid across the fault window.
	httpServices := make([]stressService, 0, len(services)-1)
	for i := range services {
		if i != contendIndex {
			httpServices = append(httpServices, services[i])
		}
	}
	flows, err := planStressHTTP(ctx, o, clients[0], httpServices, !dataPlaneSafe)
	if err != nil {
		return nil, nil, fmt.Errorf("plan fault-window HTTP load: %w", err)
	}
	reserved := map[int]bool{contendIndex: true}
	reserveHTTPFlows(flows, reserved)
	steps, err := stressFaultSteps(hosts, ownerService, stage.Faults)
	if err != nil {
		return nil, nil, err
	}
	// The undo is registered before injection, including when SSH fails ambiguously.
	healed := make([]bool, len(steps))
	defer func() {
		for i := len(steps) - 1; i >= 0; i-- {
			if healed[i] {
				continue
			}
			healCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			out, herr := runRemoteCommand(healCtx, key, hosts[steps[i].Host].PublicIPv4, steps[i].Heal())
			cancel()
			trace.record(stage.Number, "emergency-heal", map[string]any{"kind": steps[i].Kind, "error": fmt.Sprint(herr), "output": string(out)})
		}
	}()
	for _, step := range steps {
		callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		out, ierr := runRemoteCommand(callCtx, key, hosts[step.Host].PublicIPv4, step.Inject)
		cancel()
		trace.record(stage.Number, "fault-injected", map[string]any{"fault": step.Kind, "target_host": step.Host, "owner_service": ownerService, "output": string(out), "error": fmt.Sprint(ierr)})
		if ierr != nil {
			return nil, nil, fmt.Errorf("inject %s on %s: %w: %s", step.Kind, step.Host, ierr, out)
		}
	}
	// Keep the mutation oracle disjoint from the contended service and from the
	// services carrying the workload content check.
	mutationOrder := make([]int, 0, len(services))
	for _, index := range stage.Updates {
		if !reserved[index] {
			mutationOrder = append(mutationOrder, index)
		}
	}
	updated := append([]stressService(nil), services...)
	var wg sync.WaitGroup
	var httpErr, mutationErr, contendErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		faultStats = stressLoad(ctx, o, stage, clients, environments, services, trace)
	}()
	go func() {
		defer wg.Done()
		faultHTTP, httpErr = runStressHTTPFlows(ctx, o, stage, flows, key, hosts, trace, !dataPlaneSafe, repoRoot)
	}()
	mutationErr = mutateStress(ctx, o, stage, clients, updated, mutationOrder, trace)
	contendErr = contendStressService(ctx, o, stage, clients, &updated[contendIndex], trace)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return faultStats, faultHTTP, err
	}
	for i := len(steps) - 1; i >= 0; i-- {
		callCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, herr := runRemoteCommand(callCtx, key, hosts[steps[i].Host].PublicIPv4, steps[i].Heal())
		cancel()
		trace.record(stage.Number, "fault-healed", map[string]any{"kind": steps[i].Kind, "error": fmt.Sprint(herr), "output": string(out)})
		if herr != nil {
			return faultStats, faultHTTP, fmt.Errorf("heal %s: %w: %s", steps[i].Kind, herr, out)
		}
		healed[i] = true
	}
	for _, step := range steps {
		if step.Ready == "" {
			continue
		}
		if rerr := waitForRemoteCommand(ctx, key, hosts[step.Host].PublicIPv4, step.Ready); rerr != nil {
			return faultStats, faultHTTP, fmt.Errorf("fault %s did not recover: %w", step.Kind, rerr)
		}
	}
	if mutationErr != nil {
		return faultStats, faultHTTP, mutationErr
	}
	if contendErr != nil {
		return faultStats, faultHTTP, contendErr
	}
	copy(services, updated)
	if httpErr != nil {
		return faultStats, faultHTTP, httpErr
	}
	if faultStats == nil || faultStats.Requests == 0 {
		return faultStats, faultHTTP, fmt.Errorf("control-plane load made no requests during %v", stage.Faults)
	}
	if dataPlaneSafe {
		if err := faultHTTP.check(o); err != nil {
			return faultStats, faultHTTP, fmt.Errorf("data plane did not survive %v: %w", stage.Faults, err)
		}
	}
	return faultStats, faultHTTP, nil
}

func stressSpec(marker string) *platformv1.ServiceSpec {
	return inMemoryHTTPServiceSpec(marker)
}

func releaseStress(ctx context.Context, o stressOptions, client platformv1.PlatformServiceClient, environments []string) error {
	recoveryCtx, cancel := context.WithTimeout(ctx, o.Recovery)
	defer cancel()
	for _, env := range environments {
		err := testutil.Poll(recoveryCtx, testutil.PollConfig{Timeout: o.Recovery, Interval: time.Second}, func(ctx context.Context) (bool, error) {
			callCtx, cancel := context.WithTimeout(ctx, o.RPCTimeout)
			defer cancel()
			_, err := client.ReleaseEnvironment(callCtx, &platformv1.ReleaseEnvironmentRequest{EnvironmentId: env})
			if status.Code(err) == codes.Unavailable || status.Code(err) == codes.DeadlineExceeded {
				return false, nil
			}
			return err == nil, err
		})
		if err != nil {
			return fmt.Errorf("release environment: %w", err)
		}
	}
	return nil
}

// stressAmbiguous reports whether a read outcome is transient and worth
// retrying: the operation may succeed on a later attempt.
func stressAmbiguous(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Aborted, codes.Unknown, codes.ResourceExhausted, codes.Internal:
		return true
	default:
		return false
	}
}

// stressMayHaveCommitted reports whether a mutation outcome is genuinely
// unknown: the write may have committed even though the call failed, so the
// oracle must allow the attempted value. Aborted is excluded because it means
// the write lost an optimistic-concurrency race and definitively did not commit.
func stressMayHaveCommitted(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Unknown, codes.ResourceExhausted, codes.Internal:
		return true
	default:
		return false
	}
}

func mutateStress(ctx context.Context, o stressOptions, stage stressStage, clients []platformv1.PlatformServiceClient, services []stressService, order []int, trace *stressTrace) error {
	// One outstanding write per service. A lost response is ambiguous, never retried;
	// reconciliation accepts only a prior value or an exact attempted value.
	mutationCtx, cancel := context.WithTimeout(ctx, o.StageDuration)
	defer cancel()
	for n, index := range order {
		if mutationCtx.Err() != nil {
			return fmt.Errorf("mutation stage ended after %d of %d updates: %w", n, len(order), mutationCtx.Err())
		}
		s := &services[index]
		next := fmt.Sprintf("seed-%d-service-%d-v%d", o.Seed, index, stage.Number)
		callCtx, cancel := context.WithTimeout(mutationCtx, o.RPCTimeout)
		updated, err := clients[n%2].UpdateService(callCtx, &platformv1.UpdateServiceRequest{ServiceId: s.ID, Service: &platformv1.ServiceUpdate{Spec: s.spec(next)}})
		cancel()
		trace.record(stage.Number, "update", map[string]any{"id": s.ID, "marker": next, "code": status.Code(err).String(), "revision": updated.GetSpecRevision()})
		if err == nil {
			s.Marker = next
			s.Revision = updated.GetSpecRevision()
			s.Alternatives = nil
			continue
		}
		// Preserve possibly-committed alternatives in the oracle until recovery
		// resolves them through durable reads. An optimistic-concurrency loss is
		// definitive: the write did not commit, so it is not a candidate value.
		if stressMayHaveCommitted(err) {
			s.Alternatives = append(s.Alternatives, next)
			continue
		}
		if status.Code(err) == codes.Aborted {
			continue
		}
		return fmt.Errorf("update rejected unexpectedly: %w", err)
	}
	return nil
}

// contendStressService issues concurrent conflicting updates to a single service
// from both replicas. Every attempt must either succeed or be ambiguous; after
// convergence the committed marker must be one of the attempted values.
func contendStressService(ctx context.Context, o stressOptions, stage stressStage, clients []platformv1.PlatformServiceClient, service *stressService, trace *stressTrace) error {
	attempts := 4
	markers := make([]string, attempts)
	for i := range markers {
		markers[i] = fmt.Sprintf("seed-%d-service-%d-contend-%d-%d", o.Seed, service.Index, stage.Number, i)
	}
	results := make([]error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			callCtx, cancel := context.WithTimeout(ctx, o.RPCTimeout)
			defer cancel()
			_, results[i] = clients[i%len(clients)].UpdateService(callCtx, &platformv1.UpdateServiceRequest{ServiceId: service.ID, Service: &platformv1.ServiceUpdate{Spec: service.spec(markers[i])}})
		}(i)
	}
	wg.Wait()
	for i, err := range results {
		code := status.Code(err)
		trace.record(stage.Number, "contend", map[string]any{"id": service.ID, "marker": markers[i], "code": code.String()})
		switch {
		case err == nil:
			service.Alternatives = append(service.Alternatives, markers[i])
		case stressMayHaveCommitted(err):
			service.Alternatives = append(service.Alternatives, markers[i])
		case code == codes.Aborted:
			// Lost an optimistic-concurrency race; definitively not committed.
		default:
			return fmt.Errorf("contention update rejected unexpectedly: %w", err)
		}
	}
	// The service may still serve its pre-contention marker until the durable
	// readers converge; the prior marker stays an allowed value in the oracle.
	return nil
}

func stressLoad(ctx context.Context, o stressOptions, stage stressStage, clients []platformv1.PlatformServiceClient, environments []string, services []stressService, trace *stressTrace) *stressStats {
	loadCtx, cancel := context.WithTimeout(ctx, o.StageDuration)
	defer cancel()
	stats := &stressStats{Codes: make(map[string]int64)}
	started := time.Now()
	var wg sync.WaitGroup
	for worker := 0; worker < stage.Concurrency; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for iteration := 0; loadCtx.Err() == nil; iteration++ {
				client := clients[(worker+iteration)%len(clients)]
				env := environments[(worker+iteration)%len(environments)]
				service := services[(worker+iteration)%len(services)]
				// Bound each request by the scenario context, not the stage
				// window: deriving from loadCtx would shrink the request deadline
				// toward the window edge and produce spurious end-of-window
				// DeadlineExceeded samples.
				callCtx, cancel := context.WithTimeout(ctx, o.RPCTimeout)
				start := time.Now()
				var err error
				operation := ""
				switch (worker + iteration) % 4 {
				case 0:
					operation = "get"
					_, err = client.GetService(callCtx, &platformv1.GetServiceRequest{ServiceId: service.ID})
				case 1:
					operation = "status"
					_, err = client.GetServiceStatus(callCtx, &platformv1.GetServiceStatusRequest{ServiceId: service.ID})
				case 2:
					operation = "list"
					_, err = client.ListServices(callCtx, &platformv1.ListServicesRequest{EnvironmentId: env})
				case 3:
					operation = "watch"
					// The measured watch request includes its intentional one-second wait.
					snapshot, e := client.ListServices(callCtx, &platformv1.ListServicesRequest{EnvironmentId: env})
					if e != nil {
						err = e
						break
					}
					watch, e := client.ListServices(callCtx, &platformv1.ListServicesRequest{EnvironmentId: env, WaitIndex: snapshot.GetIndex(), WaitTimeoutSeconds: 1})
					err = e
					if e == nil {
						foreign := false
						for _, svc := range watch.GetServices() {
							if svc.GetEnvironmentId() != env {
								foreign = true
								break
							}
						}
						stats.addWatchAnomaly(watch.GetIndex() < snapshot.GetIndex(), foreign)
					}
				}
				elapsed := time.Since(start)
				cancel()
				stats.addOperation(operation, elapsed, err)
			}
		}(worker)
	}
	wg.Wait()
	stats.finish(time.Since(started))
	trace.record(stage.Number, "load-window", stats)
	return stats
}
