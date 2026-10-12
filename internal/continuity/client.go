package continuity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
)

func Request(ctx context.Context, dir, action string, l *Launch) (Status, error) {
	var status Status
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, "control.sock"))
	}}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport}
	method := http.MethodPost
	var body []byte
	if action == "status" {
		method = http.MethodGet
	} else if l != nil {
		var err error
		body, err = json.Marshal(l)
		if err != nil {
			return status, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://runtime/"+action, bytes.NewReader(body))
	if err != nil {
		return status, err
	}
	res, err := client.Do(req)
	if err != nil {
		return status, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return status, fmt.Errorf("continuity %s: %s", action, string(b))
	}
	err = json.NewDecoder(res.Body).Decode(&status)
	return status, err
}
