package certificates

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"ebof-wg-mesh/internal/alarm"
	"ebof-wg-mesh/internal/controlplane/xds"

	"golang.org/x/crypto/acme"
)

const (
	reconcileInterval   = 30 * time.Second
	issueTimeout        = 10 * time.Minute
	convergeTimeout     = 2 * time.Minute
	challengeLifetime   = time.Hour
	firstRetryDelay     = 5 * time.Minute
	maxRetryDelay       = 6 * time.Hour
	versionRetention    = 24 * time.Hour
	pruneInterval       = time.Hour
	maxFailureMessageSz = 512
	// renewalAlarmWindow bounds failure alarms: a failed renewal pages the
	// operator only when the served certificate is missing, expired, or
	// expires within this window. Earlier failures retry silently.
	renewalAlarmWindow = 72 * time.Hour
)

// platformAlarmLevels are the remaining-lifetime thresholds at which the
// externally renewed wildcard pages the operator. Each level fires once
// per certificate.
var platformAlarmLevels = []struct {
	name  string
	after time.Duration
}{
	{"21d", 21 * 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
	{"24h", 24 * time.Hour},
}

// Run issues and renews certificates until ctx ends. Only the live owner runs it.
func (s *Service) Run(ctx context.Context) error {
	if s == nil || (s.issuer == nil && s.platform == nil) {
		<-ctx.Done()
		return nil
	}
	var workers sync.WaitGroup
	defer workers.Wait()
	slots := make(chan struct{}, s.workers)
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	var lastPrune time.Time
	for {
		if err := s.reconcile(ctx, slots, &workers, &lastPrune); err != nil && ctx.Err() == nil {
			slog.Warn("certificate reconcile failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-s.nudge:
		}
	}
}

func (s *Service) reconcile(ctx context.Context, slots chan struct{}, workers *sync.WaitGroup, lastPrune *time.Time) error {
	s.checkPlatform(ctx)
	if s.issuer == nil {
		return nil
	}
	hosts, err := s.hosts.RoutedHostnames(ctx)
	if err != nil {
		return err
	}
	records, err := s.store.ListCertificates(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	if now.Sub(*lastPrune) >= pruneInterval {
		keep := make([]string, 0, len(hosts))
		for _, host := range hosts {
			keep = append(keep, host.Name)
		}
		if err := s.store.PruneCertificates(ctx, keep, now.Add(-versionRetention)); err != nil {
			return fmt.Errorf("prune certificates: %w", err)
		}
		*lastPrune = now
	}
	byHost := make(map[string]Record, len(records))
	for _, record := range records {
		byHost[record.Hostname] = record
	}
	for _, host := range hosts {
		if !s.individualCertificate(host) {
			continue
		}
		record, exists := byHost[host.Name]
		if !due(record, exists, now) {
			continue
		}
		if _, busy := s.inflight.LoadOrStore(host.Name, struct{}{}); busy {
			continue
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			s.inflight.Delete(host.Name)
			return nil
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-slots }()
			defer s.inflight.Delete(host.Name)
			s.issue(ctx, host, record)
		}()
	}
	return nil
}

// due reports whether a hostname needs a new certificate now: it has none, or
// its certificate reached the renewal time, and no retry delay holds it back.
func due(record Record, exists bool, now time.Time) bool {
	if !exists {
		return true
	}
	if now.Before(record.NextAttemptAt) {
		return false
	}
	return record.Fingerprint == "" || !now.Before(record.RenewAt)
}

func (s *Service) issue(ctx context.Context, host Hostname, record Record) {
	if !s.individualCertificate(host) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, issueTimeout)
	defer cancel()
	if host.PlatformHostname != "" && s.verify != nil {
		// The CA would fail the validation anyway. Waiting for DNS does not spend
		// an attempt or the CA's failed-validation budget.
		if err := s.verify(ctx, host.Name, host.PlatformHostname); err != nil {
			slog.Debug("certificate waits for DNS", "hostname", host.Name, "error", err)
			return
		}
	}
	started := s.now()
	issued, err := s.issuer.Issue(ctx, host.Name, solver{s})
	if err == nil {
		err = s.save(ctx, host.Name, issued)
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			// Shutdown or lease loss: the next owner retries without a penalty.
			return
		}
		attempts := record.Attempts + 1
		next := started.Add(retryDelay(attempts, err))
		slog.Warn("certificate issuance failed", "hostname", host.Name, "attempt", attempts, "retry_at", next, "error", err)
		if recordErr := s.store.RecordFailure(context.WithoutCancel(ctx), host.Name, attempts, next, failureMessage(err)); recordErr != nil {
			slog.Warn("record certificate failure", "hostname", host.Name, "error", recordErr)
		}
		s.alarmRenewalFailure(ctx, host.Name, record, attempts, next, err)
		return
	}
	slog.Info("certificate issued", "hostname", host.Name, "duration", s.now().Sub(started))
	s.ingress.RequestSync()
}

func (s *Service) save(ctx context.Context, hostname string, issued Issued) error {
	version, err := ParseVersion(hostname, issued.ChainPEM, issued.KeyPEM)
	if err != nil {
		return fmt.Errorf("issued certificate is unusable: %w", err)
	}
	if err := s.store.SaveIssued(ctx, version, renewAt(version.NotBefore, version.NotAfter)); err != nil {
		return fmt.Errorf("store certificate: %w", err)
	}
	s.keysMu.Lock()
	s.keys[version.Fingerprint] = xds.KeyPair{CertificatePEM: version.ChainPEM, PrivateKeyPEM: version.KeyPEM}
	s.keysMu.Unlock()
	return nil
}

// retryDelay doubles from five minutes up to six hours. Let's Encrypt allows
// five failed validations per hostname and hour; this schedule stays below it.
// A CA rate limit with Retry-After wins when it is longer.
func retryDelay(attempts int, err error) time.Duration {
	delay := firstRetryDelay
	for i := 1; i < attempts && delay < maxRetryDelay; i++ {
		delay *= 2
	}
	delay = min(delay, maxRetryDelay)
	if after, ok := acme.RateLimit(err); ok && after > delay {
		delay = after
	}
	return delay
}

// alarmRenewalFailure pages the operator when a failed attempt leaves a
// hostname without a certificate that comfortably outlives the retry
// schedule. Failures with ample lifetime left stay log-only; the loop
// retries them on its own.
func (s *Service) alarmRenewalFailure(ctx context.Context, hostname string, record Record, attempts int, next time.Time, err error) {
	if !s.alarm.Enabled() {
		return
	}
	now := s.now()
	switch {
	case record.Fingerprint == "":
	case !now.Before(record.NotAfter):
	case record.NotAfter.Sub(now) <= renewalAlarmWindow:
	default:
		return
	}
	expiry := "no certificate is served yet"
	if record.Fingerprint != "" {
		expiry = fmt.Sprintf("current certificate expires at %s", record.NotAfter.UTC().Format(time.RFC3339))
	}
	s.sendAlarm(ctx, alarm.Event{
		Event:   "certificate_renewal_failed",
		Subject: hostname,
		Message: fmt.Sprintf("issuance attempt %d failed: %s; %s; next attempt at %s",
			attempts, failureMessage(err), expiry, next.UTC().Format(time.RFC3339)),
		NextAction: "check DNS, ingress reachability, and the ACME account; renewal keeps retrying alone",
	})
}

// checkPlatform watches the externally renewed wildcard. The platform never
// holds the DNS-01 credential, so it cannot renew this certificate itself;
// instead it pages the operator early enough that a silent external renewal
// failure cannot become an outage. Each threshold fires once per
// certificate, and a fresh owner may repeat one alarm after a handover.
func (s *Service) checkPlatform(ctx context.Context) {
	if s.platform == nil || !s.alarm.Enabled() {
		return
	}
	subject := "*." + s.platformSuffix
	version, err := s.platform.load()
	if err != nil {
		if s.alarmOnce("platform-load:" + failureMessage(err)) {
			s.sendAlarm(ctx, alarm.Event{
				Event:      "platform_certificate_unavailable",
				Subject:    subject,
				Message:    fmt.Sprintf("platform wildcard certificate cannot be loaded: %s", failureMessage(err)),
				NextAction: "repair the deployed wildcard files; generated hostnames have no valid certificate",
			})
		}
		return
	}
	remaining := version.NotAfter.Sub(s.now())
	// Tightest applicable level wins, so an owner that starts late pages
	// once at the real urgency instead of replaying every looser level.
	for i := len(platformAlarmLevels) - 1; i >= 0; i-- {
		level := platformAlarmLevels[i]
		if remaining > level.after {
			continue
		}
		if s.alarmOnce("platform:" + version.Fingerprint + ":" + level.name) {
			s.sendAlarm(ctx, alarm.Event{
				Event:   "platform_certificate_expiring",
				Subject: subject,
				Message: fmt.Sprintf("platform wildcard certificate expires at %s (less than %s left) and external renewal has not replaced it",
					version.NotAfter.UTC().Format(time.RFC3339), level.name),
				NextAction: "check the external DNS-01 renewal job and its deploy hook",
			})
		}
		return
	}
}

// alarmOnce claims key, reporting true only for its first caller.
func (s *Service) alarmOnce(key string) bool {
	s.alarmMu.Lock()
	defer s.alarmMu.Unlock()
	if s.alarmed == nil {
		s.alarmed = map[string]bool{}
	}
	if s.alarmed[key] {
		return false
	}
	s.alarmed[key] = true
	return true
}

func (s *Service) sendAlarm(ctx context.Context, event alarm.Event) {
	if err := s.alarm.Send(context.WithoutCancel(ctx), event); err != nil {
		slog.Warn("send certificate alarm", "event", event.Event, "subject", event.Subject, "error", err)
	}
}

func failureMessage(err error) string {
	var acmeErr *acme.Error
	message := err.Error()
	if errors.As(err, &acmeErr) && acmeErr.Detail != "" {
		message = acmeErr.Detail
	}
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > maxFailureMessageSz {
		message = message[:maxFailureMessageSz]
	}
	return message
}

// solver publishes HTTP-01 challenges through the ingress snapshot.
type solver struct{ s *Service }

// Present stores the challenge, publishes it, and waits until at least one
// Envoy node is known and every known node applied the snapshot. Only then may
// the CA validate: an early validation spends the CA's failure budget.
func (v solver) Present(ctx context.Context, hostname, token, keyAuthorization string) error {
	s := v.s
	if err := s.store.PutChallenge(ctx, xds.Challenge{
		Hostname: hostname, Token: token, KeyAuthorization: keyAuthorization,
	}, s.now().Add(challengeLifetime)); err != nil {
		return fmt.Errorf("store challenge: %w", err)
	}
	if err := s.ingress.Sync(ctx); err != nil {
		return fmt.Errorf("publish challenge: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, convergeTimeout)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		nodes, converged, err := s.ingress.Applied(waitCtx)
		if err == nil && converged && nodes > 0 {
			return nil
		}
		select {
		case <-waitCtx.Done():
			switch {
			case err != nil:
				return fmt.Errorf("wait for ingress to apply challenge: %w", err)
			case nodes == 0:
				return errors.New("no ingress node is connected to serve the challenge")
			default:
				return errors.New("ingress did not apply the challenge in time")
			}
		case <-ticker.C:
		}
	}
}

func (v solver) CleanUp(ctx context.Context, _ string, token string) error {
	if err := v.s.store.DeleteChallenge(context.WithoutCancel(ctx), token); err != nil {
		return err
	}
	v.s.ingress.RequestSync()
	return nil
}
