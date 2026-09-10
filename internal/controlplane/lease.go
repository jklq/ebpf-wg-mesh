package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var errLeaseLost = errors.New("control-plane lease lost")

type leaseClaim struct {
	name   string
	holder string
	token  int64
}

type leaseContextKey struct{}

// LeaseManager gives one replica ownership of a named background job. The
// monotonically increasing token is checked by database.withTx, making lease loss
// a commit fence rather than merely a best-effort leader hint.
const SingletonLeaseName = "control-plane-singleton"

type LeaseManager struct {
	store         *database
	holderID      string
	ttl           time.Duration
	retryInterval time.Duration
	advertise     string
	onUnfenced    func()
	onFenced      func()
}

func NewLeaseManager(store *database, ttl, retryInterval time.Duration) *LeaseManager {
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	if retryInterval <= 0 {
		retryInterval = time.Second
	}
	return &LeaseManager{store: store, holderID: uuid.NewString(), ttl: ttl, retryInterval: retryInterval}
}

func (m *LeaseManager) SetAdvertise(addr string) {
	if m != nil {
		m.advertise = strings.TrimSpace(addr)
	}
}

func (m *LeaseManager) SetFenceHooks(unfenced, fenced func()) {
	if m != nil {
		m.onUnfenced = unfenced
		m.onFenced = fenced
	}
}

func (m *LeaseManager) Lookup(ctx context.Context, name string) (held bool, advertiseAddr string, err error) {
	if m == nil || m.store == nil || name == "" {
		return false, "", nil
	}
	var holder, addr string
	err = m.store.db.QueryRowContext(ctx, `
		SELECT holder_id, advertise_addr FROM control_plane_leases
		 WHERE name = $1 AND expires_at > statement_timestamp()`, name).Scan(&holder, &addr)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	addr = strings.TrimSpace(addr)
	if holder == m.holderID {
		return true, addr, nil
	}
	if addr == "" || addr == m.advertise {
		return false, "", nil
	}
	return false, addr, nil
}

func (m *LeaseManager) Run(ctx context.Context, name string, job func(context.Context) error) error {
	if m == nil || m.store == nil || job == nil || name == "" {
		return nil
	}
	for ctx.Err() == nil {
		claim, acquired, err := m.acquire(ctx, name)
		if err != nil {
			slog.Warn("acquire control-plane lease failed", "lease", name, "error", err)
			if !waitContext(ctx, jitter(m.retryInterval)) {
				return nil
			}
			continue
		}
		if !acquired {
			if !waitContext(ctx, jitter(m.retryInterval)) {
				return nil
			}
			continue
		}

		leaseCtx, cancel := context.WithCancel(context.WithValue(ctx, leaseContextKey{}, claim))
		jobDone := make(chan error, 1)
		go func() { jobDone <- job(leaseCtx) }()
		renewInterval := m.ttl / 3
		if renewInterval <= 0 {
			renewInterval = time.Millisecond
		}
		renew := time.NewTicker(renewInterval)
		lost := false
		for !lost {
			select {
			case <-ctx.Done():
				cancel()
				renew.Stop()
				_ = m.release(context.Background(), claim)
				<-jobDone
				return nil
			case err := <-jobDone:
				cancel()
				renew.Stop()
				_ = m.release(context.Background(), claim)
				if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, errLeaseLost) {
					return err
				}
				lost = true
			case <-renew.C:
				ok, err := m.renew(ctx, claim)
				if err != nil {
					slog.Warn("renew control-plane lease failed", "lease", name, "error", err)
					if m.onUnfenced != nil {
						m.onUnfenced()
					}
					continue
				}
				if !ok {
					cancel()
					renew.Stop()
					<-jobDone
					lost = true
				} else if m.onFenced != nil {
					m.onFenced()
				}
			}
		}
		cancel()
	}
	return nil
}

func (m *LeaseManager) acquireUntil(ctx context.Context, name string) (leaseClaim, error) {
	for {
		claim, acquired, err := m.acquire(ctx, name)
		if err != nil {
			return leaseClaim{}, err
		}
		if acquired {
			return claim, nil
		}
		if !waitContext(ctx, jitter(m.retryInterval)) {
			return leaseClaim{}, ctx.Err()
		}
	}
}

func (m *LeaseManager) hold(ctx context.Context, name string) (context.Context, func(), error) {
	claim, err := m.acquireUntil(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	leaseCtx, cancel := context.WithCancel(context.WithValue(ctx, leaseContextKey{}, claim))
	stop := make(chan struct{})
	done := make(chan struct{})
	renewInterval := m.ttl / 3
	if renewInterval <= 0 {
		renewInterval = time.Millisecond
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				ok, err := m.renew(leaseCtx, claim)
				if err != nil {
					slog.Warn("renew held control-plane lease failed", "lease", name, "error", err)
					if m.onUnfenced != nil {
						m.onUnfenced()
					}
					continue
				}
				if !ok {
					cancel()
					return
				}
				if m.onFenced != nil {
					m.onFenced()
				}
			}
		}
	}()
	var once sync.Once
	release := func() {
		once.Do(func() {
			close(stop)
			cancel()
			<-done
			_ = m.release(context.Background(), claim)
		})
	}
	return leaseCtx, release, nil
}

func jitter(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	spread := base / 5
	if spread <= 0 {
		return base
	}
	return base - spread + time.Duration(rand.Int64N(int64(2*spread)+1))
}

func (m *LeaseManager) acquire(ctx context.Context, name string) (leaseClaim, bool, error) {
	claim := leaseClaim{name: name, holder: m.holderID}
	err := m.store.withTxUnfenced(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			INSERT INTO control_plane_leases(name, holder_id, fencing_token, advertise_addr, expires_at, updated_at)
			VALUES ($1, $2, 1, $4, statement_timestamp() + $3::INT8 * INTERVAL '1 microsecond', statement_timestamp())
			ON CONFLICT(name) DO UPDATE SET
				holder_id = excluded.holder_id,
				advertise_addr = excluded.advertise_addr,
				fencing_token = CASE
					WHEN control_plane_leases.holder_id = excluded.holder_id
					 AND control_plane_leases.expires_at > statement_timestamp()
					THEN control_plane_leases.fencing_token
					ELSE control_plane_leases.fencing_token + 1
				END,
				expires_at = excluded.expires_at,
				updated_at = excluded.updated_at
			WHERE control_plane_leases.holder_id = excluded.holder_id
			   OR control_plane_leases.expires_at <= statement_timestamp()
			RETURNING fencing_token`, name, m.holderID, m.ttl.Microseconds(), m.advertise).Scan(&claim.token)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return leaseClaim{}, false, nil
	}
	return claim, err == nil, err
}

func (m *LeaseManager) renew(ctx context.Context, claim leaseClaim) (bool, error) {
	result, err := m.store.db.ExecContext(ctx, `
		UPDATE control_plane_leases
		   SET expires_at = statement_timestamp() + $1::INT8 * INTERVAL '1 microsecond', updated_at = statement_timestamp(), advertise_addr = $5
		 WHERE name = $2 AND holder_id = $3 AND fencing_token = $4
		   AND expires_at > statement_timestamp()`, m.ttl.Microseconds(), claim.name, claim.holder, claim.token, m.advertise)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (m *LeaseManager) release(ctx context.Context, claim leaseClaim) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := m.store.db.ExecContext(ctx, `
		UPDATE control_plane_leases
		   SET expires_at = statement_timestamp(), updated_at = statement_timestamp()
		 WHERE name = $1 AND holder_id = $2 AND fencing_token = $3`, claim.name, claim.holder, claim.token)
	return err
}

func assertLeaseTx(ctx context.Context, tx *sql.Tx) error {
	claim, ok := ctx.Value(leaseContextKey{}).(leaseClaim)
	if !ok {
		return nil
	}
	var valid bool
	if err := tx.QueryRowContext(ctx, `SELECT holder_id = $2 AND fencing_token = $3
		   AND expires_at > statement_timestamp()
		FROM control_plane_leases WHERE name = $1 FOR UPDATE`, claim.name, claim.holder, claim.token).Scan(&valid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", errLeaseLost, claim.name)
		}
		return err
	}
	if !valid {
		return fmt.Errorf("%w: %s", errLeaseLost, claim.name)
	}
	return nil
}

// withLeaseGuard serializes an external side effect with lease takeover. The
// lease row remains locked until fn finishes, so a successor cannot acquire the
// lease and publish a newer external state while the former owner is still able
// to publish an older one. It deliberately does not use the retrying transaction
// helper: an external side effect must never be replayed automatically.
func (s *database) withLeaseGuard(ctx context.Context, fn func() error) error {
	claim, ok := ctx.Value(leaseContextKey{}).(leaseClaim)
	if !ok {
		return fn()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var valid bool
	if err := tx.QueryRowContext(ctx, `SELECT holder_id = $2 AND fencing_token = $3 AND expires_at > statement_timestamp()
		FROM control_plane_leases WHERE name = $1 FOR UPDATE`, claim.name, claim.holder, claim.token).Scan(&valid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", errLeaseLost, claim.name)
		}
		return err
	}
	if !valid {
		return fmt.Errorf("%w: %s", errLeaseLost, claim.name)
	}
	if err := fn(); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *database) liveOwnerAddr(ctx context.Context) (string, error) {
	var addr string
	err := s.db.QueryRowContext(ctx, `SELECT advertise_addr FROM control_plane_leases
		WHERE name = $1 AND expires_at > statement_timestamp()`, SingletonLeaseName).Scan(&addr)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return strings.TrimSpace(addr), err
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
