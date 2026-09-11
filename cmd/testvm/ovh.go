package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"ebof-wg-mesh/internal/testutil"
	"github.com/ovh/go-ovh/ovh"
)

type ovhOptions struct {
	action, project, region, flavor, image, manifest, endpoint string
	agents                                                     int
	hourlyRate, budget                                         float64
}

func registerOVHFlags() *ovhOptions {
	o := &ovhOptions{}
	flag.StringVar(&o.endpoint, "ovh-endpoint", "", "OVH API endpoint: ovh-eu, ovh-ca, ovh-us (or OVH_ENDPOINT; default ovh-eu)")
	flag.StringVar(&o.action, "ovh-action", "plan", "OVH: catalog, plan, run, or destroy")
	flag.StringVar(&o.project, "ovh-project", "", "OVH project ID (or OVH_CLOUD_PROJECT)")
	flag.StringVar(&o.region, "ovh-region", "GRA11", "OVH region")
	flag.StringVar(&o.flavor, "ovh-flavor", "b3-8", "OVH flavor name, used for all hosts")
	flag.StringVar(&o.image, "ovh-image", "Ubuntu 24.04", "OVH image name")
	flag.StringVar(&o.manifest, "ovh-manifest", "", "existing ovh-resources.json to destroy")
	flag.IntVar(&o.agents, "agents", 2, "number of OVH agent VMs (2..32)")
	flag.Float64Var(&o.hourlyRate, "ovh-hourly-rate", 0, "verified per-VM hourly price including tax, in the same currency as budget")
	flag.Float64Var(&o.budget, "budget", 5, "maximum estimated compute cost for this run, in hourly-rate currency")
	return o
}

type ovhAPI interface {
	GetWithContext(context.Context, string, interface{}) error
	PostWithContext(context.Context, string, interface{}, interface{}) error
	DeleteWithContext(context.Context, string, interface{}) error
}
type ovhFlavor struct {
	ID, Name, Region, OSType string
	Available                bool
	RAM, VCPUs, Quota        int
}
type ovhImage struct{ ID, Name, Region, Status, Visibility string }
type ovhInstance struct {
	ID, Name, Region, Status string
	IPAddresses              []struct {
		IP, Type string
		Version  int
	} `json:"ipAddresses"`
}
type ovhKey struct{ ID, Name string }
type ovhResources struct {
	Endpoint  string   `json:"endpoint"`
	Project   string   `json:"project"`
	RunID     string   `json:"run_id"`
	Region    string   `json:"region"`
	Names     []string `json:"names"`
	Instances []string `json:"instances"`
	SSHKeyID  string   `json:"ssh_key_id"`
	Destroyed bool     `json:"destroyed"`
}
type ovhPlan struct {
	Endpoint      string    `json:"endpoint"`
	RunID         string    `json:"run_id"`
	Project       string    `json:"project"`
	Region        string    `json:"region"`
	Flavor        ovhFlavor `json:"flavor"`
	Image         ovhImage  `json:"image"`
	Hosts         int       `json:"hosts"`
	Timeout       string    `json:"timeout"`
	HourlyRate    float64   `json:"user_supplied_hourly_rate"`
	EstimatedCost float64   `json:"estimated_compute_cost"`
	Budget        float64   `json:"budget"`
}

var safeRunID = regexp.MustCompile(`^vm-[a-z0-9][a-z0-9-]{0,39}$`)

func (o ovhOptions) estimate(timeout time.Duration) (float64, error) {
	if o.agents < 2 || o.agents > 32 {
		return 0, errors.New("agents must be between 2 and 32")
	}
	if timeout <= 0 || timeout > 6*time.Hour {
		return 0, errors.New("timeout must be positive and at most 6h")
	}
	if math.IsNaN(o.hourlyRate) || math.IsInf(o.hourlyRate, 0) || o.hourlyRate <= 0 || math.IsNaN(o.budget) || math.IsInf(o.budget, 0) || o.budget <= 0 {
		return 0, errors.New("positive finite -ovh-hourly-rate and -budget are required; verify the rate in your OVH billing currency")
	}
	// Round each VM up to whole hours, with another hour reserved for cleanup.
	cost := float64(o.agents+1) * o.hourlyRate * (math.Ceil(timeout.Hours()) + 1)
	if cost > o.budget {
		return cost, fmt.Errorf("estimated compute cost %.2f exceeds budget %.2f", cost, o.budget)
	}
	return cost, nil
}

func prepareOVH(ctx context.Context, o *ovhOptions, runID string, timeout time.Duration) (ovhAPI, ovhPlan, error) {
	var plan ovhPlan
	if o.action != "catalog" && o.action != "plan" && o.action != "run" && o.action != "destroy" {
		return nil, plan, errors.New("ovh-action must be catalog, plan, run, or destroy")
	}
	if o.action == "run" || o.action == "plan" {
		if !safeRunID.MatchString(runID) {
			return nil, plan, errors.New("OVH run-id must match vm-[a-z0-9][a-z0-9-]{0,39}")
		}
		cost, err := o.estimate(timeout)
		if err != nil {
			return nil, plan, err
		}
		plan.EstimatedCost = cost
	}
	for _, name := range []string{"OVH_APPLICATION_KEY", "OVH_APPLICATION_SECRET", "OVH_CONSUMER_KEY"} {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			return nil, plan, fmt.Errorf("missing %s (export or set in .env.ovh)", name)
		}
	}
	if o.endpoint == "" {
		o.endpoint = os.Getenv("OVH_ENDPOINT")
	}
	if o.endpoint == "" {
		o.endpoint = "ovh-eu"
	}
	if o.endpoint != "ovh-eu" && o.endpoint != "ovh-ca" && o.endpoint != "ovh-us" {
		return nil, plan, errors.New("OVH endpoint must be ovh-eu, ovh-ca, or ovh-us")
	}
	if o.action == "destroy" {
		data, err := os.ReadFile(o.manifest)
		if err != nil {
			return nil, plan, err
		}
		var resources ovhResources
		if err := json.Unmarshal(data, &resources); err != nil {
			return nil, plan, err
		}
		if resources.Endpoint != o.endpoint {
			return nil, plan, fmt.Errorf("manifest endpoint %q differs from %q", resources.Endpoint, o.endpoint)
		}
	}
	client, err := ovh.NewClient(o.endpoint, os.Getenv("OVH_APPLICATION_KEY"), os.Getenv("OVH_APPLICATION_SECRET"), os.Getenv("OVH_CONSUMER_KEY"))
	if err != nil {
		return nil, plan, err
	}
	client.Client.Timeout = 30 * time.Second
	client.Timeout = 30 * time.Second
	if o.action == "destroy" {
		return client, plan, nil
	}
	if o.project == "" {
		o.project = strings.TrimSpace(os.Getenv("OVH_CLOUD_PROJECT"))
	}
	if o.project == "" {
		var projects []string
		if err := client.GetWithContext(ctx, "/cloud/project", &projects); err != nil {
			return nil, plan, err
		}
		if len(projects) != 1 {
			return nil, plan, fmt.Errorf("set -ovh-project: accessible projects: %v", projects)
		}
		o.project = projects[0]
	}
	base := "/cloud/project/" + url.PathEscape(o.project)
	var flavors []ovhFlavor
	var images []ovhImage
	if err := client.GetWithContext(ctx, base+"/flavor?region="+url.QueryEscape(o.region), &flavors); err != nil {
		return nil, plan, err
	}
	if err := client.GetWithContext(ctx, base+"/image?osType=linux&region="+url.QueryEscape(o.region), &images); err != nil {
		return nil, plan, err
	}
	if o.action == "catalog" {
		err := json.NewEncoder(os.Stdout).Encode(map[string]any{"project": o.project, "region": o.region, "flavors": flavors, "images": images})
		return client, plan, err
	}
	for _, f := range flavors {
		if f.Name == o.flavor && f.Region == o.region && f.Available && strings.EqualFold(f.OSType, "linux") {
			plan.Flavor = f
			break
		}
	}
	for _, i := range images {
		if i.Name == o.image && i.Region == o.region && strings.EqualFold(i.Status, "active") && i.Visibility == "public" {
			plan.Image = i
			break
		}
	}
	if plan.Flavor.ID == "" || plan.Image.ID == "" {
		return nil, plan, errors.New("requested available Linux flavor/public active image not found; use -ovh-action catalog")
	}
	if plan.Flavor.Quota < o.agents+1 {
		return nil, plan, fmt.Errorf("flavor quota %d is below required %d VMs", plan.Flavor.Quota, o.agents+1)
	}
	plan.Endpoint = o.endpoint
	plan.RunID = runID
	plan.Project = o.project
	plan.Region = o.region
	plan.Hosts = o.agents + 1
	plan.Timeout = timeout.String()
	plan.HourlyRate = o.hourlyRate
	plan.Budget = o.budget
	return client, plan, nil
}

func ovhHost(instance ovhInstance, role string) (hostInfo, error) {
	h := hostInfo{Role: role, Name: instance.Name}
	for _, a := range instance.IPAddresses {
		ip := net.ParseIP(a.IP)
		if ip == nil || a.Type != "public" {
			continue
		}
		if ip.To4() != nil {
			h.PublicIPv4 = a.IP
		} else {
			h.PublicIPv6 = a.IP
		}
	}
	if h.PublicIPv4 == "" {
		return h, errors.New("instance has no public IPv4")
	}
	if h.PublicIPv6 == "" {
		return h, errors.New("instance has no public IPv6")
	}
	return h, nil
}

func provisionOVH(ctx context.Context, api ovhAPI, plan ovhPlan, repoRoot, artifactRoot, keyPath string) (map[string]hostInfo, func() error, error) {
	base := "/cloud/project/" + url.PathEscape(plan.Project)
	state := ovhResources{Endpoint: plan.Endpoint, Project: plan.Project, RunID: plan.RunID, Region: plan.Region}
	roles := []string{"controlplane"}
	for i := 0; i < plan.Hosts-1; i++ {
		roles = append(roles, fmt.Sprintf("agent-%02d", i+1))
	}
	for _, role := range roles {
		state.Names = append(state.Names, plan.RunID+"-"+role)
	}
	path := filepath.Join(artifactRoot, "ovh-resources.json")
	cleanup := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		return destroyOVH(cleanupCtx, api, path)
	}
	// Refuse collisions before recording ownership; never adopt existing resources.
	var existing []ovhInstance
	if err := api.GetWithContext(ctx, base+"/instance", &existing); err != nil {
		return nil, nil, err
	}
	for _, i := range existing {
		for _, name := range state.Names {
			if i.Name == name {
				return nil, nil, fmt.Errorf("instance %s already exists; use a fresh run-id", name)
			}
		}
	}
	var keys []ovhKey
	if err := api.GetWithContext(ctx, base+"/sshkey", &keys); err != nil {
		return nil, nil, err
	}
	for _, k := range keys {
		if k.Name == plan.RunID+"-runner" {
			return nil, nil, errors.New("runner SSH key already exists; use a fresh run-id")
		}
	}
	if _, err := os.Stat(path); err == nil {
		return nil, nil, errors.New("resource manifest already exists; use a fresh artifact directory")
	} else if !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("inspect resource manifest: %w", err)
	}
	if err := writeOVHResources(path, state); err != nil {
		return nil, nil, err
	}
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return nil, cleanup, err
	}
	var key ovhKey
	if err := api.PostWithContext(ctx, base+"/sshkey", map[string]any{"name": plan.RunID + "-runner", "publicKey": strings.TrimSpace(string(pub)), "region": plan.Region}, &key); err != nil {
		return nil, cleanup, err
	}
	if strings.TrimSpace(key.ID) == "" {
		return nil, cleanup, errors.New("OVH create returned an empty SSH key id")
	}
	state.SSHKeyID = key.ID
	if err := writeOVHResources(path, state); err != nil {
		return nil, cleanup, err
	}
	for i, role := range roles {
		templateRole := "agent"
		if role == "controlplane" {
			templateRole = role
		}
		template, err := os.ReadFile(filepath.Join(repoRoot, "infra/test-vm/cloud-init", templateRole+".yaml.tftpl"))
		if err != nil {
			return nil, cleanup, err
		}
		userData := strings.ReplaceAll(string(template), "${hostname}", state.Names[i])
		// OVH Ubuntu disables root SSH by default. Authorize only this ephemeral key.
		userData += "\ndisable_root: false\nssh_pwauth: false\nusers:\n  - default\n  - name: root\n    ssh_authorized_keys:\n      - " + strings.TrimSpace(string(pub)) + "\n"
		var instance ovhInstance
		infof("OVH: creating %s (%s in %s)", state.Names[i], plan.Flavor.Name, plan.Region)
		// Do not retry POST: an ambiguous response may already have created a billable VM.
		err = api.PostWithContext(ctx, base+"/instance", map[string]any{"name": state.Names[i], "region": plan.Region, "flavorId": plan.Flavor.ID, "imageId": plan.Image.ID, "sshKeyId": key.ID, "monthlyBilling": false, "userData": userData}, &instance)
		if err != nil {
			return nil, cleanup, err
		}
		if strings.TrimSpace(instance.ID) == "" {
			return nil, cleanup, fmt.Errorf("OVH create returned empty instance id for %s", state.Names[i])
		}
		state.Instances = append(state.Instances, instance.ID)
		if err := writeOVHResources(path, state); err != nil {
			return nil, cleanup, err
		}
	}
	hosts := make(map[string]hostInfo)
	var lastHostErr error
	err = testutil.Poll(ctx, testutil.PollConfig{Timeout: 15 * time.Minute, Interval: 5 * time.Second}, func(ctx context.Context) (bool, error) {
		for i, id := range state.Instances {
			if strings.TrimSpace(id) == "" {
				return false, nil
			}
			var instance ovhInstance
			if err := api.GetWithContext(ctx, base+"/instance/"+url.PathEscape(id), &instance); err != nil {
				if ovhTransient(err) {
					return false, nil
				}
				return false, err
			}
			if instance.Status == "ERROR" {
				return false, fmt.Errorf("OVH instance %s entered ERROR", instance.Name)
			}
			if instance.Status != "ACTIVE" {
				return false, nil
			}
			role := "agent"
			if i == 0 {
				role = "controlplane"
			}
			h, err := ovhHost(instance, role)
			if err != nil {
				lastHostErr = fmt.Errorf("OVH instance %s is active but not ready: %w", instance.Name, err)
				return false, nil
			}
			hosts[roles[i]] = h
		}
		return true, nil
	})
	if err != nil {
		return hosts, cleanup, errors.Join(err, lastHostErr)
	}
	return hosts, cleanup, nil
}

func destroyOVH(ctx context.Context, api ovhAPI, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			infof("OVH: no resource manifest at %s; nothing to destroy", path)
			return nil
		}
		return err
	}
	var state ovhResources
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	if !safeRunID.MatchString(state.RunID) || state.Project == "" || state.Region == "" || len(state.Names) == 0 {
		return errors.New("invalid OVH resource manifest")
	}
	if state.Destroyed {
		return nil
	}
	allowed := make(map[string]bool)
	for _, name := range state.Names {
		if !strings.HasPrefix(name, state.RunID+"-") {
			return errors.New("manifest name outside run")
		}
		allowed[name] = true
	}
	base := "/cloud/project/" + url.PathEscape(state.Project)
	// Reconcile names as well as IDs to catch successful POSTs whose responses were lost.
	// Require a sustained absence to allow an ambiguously successful create to
	// become visible before cleanup is declared complete.
	empty := 0
	err = testutil.Poll(ctx, testutil.PollConfig{Timeout: 9 * time.Minute, Interval: 10 * time.Second}, func(ctx context.Context) (bool, error) {
		found, err := reconcileOVHInstances(ctx, api, base, state.Region, allowed)
		if err != nil {
			if ovhTransient(err) {
				return false, nil
			}
			return false, err
		}
		if found {
			empty = 0
		} else {
			empty++
		}
		return empty >= 12, nil
	})
	if err != nil {
		return fmt.Errorf("OVH instances may remain; retry -ovh-action destroy -ovh-manifest %s: %w", path, err)
	}
	var keys []ovhKey
	if err := retryOVH(ctx, func(ctx context.Context) error {
		keys = nil
		return api.GetWithContext(ctx, base+"/sshkey", &keys)
	}); err != nil {
		return err
	}
	for _, key := range keys {
		if key.Name == state.RunID+"-runner" {
			if strings.TrimSpace(key.ID) == "" {
				return errors.New("owned OVH SSH key has an empty id")
			}
			if err := retryOVH(ctx, func(ctx context.Context) error {
				err := api.DeleteWithContext(ctx, base+"/sshkey/"+url.PathEscape(key.ID), nil)
				if ovhNotFound(err) {
					return nil
				}
				return err
			}); err != nil {
				return err
			}
		}
	}
	state.Destroyed = true
	return writeOVHResources(path, state)
}
func ovhNotFound(err error) bool {
	var apiErr *ovh.APIError
	return errors.As(err, &apiErr) && apiErr.Code == 404
}

func ovhTransient(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *ovh.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code >= 500 || apiErr.Code == 408 || apiErr.Code == 425 || apiErr.Code == 429
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary())
}

func retryOVH(ctx context.Context, operation func(context.Context) error) error {
	var lastErr error
	err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 2 * time.Minute, Interval: 2 * time.Second}, func(ctx context.Context) (bool, error) {
		err := operation(ctx)
		if err == nil {
			return true, nil
		}
		if !ovhTransient(err) {
			return false, err
		}
		lastErr = err
		return false, nil
	})
	if err != nil {
		return errors.Join(err, lastErr)
	}
	return nil
}

// Atomic replacement leaves a usable cleanup manifest if the runner is killed
// while recording a successful API response.
func writeOVHResources(path string, state ovhResources) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".ovh-resources-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Attempt every owned instance even if one deletion fails. Resources from
// another run or region must never be swept up by cleanup.
func reconcileOVHInstances(ctx context.Context, api ovhAPI, base, region string, allowed map[string]bool) (bool, error) {
	var instances []ovhInstance
	if err := api.GetWithContext(ctx, base+"/instance", &instances); err != nil {
		return false, err
	}
	found := false
	var failures []error
	for _, instance := range instances {
		if !allowed[instance.Name] || instance.Region != region {
			continue
		}
		found = true
		if instance.Status == "DELETING" || instance.Status == "DELETED" {
			continue
		}
		if strings.TrimSpace(instance.ID) == "" {
			failures = append(failures, fmt.Errorf("owned OVH instance %s has an empty id", instance.Name))
			continue
		}
		if err := api.DeleteWithContext(ctx, base+"/instance/"+url.PathEscape(instance.ID), nil); err != nil && !ovhNotFound(err) {
			failures = append(failures, err)
		}
	}
	return found, errors.Join(failures...)
}
