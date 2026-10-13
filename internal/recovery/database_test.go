package recovery

import "testing"

// FreezeDatabase must pin exactly the selected backup chain's objects and
// the frozen WAL range — no other backup's files, no later WAL — because
// the protected manifest is the only thing a restore may trust.
func TestRepositoryObjectSelection(t *testing.T) {
	b := Database{Stanza: "platform", BackupLabel: "F1_20260101-010000I", Chain: []string{"F1", "F1_20260101-010000I"},
		WALStart: "000000010000000000000001", WALStop: "000000010000000000000005"}
	cases := map[string]bool{
		"production/database/backup/platform/backup.info":                                             true,
		"production/database/backup/platform/backup.info.copy":                                        true,
		"production/database/backup/platform/F1/backup.manifest":                                      true,
		"production/database/backup/platform/F1_20260101-010000I/backup.manifest.gz":                  true,
		"production/database/backup/platform/F2/backup.manifest":                                      false,
		"production/database/backup/platform/latest":                                                  false,
		"production/database/archive/platform/16-1/0000000100000000/000000010000000000000003-aaaa.gz": true,
		"production/database/archive/platform/16-1/0000000100000000/000000010000000000000005-bbbb":    true,
		"production/database/archive/platform/16-1/0000000100000000/000000010000000000000006-cccc.gz": false,
		"production/database/archive/platform/16-1/00000001.history":                                  true,
		"production/database/points/production/manifest":                                              false,
	}
	for key, want := range cases {
		if got := repositoryObjectSelected(b, key); got != want {
			t.Fatalf("%s: selected=%v want %v", key, got, want)
		}
	}
	prefixes := RepositoryObjectPrefixes(b, "production/database")
	if len(prefixes) != 2 || prefixes[0] != "production/database/backup/platform/" || prefixes[1] != "production/database/archive/platform/" {
		t.Fatal("repository prefixes", prefixes)
	}
}
