package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ebof-wg-mesh/internal/continuity"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 3 {
		return fmt.Errorf("usage: continuity serve|activate|status|finish DIRECTORY [LAUNCH]")
	}
	action, dir := os.Args[1], os.Args[2]
	if action == "serve" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		err := continuity.New(dir).Run(ctx)
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var launch *continuity.Launch
	if action == "activate" || action == "admission" || action == "verify" || action == "seed" {
		if len(os.Args) != 4 {
			return fmt.Errorf("activate requires a private launch file")
		}
		l, err := continuity.Prepare(os.Args[3])
		if err != nil {
			return err
		}
		launch = &l
	}
	if action == "seed" {
		return continuity.New(dir).Seed(*launch)
	}
	if action != "activate" && action != "admission" && action != "status" && action != "finish" && action != "verify" {
		return fmt.Errorf("unknown continuity action")
	}
	request := action
	if action == "verify" {
		request = "status"
	}
	s, err := continuity.Request(ctx, dir, request, launch)
	if err != nil {
		return err
	}
	if action == "verify" && (s.Protocol != continuity.Protocol || s.Active != launch.ID) {
		return fmt.Errorf("running generation differs from desired inputs")
	}
	return json.NewEncoder(os.Stdout).Encode(s)
}
