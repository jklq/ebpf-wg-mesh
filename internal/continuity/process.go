package continuity

import (
	"context"
	"encoding/json"
	"fmt"
	"go.yaml.in/yaml/v3"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func start(ctx context.Context, dir string, l Launch) (*child, error) {
	inputs, err := snapshotConfig(dir, l)
	if err != nil {
		return nil, err
	}
	boot := bootLaunch(l, inputs)
	root, err := os.MkdirTemp(dir, "running-")
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(root)
		}
	}()
	replace := func(v string) string { return strings.NewReplacer(inputs, root, boot.ConfigOrigin, root).Replace(v) }
	if err = copyConfig(inputs, root, replace); err != nil {
		return nil, err
	}
	l = boot
	c := &child{launch: l, boot: boot, configPath: root, done: make(chan struct{})}
	env := map[string]string{}
	for k, v := range l.Environment {
		env[k] = replace(v)
	}
	if state := env["CONTROLPLANE_STATE_DIR"]; state != "" {
		if env["CONTROLPLANE_LOGS_DIR"] == "" {
			env["CONTROLPLANE_LOGS_DIR"] = filepath.Join(state, "logs")
		}
		env["CONTROLPLANE_STATE_DIR"] = filepath.Join(root, "state")
		if err := os.MkdirAll(env["CONTROLPLANE_STATE_DIR"], 0700); err != nil {
			return nil, err
		}
	}
	ports := map[string]string{}
	var reserved []net.Listener
	defer func() {
		for _, ln := range reserved {
			ln.Close()
		}
	}()
	for _, p := range l.Listeners {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		reserved = append(reserved, ln)
		backend := ln.Addr().String()
		c.backends = append(c.backends, backend)
		_, frontPort, err := net.SplitHostPort(p.Address)
		if err != nil {
			return nil, err
		}
		_, port, _ := net.SplitHostPort(backend)
		ports[frontPort] = port
		if p.PortOnly {
			env[p.Env] = port
			if p.Env == "PORT" {
				env["NITRO_PORT"] = port
				env["HOST"] = "127.0.0.1"
				env["NITRO_HOST"] = "127.0.0.1"
			}
		} else {
			env[p.Env] = backend
		}
	}
	if env["PLATFORM_ENVOY_HTTP_LISTEN"] != "" {
		path := filepath.Join(root, "envoy.yaml")
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var bootstrap map[string]any
		if err = yaml.Unmarshal(b, &bootstrap); err != nil {
			return nil, err
		}
		node, ok := bootstrap["node"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("ingress bootstrap lacks its authenticated node identity")
		}
		metadata := map[string]any{}
		if existing, ok := node["metadata"].(map[string]any); ok {
			metadata = existing
		}
		projection := map[string]any{}
		for _, listener := range l.Listeners {
			if listener.Env == "PLATFORM_ENVOY_ADMIN_LISTEN" {
				continue
			}
			_, port, _ := net.SplitHostPort(listener.Address)
			if target, ok := ports[port]; ok {
				n, _ := strconv.Atoi(target)
				projection[port] = n
			}
		}
		metadata[ListenerPortsMetadata] = projection
		node["metadata"] = metadata
		_, adminPort, err := net.SplitHostPort(env["PLATFORM_ENVOY_ADMIN_LISTEN"])
		if err != nil {
			return nil, err
		}
		n, _ := strconv.Atoi(adminPort)
		bootstrap["admin"] = map[string]any{"address": map[string]any{"socket_address": map[string]any{"protocol": "TCP", "address": "127.0.0.1", "port_value": n}}}
		if health := env["PLATFORM_ENVOY_HEALTH_LISTEN"]; health != "" {
			_, healthPort, err := net.SplitHostPort(health)
			if err != nil {
				return nil, err
			}
			port, _ := strconv.Atoi(healthPort)
			static, _ := bootstrap["static_resources"].(map[string]any)
			listeners, _ := static["listeners"].([]any)
			projected := false
			for _, item := range listeners {
				if listener, ok := item.(map[string]any); ok && listener["name"] == "ingress_health" {
					listener["address"] = map[string]any{"socket_address": map[string]any{"address": "127.0.0.1", "port_value": port}}
					projected = true
				}
			}
			if !projected {
				return nil, fmt.Errorf("ingress bootstrap lacks the health listener its owner serves")
			}
		}
		b, err = yaml.Marshal(bootstrap)
		if err != nil {
			return nil, err
		}
		if err = os.WriteFile(path, b, 0600); err != nil {
			return nil, err
		}
	}
	probePath := filepath.Join(root, "probe.json")
	if b, err := os.ReadFile(probePath); err == nil {
		var p map[string]any
		if err = json.Unmarshal(b, &p); err != nil {
			return nil, err
		}
		for _, key := range []string{"url", "runtimeURL"} {
			if value, ok := p[key].(string); ok && value != "" {
				u, err := url.Parse(value)
				if err != nil {
					return nil, err
				}
				if port, ok := ports[u.Port()]; ok {
					u.Host = net.JoinHostPort("127.0.0.1", port)
					p[key] = u.String()
				}
			}
		}
		if err = save(probePath, p); err != nil {
			return nil, err
		}
	}
	env["PLATFORM_CONTINUITY_ID"] = l.ID
	argv := make([]string, len(l.Command))
	for i, v := range l.Command {
		argv[i] = replace(v)
	}
	c.ready = append([]string(nil), l.Ready...)
	for i, v := range c.ready {
		c.ready[i] = replace(v)
	}
	c.stop = append([]string(nil), l.Stop...)
	for i, v := range c.stop {
		c.stop[i] = replace(v)
	}
	if invocation := os.Getenv("INVOCATION_ID"); invocation != "" {
		env["INVOCATION_ID"] = invocation
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = l.Directory
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+env[k])
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cleanup, err := nativeResources(cmd, l)
	if err != nil {
		return nil, err
	}
	c.cmd = cmd
	for _, ln := range reserved {
		ln.Close()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cleanup()
		return nil, err
	}
	success = true
	go func() { cmd.Wait(); cleanup(); c.exited.Store(true); close(c.done) }()
	return c, nil
}

// Verify socket ownership after releasing the port reservation to prevent false readiness.
func ownsListeners(c *child) bool {
	pid := strconv.Itoa(c.cmd.Process.Pid)
	fds, err := os.ReadDir("/proc/" + pid + "/fd")
	if err != nil {
		return false
	}
	inodes := map[string]bool{}
	for _, fd := range fds {
		link, _ := os.Readlink("/proc/" + pid + "/fd/" + fd.Name())
		if strings.HasPrefix(link, "socket:[") {
			inodes[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] = true
		}
	}
	owned := map[int]bool{}
	for _, family := range []string{"tcp", "tcp6"} {
		b, err := os.ReadFile("/proc/" + pid + "/net/" + family)
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 10 || f[3] != "0A" || !inodes[f[9]] {
				continue
			}
			_, p, ok := strings.Cut(f[1], ":")
			if ok {
				port, _ := strconv.ParseInt(p, 16, 32)
				owned[int(port)] = true
			}
		}
	}
	for _, addr := range c.backends {
		_, p, _ := net.SplitHostPort(addr)
		port, _ := strconv.Atoi(p)
		if !owned[port] {
			return false
		}
	}
	return true
}

func bootLaunch(l Launch, inputs string) Launch {
	boot := l
	if boot.ConfigOrigin == "" {
		boot.ConfigOrigin = l.ConfigDir
	}
	boot.ConfigDir = inputs
	boot.Environment = map[string]string{}
	for key, value := range l.Environment {
		boot.Environment[key] = strings.ReplaceAll(value, l.ConfigDir, inputs)
	}
	boot.Command = append([]string(nil), l.Command...)
	boot.Ready = append([]string(nil), l.Ready...)
	boot.Stop = append([]string(nil), l.Stop...)
	for _, argv := range [][]string{boot.Command, boot.Ready, boot.Stop} {
		for n := range argv {
			argv[n] = strings.ReplaceAll(argv[n], l.ConfigDir, inputs)
		}
	}
	return boot
}
