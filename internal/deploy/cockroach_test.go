package deploy

import (
	"strings"
	"testing"
)

func TestNativeDatabaseVerificationCountsVotersAndKeepsUnavailableMembers(t *testing.T) {
	i, _, _ := fixture(3)
	nodes := "id,address,is_live,is_available,ranges_underreplicated,ranges_unavailable,is_decommissioning,gossiped_replicas\n1,10.0.0.1:26257,true,true,0,0,false,10\n2,10.0.0.2:26257,true,true,0,0,false,10\n3,10.0.0.3:26257,true,true,0,0,false,10\n"
	ranges := "range_id,voting_replicas,learner_replicas\n1,\"{1,2,3}\",{}\n"
	status, healthy, err := parseDatabaseStatus(i, []byte(nodes), []byte(ranges))
	if err != nil || !healthy || !status.Replicated {
		t.Fatal(status, healthy, err)
	}
	pending := strings.Replace(ranges, "{1,2,3}", "{1,2}", 1)
	status, _, err = parseDatabaseStatus(i, []byte(nodes), []byte(pending))
	if err != nil || status.Replicated {
		t.Fatal("non-voting or absent replica counted", status, err)
	}
	offline := strings.Replace(nodes, "3,10.0.0.3:26257,true,true", "3,10.0.0.3:26257,false,false", 1)
	status, healthy, err = parseDatabaseStatus(i, []byte(offline), []byte(ranges))
	if err != nil || healthy || len(status.Members) != 3 {
		t.Fatal("temporary outage reduced membership", status, err)
	}
	if _, _, err := parseDatabaseStatus(i, []byte("invalid output"), []byte(ranges)); err == nil {
		t.Fatal("unsupported native output accepted")
	}
}
