package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Server struct{ ID, Address, Status string }
type ProviderCapabilities struct{ Create, Power, Delete bool }
type Adapter interface {
	Capabilities() ProviderCapabilities
	Discover(context.Context, string, Host) (Server, bool, error)
	Create(context.Context, string, Host) (Server, error)
	Power(context.Context, string, string) error
	Delete(context.Context, string) error
}
type APIAdapter struct {
	Kind, BaseURL, Token string
	Client               *http.Client
}
type apiError struct {
	Status         int
	Provider, Path string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s %s returned HTTP %d", e.Provider, e.Path, e.Status)
}

func NewAdapter(i Installation, h Host) (Adapter, error) {
	p := i.Providers[h.Binding.Provider]
	if p.Kind == "linux" {
		return imported{}, nil
	}
	b, err := i.Resolve(p.Token)
	if err != nil {
		return nil, err
	}
	base := "https://api.hetzner.cloud/v1"
	if p.Kind == "gigahost" {
		base = "https://api.gigahost.no/api/v0"
	}
	return &APIAdapter{Kind: p.Kind, BaseURL: base, Token: strings.TrimSpace(string(b)), Client: &http.Client{Timeout: 45 * time.Second}}, nil
}
func (a *APIAdapter) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{Create: a.Kind == "hetzner", Power: true, Delete: true}
}
func (a *APIAdapter) request(ctx context.Context, method, path string, body, out any) error {
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.BaseURL+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%s API transport failed: %w", a.Kind, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{Status: resp.StatusCode, Provider: a.Kind, Path: path}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode %s response: %w", a.Kind, err)
	}
	return nil
}

type hcloudServer struct {
	ID        int64             `json:"id"`
	Status    string            `json:"status"`
	Labels    map[string]string `json:"labels"`
	PublicNet struct {
		IPv4 struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
}

func (s hcloudServer) server() Server {
	return Server{ID: strconv.FormatInt(s.ID, 10), Address: s.PublicNet.IPv4.IP, Status: s.Status}
}

type gigahostServer struct {
	ID      string `json:"srv_id"`
	Address string `json:"srv_primary_ip"`
	Online  bool   `json:"srv_status"`
}
type gigahostResponse struct {
	Meta struct {
		Status     int `json:"status"`
		TotalPages int `json:"total_pages"`
	} `json:"meta"`
	Data []gigahostServer `json:"data"`
}

func (a *APIAdapter) Discover(ctx context.Context, installation string, h Host) (Server, bool, error) {
	if a.Kind == "gigahost" {
		if h.Binding.ServerID == "" {
			return Server{}, false, fmt.Errorf("Gigahost imports require the purchased srv_id; server creation is unsupported")
		}
		var response gigahostResponse
		if err := a.request(ctx, http.MethodGet, "/servers/"+url.PathEscape(h.Binding.ServerID), nil, &response); err != nil {
			var api *apiError
			if errors.As(err, &api) && api.Status == http.StatusNotFound {
				return Server{}, false, nil
			}
			return Server{}, false, err
		}
		if response.Meta.Status != 200 {
			return Server{}, false, fmt.Errorf("Gigahost discovery returned meta.status %d", response.Meta.Status)
		}
		for _, s := range response.Data {
			if s.ID == h.Binding.ServerID {
				status := "off"
				if s.Online {
					status = "running"
				}
				return Server{s.ID, s.Address, status}, true, nil
			}
		}
		return Server{}, false, nil
	}
	if h.Binding.ServerID != "" {
		var response struct {
			Server hcloudServer `json:"server"`
		}
		if err := a.request(ctx, http.MethodGet, "/servers/"+url.PathEscape(h.Binding.ServerID), nil, &response); err != nil {
			var api *apiError
			if errors.As(err, &api) && api.Status == http.StatusNotFound {
				return Server{}, false, nil
			}
			return Server{}, false, err
		}
		return response.Server.server(), response.Server.ID != 0, nil
	}
	selector := "platform-installation=" + installation + ",platform-host=" + h.ID
	var response struct {
		Servers []hcloudServer `json:"servers"`
	}
	if err := a.request(ctx, http.MethodGet, "/servers?label_selector="+url.QueryEscape(selector)+"&per_page=50", nil, &response); err != nil {
		return Server{}, false, err
	}
	if len(response.Servers) > 1 {
		return Server{}, false, fmt.Errorf("multiple servers match installation %s host %s; resolve ambiguous purchase before continuing", installation, h.ID)
	}
	if len(response.Servers) == 0 {
		return Server{}, false, nil
	}
	return response.Servers[0].server(), true, nil
}
func (a *APIAdapter) Create(ctx context.Context, installation string, h Host) (Server, error) {
	if !a.Capabilities().Create || h.Purchase == nil {
		return Server{}, fmt.Errorf("%s does not support server creation; import an existing server", a.Kind)
	}
	var response struct {
		Server hcloudServer `json:"server"`
	}
	err := a.request(ctx, http.MethodPost, "/servers", map[string]any{"name": installation + "-" + h.ID, "server_type": h.Purchase.ServerType, "location": h.Purchase.Location, "image": h.Purchase.Image, "ssh_keys": h.Purchase.SSHKeys, "labels": map[string]string{"platform-installation": installation, "platform-host": h.ID}, "start_after_create": true}, &response)
	return response.Server.server(), err
}
func (a *APIAdapter) Power(ctx context.Context, id, action string) error {
	if !contains([]string{"on", "off", "reboot"}, action) {
		return fmt.Errorf("unsupported power action %q", action)
	}
	if a.Kind == "gigahost" {
		var response struct {
			Meta struct {
				Status int `json:"status"`
			} `json:"meta"`
		}
		if err := a.request(ctx, http.MethodGet, "/servers/"+url.PathEscape(id)+"/"+action, nil, &response); err != nil {
			return err
		}
		if response.Meta.Status != 200 {
			return fmt.Errorf("Gigahost power returned status %d", response.Meta.Status)
		}
		return nil
	}
	verbs := map[string]string{"on": "poweron", "off": "poweroff", "reboot": "reboot"}
	return a.request(ctx, http.MethodPost, "/servers/"+url.PathEscape(id)+"/actions/"+verbs[action], nil, nil)
}
func (a *APIAdapter) Delete(ctx context.Context, id string) error {
	if a.Kind == "gigahost" {
		var response struct {
			Meta struct {
				Status int `json:"status"`
			} `json:"meta"`
		}
		if err := a.request(ctx, http.MethodPost, "/servers/"+url.PathEscape(id)+"/cancel", map[string]int{"early_termination": 1}, &response); err != nil {
			return err
		}
		if response.Meta.Status != 200 {
			return fmt.Errorf("Gigahost cancellation returned status %d", response.Meta.Status)
		}
		return nil
	}
	return a.request(ctx, http.MethodDelete, "/servers/"+url.PathEscape(id), nil, nil)
}

type imported struct{}

func (imported) Capabilities() ProviderCapabilities { return ProviderCapabilities{} }
func (imported) Discover(_ context.Context, _ string, h Host) (Server, bool, error) {
	return Server{ID: h.Binding.ServerID, Address: h.SSH.Address, Status: "imported"}, true, nil
}
func (imported) Create(context.Context, string, Host) (Server, error) {
	return Server{}, fmt.Errorf("imported Linux hosts do not support purchases")
}
func (imported) Power(context.Context, string, string) error {
	return fmt.Errorf("imported Linux hosts do not support provider power operations")
}
func (imported) Delete(context.Context, string) error {
	return fmt.Errorf("imported Linux hosts do not support provider deletion")
}
