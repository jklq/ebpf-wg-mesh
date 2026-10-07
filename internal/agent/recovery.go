package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/reconciliation"
	"go.etcd.io/bbolt"
)

var installationIDKey = []byte("installation_identity")
var recoveryGenerationKey = []byte("recovery_generation")
var checkpointRequiredKey = []byte("recovery_checkpoint_required")

type generationCommand interface {
	GetInstallationId() string
	GetRecoveryGeneration() string
	GetAuthorityEpoch() uint64
}

func validateGeneration(meta *bbolt.Bucket, command generationCommand) error {
	if command.GetInstallationId() != string(meta.Get(installationIDKey)) || command.GetRecoveryGeneration() != string(meta.Get(recoveryGenerationKey)) {
		return errors.New("command authority differs from host-admin admission")
	}
	return nil
}

// admitAuthority runs only with an operator-provisioned local file, before
// connecting. Preserve allocation and runtime inventory across the cutover.
func (s *localStateStore) admitAuthority(a reconciliation.Authority) (bool, error) {
	changed := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(localMetaBucket)
		if id := string(meta.Get(installationIDKey)); id != "" && id != a.InstallationID {
			return errors.New("recovery cannot change installation identity")
		}
		if string(meta.Get(recoveryGenerationKey)) == a.Generation {
			return nil
		}
		retiredKey := []byte("retired_generation/" + a.Generation)
		if meta.Get(retiredKey) != nil {
			return errors.New("recovery generation has already been retired")
		}
		if err := retainRecoveryNetwork(tx); err != nil {
			return err
		}
		previous := string(meta.Get(recoveryGenerationKey))
		if previous != "" {
			if err := meta.Put([]byte("retired_generation/"+previous), []byte{1}); err != nil {
				return err
			}
		}
		for key, value := range map[string][]byte{
			string(installationIDKey): []byte(a.InstallationID), string(recoveryGenerationKey): []byte(a.Generation),
			string(clusterIdentityKey): []byte(a.ClusterID), string(checkpointRequiredKey): {1},
			string(initStateKey): []byte(initializationRecovery),
		} {
			if err := meta.Put([]byte(key), value); err != nil {
				return err
			}
		}
		for _, key := range [][]byte{authorityEpochKey, highestEpochKey, cursorKey, nodeConfigVersionKey, credentialsVersionKey, replicasVersionKey, identityRecoveryKey} {
			if err := meta.Delete(key); err != nil {
				return err
			}
		}
		for _, name := range [][]byte{localDesiredBucket, localCredentialsBucket, localObservationsBucket} {
			if err := tx.DeleteBucket(name); err != nil {
				return err
			}
			if _, err := tx.CreateBucket(name); err != nil {
				return err
			}
		}
		changed = true
		return nil
	})
	return changed && err == nil, err
}

func (a *App) admitRecoveryAuthority() error {
	if a.cfg.AuthorityFile == "" {
		if a.cfg.Profile.IsProduction() {
			return errors.New("production agent requires --authority-file from host administration")
		}
		return nil
	}
	admission, err := reconciliation.ReadAuthority(a.cfg.AuthorityFile)
	if err != nil {
		return err
	}
	a.recoveryAuthority = admission
	_, err = a.stateStore.admitAuthority(admission)
	if err != nil {
		return err
	}
	return a.discardPriorGenerationTLS()
}

// The host-admin tool can provision new credentials before restarting an agent.
// Preserve those credentials, but discard an old or interrupted cache even when
// local generation admission committed before the previous process crashed.
func (a *App) discardPriorGenerationTLS() error {
	if a.stateStore == nil {
		return nil
	}
	_, generation := a.stateStore.commandGeneration()
	if generation == "" {
		return nil
	}
	marker, err := os.ReadFile(filepath.Join(a.clientTLSDir(), "generation"))
	if err == nil && string(marker) == generation {
		return nil
	}
	for _, name := range []string{agentCertFileName, agentKeyFileName, agentCAFileName} {
		if err := os.Remove(filepath.Join(a.clientTLSDir(), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("replace recovery client identity: %w", err)
		}
	}
	return nil
}

func (s *localStateStore) commandGeneration() (string, string) {
	var id, generation string
	_ = s.db.View(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(localMetaBucket)
		id, generation = string(meta.Get(installationIDKey)), string(meta.Get(recoveryGenerationKey))
		return nil
	})
	return id, generation
}

func stampReportGeneration(report *agentv1.StatusReport, summary localStateSummary) {
	report.InstallationId, report.RecoveryGeneration = summary.InstallationID, summary.RecoveryGeneration
}
