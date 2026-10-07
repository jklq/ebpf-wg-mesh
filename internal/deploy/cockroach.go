package deploy

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// RunDatabaseStatus uses supported native commands, including voting replica
// information. Learners and non-voters never establish redundancy. A release
// can use this command as its database-verify hook without platform login.
func RunDatabaseStatus(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("platformctl database-status", flag.ContinueOnError)
	var planPath, binary, host, certs, databases string
	var convergence bool
	fs.StringVar(&planPath, "plan", os.Getenv("PLATFORM_PLAN"), "installation plan JSON")
	fs.StringVar(&binary, "binary", "", "absolute path to the pinned CockroachDB executable")
	fs.StringVar(&host, "host", "", "live database address")
	fs.StringVar(&certs, "certs-dir", "", "externally provisioned database CA/client certificates")
	fs.StringVar(&databases, "databases", "system,defaultdb", "all databases containing platform, console and system state")
	fs.BoolVar(&convergence, "require-convergence", false, "fail until every desired member and its voting replicas have converged")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || planPath == "" || !filepath.IsAbs(binary) || host == "" || !filepath.IsAbs(certs) {
		return fmt.Errorf("database-status requires --plan, --binary, --host and --certs-dir")
	}
	b, err := os.ReadFile(planPath)
	if err != nil {
		return err
	}
	var p Plan
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	client := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, binary, append(args, "--host="+host, "--certs-dir="+certs, "--format=csv")...)
		var output limitedBuffer
		cmd.Stdout = &output
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("native CockroachDB inspection failed: %w", err)
		}
		return output.Bytes(), nil
	}
	nodes, err := client("node", "status", "--all", "--timeout=30s")
	if err != nil {
		return err
	}
	var ranges []byte
	for _, name := range strings.Split(databases, ",") {
		if name == "" {
			return fmt.Errorf("empty database name")
		}
		identifier := "\"" + strings.ReplaceAll(name, "\"", "\"\"") + "\""
		raw, err := client("sql", "--execute=SELECT range_id, voting_replicas, learner_replicas FROM [SHOW RANGES FROM DATABASE "+identifier+"]")
		if err != nil {
			return err
		}
		if len(ranges) == 0 {
			ranges = raw
		} else {
			lines := strings.SplitN(string(raw), "\n", 2)
			if len(lines) != 2 {
				return fmt.Errorf("invalid native range response")
			}
			ranges = append(ranges, []byte(lines[1])...)
		}
	}
	status, healthy, err := parseDatabaseStatus(p.Installation, nodes, ranges)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(out).Encode(status); err != nil {
		return err
	}
	if convergence {
		desired := map[string]bool{}
		for _, pl := range p.Placements {
			if pl.Role == Database {
				desired[pl.Host] = true
			}
		}
		for id := range desired {
			if !contains(status.Members, id) {
				return fmt.Errorf("database member %s has not joined", id)
			}
		}
		if !healthy || (len(desired) >= 3 && !status.Replicated) {
			return fmt.Errorf("database replication has not converged")
		}
	}
	return nil
}
func csvRecords(raw []byte, required ...string) ([]map[string]string, error) {
	reader := csv.NewReader(strings.NewReader(string(raw)))
	rows, err := reader.ReadAll()
	if err != nil || len(rows) == 0 {
		return nil, fmt.Errorf("invalid native CSV: %v", err)
	}
	columns := rows[0]
	for _, key := range required {
		if !contains(columns, key) {
			return nil, fmt.Errorf("native CSV missing column %s", key)
		}
	}
	var records []map[string]string
	for _, row := range rows[1:] {
		record := map[string]string{}
		for n, v := range row {
			record[columns[n]] = v
		}
		records = append(records, record)
	}
	return records, nil
}
func parseDatabaseStatus(i Installation, nodes, ranges []byte) (DatabaseStatus, bool, error) {
	status := DatabaseStatus{Replicated: true}
	records, err := csvRecords(nodes, "id", "address", "is_live", "is_available", "ranges_underreplicated", "ranges_unavailable", "is_decommissioning", "gossiped_replicas")
	if err != nil {
		return status, false, err
	}
	addressToHost := map[string]string{}
	for _, h := range i.Hosts {
		addressToHost[h.Network.Address] = h.ID
	}
	nodeHosts := map[string]string{}
	healthy := true
	for _, row := range records {
		address, _, err := net.SplitHostPort(row["address"])
		if err != nil {
			return status, false, err
		}
		id, ok := addressToHost[address]
		if !ok {
			return status, false, fmt.Errorf("observed database node %s has no stable host binding", row["id"])
		}
		replicas, err := strconv.Atoi(row["gossiped_replicas"])
		if err != nil {
			return status, false, err
		}
		if row["is_decommissioning"] == "true" && replicas == 0 {
			continue
		}
		status.Members = append(status.Members, id)
		nodeHosts[row["id"]] = id
		if row["is_live"] != "true" || row["is_available"] != "true" {
			healthy = false
		}
		for _, column := range []string{"ranges_underreplicated", "ranges_unavailable"} {
			count, err := strconv.Atoi(row[column])
			if err != nil {
				return status, false, err
			}
			if count != 0 {
				status.Replicated = false
			}
		}
	}
	records, err = csvRecords(ranges, "range_id", "voting_replicas", "learner_replicas")
	if err != nil {
		return status, false, err
	}
	if len(records) == 0 || len(status.Members) < 3 {
		status.Replicated = false
	}
	seen := map[string]bool{}
	for _, row := range records {
		if seen[row["range_id"]] {
			continue
		}
		seen[row["range_id"]] = true
		rangeStatus := RangeStatus{ID: row["range_id"]}
		hosts := map[string]bool{}
		voters := strings.Trim(row["voting_replicas"], "{}")
		if voters == "" {
			return status, false, fmt.Errorf("range %s has no voting replicas", row["range_id"])
		}
		for _, node := range strings.Split(voters, ",") {
			id, ok := nodeHosts[strings.TrimSpace(node)]
			if !ok {
				status.Replicated = false
				id = "unknown-node-" + strings.TrimSpace(node)
			}
			hosts[id] = true
		}
		for id := range hosts {
			rangeStatus.Voters = append(rangeStatus.Voters, id)
		}
		sort.Strings(rangeStatus.Voters)
		if len(hosts) < 3 || row["learner_replicas"] != "{}" {
			status.Replicated = false
		}
		status.Ranges = append(status.Ranges, rangeStatus)
	}
	sort.Strings(status.Members)
	sort.Slice(status.Ranges, func(a, b int) bool { return status.Ranges[a].ID < status.Ranges[b].ID })
	return status, healthy, nil
}
