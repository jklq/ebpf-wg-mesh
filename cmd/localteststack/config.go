package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

const (
	defaultLocalIngressHost   = "platform.localtest.me"
	defaultLocalIngressPort   = 8080
	defaultLocalDockerNetwork = "ebpf-wg-mesh-local"
	defaultLocalDomainSuffix  = "localtest.me"
)

type localStackConfig struct {
	IngressHost       string
	IngressPort       int
	DockerNetwork     string
	LocalDomainSuffix string
}

func loadLocalStackConfig(lookup func(string) string) (localStackConfig, error) {
	if runtimeMode := strings.TrimSpace(lookup("LOCALTESTSTACK_RUNTIME")); runtimeMode != "" {
		return localStackConfig{}, fmt.Errorf("LOCALTESTSTACK_RUNTIME has been removed; localteststack always uses the Docker runtime")
	}
	cfg := localStackConfig{
		IngressHost:       firstNonEmpty(strings.TrimSpace(lookup("LOCALTESTSTACK_INGRESS_HOST")), defaultLocalIngressHost),
		DockerNetwork:     firstNonEmpty(strings.TrimSpace(lookup("LOCALTESTSTACK_DOCKER_NETWORK")), defaultLocalDockerNetwork),
		LocalDomainSuffix: normalizeDomainSuffix(firstNonEmpty(strings.TrimSpace(lookup("LOCALTESTSTACK_LOCAL_DOMAIN_SUFFIX")), defaultLocalDomainSuffix)),
		IngressPort:       defaultLocalIngressPort,
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
	return cfg, nil
}

func (c localStackConfig) ingressBaseURL() string {
	return fmt.Sprintf("http://%s:%d", c.IngressHost, c.IngressPort)
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
