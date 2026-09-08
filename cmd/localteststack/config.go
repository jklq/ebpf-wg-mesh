package main

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

const (
	defaultLocalIngressHost   = "platform.localtest.me"
	defaultLocalIngressPort   = 8080
	defaultLocalDockerNetwork = "ebpf-wg-mesh-local"
	defaultLocalDomainSuffix  = "localtest.me"
	defaultConsoleBindAddress = "127.0.0.1"
)

var githubLoginPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?$`)

type localStackConfig struct {
	IngressHost          string
	IngressPort          int
	DockerNetwork        string
	LocalDomainSuffix    string
	PlatformDomainSuffix string
	ConsoleBindAddress   string
	EnablePublicTunnel   bool
	OperatorGitHubLogin  string
}

func loadLocalStackConfig(lookup func(string) string) (localStackConfig, error) {
	if runtimeMode := strings.TrimSpace(lookup("LOCALTESTSTACK_RUNTIME")); runtimeMode != "" {
		return localStackConfig{}, fmt.Errorf("LOCALTESTSTACK_RUNTIME has been removed; localteststack always uses the Docker runtime")
	}
	cfg := localStackConfig{
		IngressHost:          firstNonEmpty(strings.TrimSpace(lookup("LOCALTESTSTACK_INGRESS_HOST")), defaultLocalIngressHost),
		DockerNetwork:        firstNonEmpty(strings.TrimSpace(lookup("LOCALTESTSTACK_DOCKER_NETWORK")), defaultLocalDockerNetwork),
		LocalDomainSuffix:    normalizeDomainSuffix(firstNonEmpty(strings.TrimSpace(lookup("LOCALTESTSTACK_LOCAL_DOMAIN_SUFFIX")), defaultLocalDomainSuffix)),
		PlatformDomainSuffix: normalizeDomainSuffix(strings.TrimSpace(lookup("LOCALTESTSTACK_PLATFORM_DOMAIN_SUFFIX"))),
		ConsoleBindAddress:   firstNonEmpty(strings.TrimSpace(lookup("LOCALTESTSTACK_CONSOLE_BIND_ADDRESS")), defaultConsoleBindAddress),
		EnablePublicTunnel:   strings.EqualFold(strings.TrimSpace(lookup("LOCALTESTSTACK_ENABLE_PUBLIC_TUNNEL")), "true") || strings.TrimSpace(lookup("LOCALTESTSTACK_ENABLE_PUBLIC_TUNNEL")) == "1",
		OperatorGitHubLogin:  strings.ToLower(strings.TrimSpace(lookup("LOCALTESTSTACK_OPERATOR_GITHUB_LOGIN"))),
		IngressPort:          defaultLocalIngressPort,
	}
	if raw := strings.TrimSpace(lookup("LOCALTESTSTACK_INGRESS_PORT")); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil {
			return localStackConfig{}, fmt.Errorf("parse LOCALTESTSTACK_INGRESS_PORT: %w", err)
		}
		cfg.IngressPort = port
	}
	if cfg.IngressHost == "" {
		return localStackConfig{}, fmt.Errorf("LOCALTESTSTACK_INGRESS_HOST is required")
	}
	if net.ParseIP(cfg.IngressHost) != nil {
		return localStackConfig{}, fmt.Errorf("LOCALTESTSTACK_INGRESS_HOST must be a hostname")
	}
	if cfg.IngressPort <= 0 || cfg.IngressPort > 65535 {
		return localStackConfig{}, fmt.Errorf("LOCALTESTSTACK_INGRESS_PORT must be between 1 and 65535")
	}
	if cfg.DockerNetwork == "" {
		return localStackConfig{}, fmt.Errorf("LOCALTESTSTACK_DOCKER_NETWORK is required")
	}
	if cfg.LocalDomainSuffix == "" {
		return localStackConfig{}, fmt.Errorf("LOCALTESTSTACK_LOCAL_DOMAIN_SUFFIX is required")
	}
	if cfg.PlatformDomainSuffix != "" {
		if net.ParseIP(cfg.PlatformDomainSuffix) != nil {
			return localStackConfig{}, fmt.Errorf("LOCALTESTSTACK_PLATFORM_DOMAIN_SUFFIX must be a hostname")
		}
		if !strings.Contains(cfg.PlatformDomainSuffix, ".") {
			return localStackConfig{}, fmt.Errorf("LOCALTESTSTACK_PLATFORM_DOMAIN_SUFFIX must be a DNS name with a dot")
		}
	}
	if net.ParseIP(cfg.ConsoleBindAddress) == nil {
		return localStackConfig{}, fmt.Errorf("LOCALTESTSTACK_CONSOLE_BIND_ADDRESS must be an IP address")
	}
	if cfg.EnablePublicTunnel && net.ParseIP(cfg.ConsoleBindAddress).IsLoopback() {
		return localStackConfig{}, fmt.Errorf("public tunnel requires an explicit non-loopback LOCALTESTSTACK_CONSOLE_BIND_ADDRESS")
	}
	if cfg.OperatorGitHubLogin != "" && !githubLoginPattern.MatchString(cfg.OperatorGitHubLogin) {
		return localStackConfig{}, fmt.Errorf("LOCALTESTSTACK_OPERATOR_GITHUB_LOGIN must be a valid GitHub login")
	}
	return cfg, nil
}

func operatorGitHubUserID(login string) string {
	return "github:" + strings.ToLower(strings.TrimSpace(login))
}

func (c localStackConfig) examplePublishedHost(service string) string {
	service = strings.TrimSpace(strings.ToLower(service))
	if service == "" {
		service = "echo"
	}
	return service + "." + c.LocalDomainSuffix
}

func normalizeDomainSuffix(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.TrimPrefix(value, ".")
	value = strings.TrimSuffix(value, ".")
	return value
}
