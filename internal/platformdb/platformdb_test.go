package platformdb

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestWithSchemaPinsSearchPath(t *testing.T) {
	got, err := WithSchema("postgresql://platform@db.example:5432/platform?sslmode=verify-full", SchemaConsole)
	if err != nil || !strings.Contains(got, "search_path=console") {
		t.Fatal(got, err)
	}
	if _, err := WithSchema("mysql://db/platform", SchemaPlatform); err == nil {
		t.Fatal("non-PostgreSQL URL accepted")
	}
	if _, err := WithSchema("postgresql://db/platform", "bad;schema"); err == nil {
		t.Fatal("unsafe schema accepted")
	}
	if _, err := For("cockroachdb"); err == nil {
		t.Fatal("removed provider still resolves")
	}
}

func TestVersionTransition(t *testing.T) {
	if err := ValidateVersionTransition("17.2", "17.4"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateVersionTransition("17.4", "18.0"); err == nil {
		t.Fatal("major transition accepted for in-place restart")
	}
	if err := ValidateVersionTransition("17.4", "17.2"); err == nil {
		t.Fatal("downgrade accepted")
	}
	if MajorVersion("17.4 (Debian 17.4-1)") != 17 {
		t.Fatal("major version parse")
	}
}

func TestWALSegmentOrdering(t *testing.T) {
	a, b := "0000000100000000000000A1", "0000000100000000000000A2"
	if cmp, err := CompareWALSegments(a, b); err != nil || cmp >= 0 {
		t.Fatal(cmp, err)
	}
	if _, _, err := ParseWALSegment("not-a-segment"); err == nil {
		t.Fatal("invalid segment accepted")
	}
}

const sampleInfo = `[{
  "name": "platform",
  "backup": [
    {"label":"20260101-000000F","type":"full","error":false,
     "archive":{"start":"000000010000000000000001","stop":"000000010000000000000003"},
     "timestamp":{"start":1767225600,"stop":1767225900},
     "lsn":{"start":"0/2000028","stop":"0/4000028"}},
    {"label":"20260101-000000F_20260101-010000I","prior":"20260101-000000F","type":"incr","error":false,
     "archive":{"start":"000000010000000000000004","stop":"000000010000000000000006"},
     "timestamp":{"start":1767229200,"stop":1767229260},
     "lsn":{"start":"0/4000028","stop":"0/6000028"}}
  ],
  "archive": [{"db":{"id":1},"id":"17-1","min":"000000010000000000000001","max":"000000010000000000000009"}]
}]`

func TestPgBackRestInfoSelection(t *testing.T) {
	info, err := ParseInfo([]byte(sampleInfo), "platform")
	if err != nil || len(info.Backups) != 2 {
		t.Fatal(info, err)
	}
	at := time.Unix(1767229260, 0).UTC()
	best, err := SelectBackup(info, at)
	if err != nil || best.Label != "20260101-000000F_20260101-010000I" {
		t.Fatal(best, err)
	}
	chain, err := ChainLabels(info, best.Label)
	if err != nil || len(chain) != 2 || chain[0] != "20260101-000000F" {
		t.Fatal(chain, err)
	}
	if !ArchiveCovers(info, 1, "000000010000000000000008") || ArchiveCovers(info, 1, "00000001000000000000000A") {
		t.Fatal("archive coverage wrong")
	}
	if _, err := SelectBackup(info, time.Unix(0, 0)); err == nil {
		t.Fatal("backup selected before any backup stopped")
	}
}

func TestPgBackRestCommands(t *testing.T) {
	var got []string
	p := PgBackRest{Binary: "pgbackrest", ConfigFile: "/etc/pgbackrest.conf", Stanza: "platform", Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		got = append([]string{name}, args...)
		return []byte(sampleInfo), nil
	}}
	if _, err := p.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--stanza=platform") || !strings.Contains(joined, "info --output=json") {
		t.Fatal(joined)
	}
	if err := p.Restore(context.Background(), "/data", "20260101-000000F", time.Unix(1767229260, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(got, " ")
	if !strings.Contains(joined, "restore") || !strings.Contains(joined, "--set=20260101-000000F") || !strings.Contains(joined, "--type=time") {
		t.Fatal(joined)
	}
}
