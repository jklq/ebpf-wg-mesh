package productionops

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type RedfishFence struct {
	SystemURL       string `json:"systemURL"`
	CredentialsFile string `json:"credentialsFile"`
	CAFile          string `json:"caFile,omitempty"`
}

func (f RedfishFence) call(ctx context.Context, method, target string, body io.Reader, result any) error {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.User != nil || !strings.HasPrefix(u.Path, "/redfish/v1/Systems/") {
		return fmt.Errorf("fencing requires an explicit HTTPS Redfish computer system")
	}
	var credential struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err = privateJSON(f.CredentialsFile, &credential); err != nil {
		return err
	}
	if credential.Username == "" || credential.Password == "" {
		return fmt.Errorf("Redfish credentials required")
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if f.CAFile != "" {
		b, err := os.ReadFile(f.CAFile)
		if err != nil {
			return err
		}
		cfg.RootCAs = x509.NewCertPool()
		if !cfg.RootCAs.AppendCertsFromPEM(b) {
			return fmt.Errorf("invalid fencing CA")
		}
	}
	client := http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(credential.Username, credential.Password)
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("independent fencing unavailable: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Redfish fencing request rejected: HTTP %d", response.StatusCode)
	}
	if result != nil {
		return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(result)
	}
	return nil
}
func (f RedfishFence) off(ctx context.Context) error {
	var state struct{ PowerState string }
	if err := f.call(ctx, http.MethodGet, f.SystemURL, nil, &state); err != nil {
		return err
	}
	if state.PowerState != "Off" {
		return fmt.Errorf("Redfish reports power %q; fence unresolved", state.PowerState)
	}
	return nil
}
func (f RedfishFence) powerOff(ctx context.Context) error {
	if err := f.off(ctx); err == nil {
		return nil
	}
	var system struct {
		Actions map[string]struct{ Target string }
	}
	if err := f.call(ctx, http.MethodGet, f.SystemURL, nil, &system); err != nil {
		return err
	}
	target := system.Actions["#ComputerSystem.Reset"].Target
	reference, err := url.Parse(target)
	if err != nil {
		return err
	}
	origin, err := url.Parse(f.SystemURL)
	if err != nil {
		return err
	}
	reset := origin.ResolveReference(reference)
	if reset.Host != origin.Host {
		return fmt.Errorf("Redfish reset escapes selected fencing authority")
	}
	if err = f.call(ctx, http.MethodPost, reset.String(), strings.NewReader(`{"ResetType":"ForceOff"}`), nil); err != nil {
		return err
	}
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err = f.off(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("Redfish shutdown pending; retry verifies actual power before restore")
		case <-ticker.C:
		}
	}
}
