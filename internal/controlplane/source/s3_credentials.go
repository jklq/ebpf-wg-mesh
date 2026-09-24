package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type s3Credentials struct {
	accessKey    string
	secretKey    string
	sessionToken string
	expiresAt    time.Time
}

func (c s3Credentials) valid() bool {
	return strings.TrimSpace(c.accessKey) != "" && strings.TrimSpace(c.secretKey) != ""
}

func (c s3Credentials) expired(now time.Time) bool {
	if c.expiresAt.IsZero() {
		return false
	}
	return !now.Before(c.expiresAt.Add(-time.Minute))
}

type s3CredentialsProvider struct {
	file string

	mu     sync.Mutex
	cached s3Credentials
	has    bool

	httpClient *http.Client
}

func newS3CredentialsProvider(credentialsFile string) *s3CredentialsProvider {
	return &s3CredentialsProvider{
		file:       strings.TrimSpace(credentialsFile),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

func (p *s3CredentialsProvider) validate() error {
	if p == nil || p.file == "" {
		return nil
	}
	creds, err := readS3CredentialsFile(p.file)
	if err != nil {
		return err
	}
	if !creds.valid() {
		return fmt.Errorf("S3 credentials file %s has no access key material", p.file)
	}
	return nil
}

func (p *s3CredentialsProvider) get(ctx context.Context) (s3Credentials, bool, error) {
	if p == nil {
		return s3Credentials{}, true, nil
	}
	if p.file != "" {
		creds, err := readS3CredentialsFile(p.file)
		if err != nil {
			return s3Credentials{}, false, err
		}
		if !creds.valid() {
			return s3Credentials{}, false, fmt.Errorf("S3 credentials file %s has no access key material", p.file)
		}
		return creds, false, nil
	}
	if access, secret, token := envStaticS3Credentials(); access != "" && secret != "" {
		return s3Credentials{accessKey: access, secretKey: secret, sessionToken: token}, false, nil
	}
	p.mu.Lock()
	cached, has := p.cached, p.has
	p.mu.Unlock()
	if has && cached.valid() && !cached.expired(time.Now().UTC()) {
		return cached, false, nil
	}
	if creds, ok, err := p.containerCredentials(ctx); err != nil {
		return s3Credentials{}, false, err
	} else if ok {
		p.store(creds)
		return creds, false, nil
	}
	if creds, ok := p.instanceCredentials(ctx); ok {
		p.store(creds)
		return creds, false, nil
	}
	return s3Credentials{}, true, nil
}

func (p *s3CredentialsProvider) store(creds s3Credentials) {
	p.mu.Lock()
	p.cached, p.has = creds, true
	p.mu.Unlock()
}

func envStaticS3Credentials() (access, secret, token string) {
	return strings.TrimSpace(os.Getenv("AWS_ACCESS_KEY_ID")),
		strings.TrimSpace(os.Getenv("AWS_SECRET_ACCESS_KEY")),
		strings.TrimSpace(os.Getenv("AWS_SESSION_TOKEN"))
}

func readS3CredentialsFile(path string) (s3Credentials, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return s3Credentials{}, fmt.Errorf("read S3 credentials file %s: %w", path, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return s3Credentials{}, fmt.Errorf("parse S3 credentials file %s: %w", path, err)
	}
	lowered := make(map[string]string, len(decoded))
	for key, value := range decoded {
		text, _ := value.(string)
		lowered[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(text)
	}
	lookup := func(names ...string) string {
		for _, name := range names {
			if value := lowered[strings.ToLower(name)]; value != "" {
				return value
			}
		}
		return ""
	}
	return s3Credentials{
		accessKey:    lookup("access_key_id", "accesskeyid", "access_key", "aws_access_key_id"),
		secretKey:    lookup("secret_access_key", "secretaccesskey", "secret_key", "aws_secret_access_key"),
		sessionToken: lookup("session_token", "sessiontoken", "token", "aws_session_token"),
	}, nil
}

type containerCredentialsResponse struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
}

func (p *s3CredentialsProvider) containerCredentials(ctx context.Context) (s3Credentials, bool, error) {
	uri := strings.TrimSpace(os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI"))
	if uri == "" {
		if relative := strings.TrimSpace(os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI")); relative != "" {
			uri = "http://169.254.170.2" + relative
		}
	}
	if uri == "" {
		return s3Credentials{}, false, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return s3Credentials{}, false, fmt.Errorf("build container credential request: %w", err)
	}
	if token := strings.TrimSpace(os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN")); token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return s3Credentials{}, false, fmt.Errorf("fetch container credentials: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s3Credentials{}, false, fmt.Errorf("fetch container credentials: status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return s3Credentials{}, false, fmt.Errorf("read container credentials: %w", err)
	}
	var decoded containerCredentialsResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return s3Credentials{}, false, fmt.Errorf("parse container credentials: %w", err)
	}
	creds := s3Credentials{
		accessKey:    strings.TrimSpace(decoded.AccessKeyID),
		secretKey:    strings.TrimSpace(decoded.SecretAccessKey),
		sessionToken: strings.TrimSpace(decoded.Token),
	}
	if !creds.valid() {
		return s3Credentials{}, false, errors.New("container credentials response has no access key material")
	}
	if decoded.Expiration != "" {
		if expires, err := time.Parse(time.RFC3339, strings.TrimSpace(decoded.Expiration)); err == nil {
			creds.expiresAt = expires
		}
	}
	return creds, true, nil
}

func (p *s3CredentialsProvider) instanceCredentials(ctx context.Context) (s3Credentials, bool) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("AWS_EC2_METADATA_DISABLED")), "true") {
		return s3Credentials{}, false
	}
	client := &http.Client{Timeout: 2 * time.Second}
	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://169.254.169.254/latest/api/token", nil)
	if err != nil {
		return s3Credentials{}, false
	}
	tokenReq.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	tokenResp, err := client.Do(tokenReq)
	if err != nil {
		return s3Credentials{}, false
	}
	tokenRaw, err := io.ReadAll(io.LimitReader(tokenResp.Body, 4<<10))
	_ = tokenResp.Body.Close()
	if err != nil || tokenResp.StatusCode < 200 || tokenResp.StatusCode >= 300 {
		return s3Credentials{}, false
	}
	token := strings.TrimSpace(string(tokenRaw))
	if token == "" {
		return s3Credentials{}, false
	}
	get := func(path string) ([]byte, bool) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://169.254.169.254"+path, nil)
		if err != nil {
			return nil, false
		}
		req.Header.Set("X-aws-ec2-metadata-token", token)
		resp, err := client.Do(req)
		if err != nil {
			return nil, false
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, false
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err != nil {
			return nil, false
		}
		return raw, true
	}
	roleRaw, ok := get("/latest/meta-data/iam/security-credentials/")
	if !ok {
		return s3Credentials{}, false
	}
	role := strings.TrimSpace(string(roleRaw))
	if role == "" {
		return s3Credentials{}, false
	}
	raw, ok := get("/latest/meta-data/iam/security-credentials/" + role)
	if !ok {
		return s3Credentials{}, false
	}
	var decoded containerCredentialsResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return s3Credentials{}, false
	}
	creds := s3Credentials{
		accessKey:    strings.TrimSpace(decoded.AccessKeyID),
		secretKey:    strings.TrimSpace(decoded.SecretAccessKey),
		sessionToken: strings.TrimSpace(decoded.Token),
	}
	if !creds.valid() {
		return s3Credentials{}, false
	}
	if decoded.Expiration != "" {
		if expires, err := time.Parse(time.RFC3339, strings.TrimSpace(decoded.Expiration)); err == nil {
			creds.expiresAt = expires
		}
	}
	return creds, true
}
