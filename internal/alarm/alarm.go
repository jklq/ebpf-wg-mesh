// Package alarm sends one operator alarm per failure to an operator-chosen
// HTTPS webhook. There is no default destination: an empty URL disables
// alarms, because only the operator can say where a page must go. Sending is
// best-effort; callers log a send error and never let it mask the failure
// that raised the alarm.
package alarm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ebof-wg-mesh/internal/telemetry"
)

const (
	sendTimeout    = 10 * time.Second
	maxMessageSize = 1024
)

// Event is the JSON body POSTed to the webhook. NextAction names the one
// thing the operator should do next; it is empty when the system already
// recovered on its own.
type Event struct {
	Event      string    `json:"event"`
	Source     string    `json:"source"`
	Subject    string    `json:"subject"`
	Message    string    `json:"message"`
	NextAction string    `json:"nextAction,omitempty"`
	At         time.Time `json:"at"`
}

// Sender POSTs events to one webhook URL. A nil or disabled Sender sends
// nothing and reports no error, so callers need no nil checks.
type Sender struct {
	url    string
	source string
	client *http.Client
	now    func() time.Time
}

// New validates rawURL and returns a Sender for source. An empty URL is a
// disabled Sender, not an error.
func New(rawURL, source string) (*Sender, error) {
	if err := Validate(rawURL); err != nil {
		return nil, err
	}
	return &Sender{
		url:    strings.TrimSpace(rawURL),
		source: strings.TrimSpace(source),
		client: telemetry.HTTPClient(&http.Client{Timeout: sendTimeout}),
		now:    time.Now,
	}, nil
}

// Validate reports whether rawURL may receive alarms: HTTPS, or plain HTTP
// to a literal loopback address for local receivers. Userinfo is rejected so
// credentials never ride along in configuration dumps.
func Validate(rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("alarm webhook URL is invalid")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("alarm webhook URL must use HTTPS (plain HTTP only to a loopback address)")
}

// Enabled reports whether a destination is configured.
func (s *Sender) Enabled() bool { return s != nil && s.url != "" }

// Send posts one event. It makes a single attempt; the raising code retries
// its own work on its own schedule and alarms again only if that fails too.
func (s *Sender) Send(ctx context.Context, event Event) error {
	if !s.Enabled() {
		return nil
	}
	if event.Source == "" {
		event.Source = s.source
	}
	if event.At.IsZero() {
		event.At = s.now().UTC()
	}
	event.Message = truncate(event.Message)
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("send alarm: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("send alarm: webhook answered %s", resp.Status)
	}
	return nil
}

func truncate(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > maxMessageSize {
		message = message[:maxMessageSize]
	}
	return message
}
