package bootstrap

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	"ebof-wg-mesh/internal/controlplane/ingressnodes"
)

func RunIngressNodes(args []string) error {
	if len(args) == 0 || (args[0] != "list" && args[0] != "retire") {
		return fmt.Errorf("usage: controlplane ingress-nodes <list|retire> [flags]")
	}
	fs := flag.NewFlagSet("controlplane ingress-nodes "+args[0], flag.ContinueOnError)
	var dbURL, id string
	var trafficStopped bool
	stringFlag(fs, &dbURL, "db-url", "CONTROLPLANE_DATABASE_URL", "", "control plane database")
	fs.StringVar(&id, "node-id", "", "stable ingress node ID to retire")
	fs.BoolVar(&trafficStopped, "traffic-stopped", false, "assert this ingress has been removed from traffic and stopped")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if dbURL == "" {
		return fmt.Errorf("ingress-nodes requires --db-url")
	}
	if args[0] == "retire" && (id == "" || !trafficStopped) {
		return fmt.Errorf("retire requires --node-id and --traffic-stopped after removing the instance from traffic and stopping it")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return err
	}
	defer db.Close()
	registry := ingressnodes.New(db)
	if args[0] == "retire" {
		if err := registry.Retire(ctx, id); err != nil {
			return err
		}
		_, err := fmt.Fprintf(os.Stdout, "retired ingress node %s\n", id)
		return err
	}
	nodes, err := registry.List(ctx)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		fmt.Fprintf(os.Stdout, "%s\t%s\n", node.ID, node.State)
	}
	return nil
}
