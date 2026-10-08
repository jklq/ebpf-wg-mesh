package recovery

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	ini "gopkg.in/ini.v1"
)

func storageClient(c StorageConfig) (*s3.Client, error) {
	if !filepath.IsAbs(c.CredentialsFile) || c.Profile == "" || c.Region == "" {
		return nil, fmt.Errorf("S3 requires explicit credential file, profile and region")
	}
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("S3 requires an HTTPS service origin")
		}
	}
	if c.ServerPublicKeySHA256 != "" && (c.Endpoint == "" || !digestPattern.MatchString(c.ServerPublicKeySHA256)) {
		return nil, fmt.Errorf("S3 identity pin requires a selected endpoint and SHA256")
	}
	b, err := privateFile(c.CredentialsFile)
	if err != nil {
		return nil, err
	}
	file, err := ini.Load(b)
	if err != nil {
		return nil, fmt.Errorf("invalid S3 credentials")
	}
	section, err := file.GetSection(c.Profile)
	if err != nil {
		return nil, fmt.Errorf("S3 credential profile is absent")
	}
	access, secret, token := section.Key("aws_access_key_id").String(), section.Key("aws_secret_access_key").String(), section.Key("aws_session_token").String()
	if access == "" || secret == "" {
		return nil, fmt.Errorf("S3 profile requires explicit access credentials")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CAFile != "" {
		b, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, err
		}
		tlsConfig.RootCAs = x509.NewCertPool()
		if !tlsConfig.RootCAs.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("invalid S3 TLS trust root")
		}
	}
	if c.ServerPublicKeySHA256 != "" {
		// VerifyConnection runs on every TLS connection, after normal hostname/chain
		// validation, including resumed sessions. No separate preflight TOCTOU exists.
		tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 || fmt.Sprintf("sha256:%x", sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo)) != c.ServerPublicKeySHA256 {
				return fmt.Errorf("S3 server public key differs from selected authority")
			}
			return nil
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.MaxIdleConnsPerHost = 16
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("S3 endpoint redirects are forbidden") }}
	cfg := aws.Config{Region: c.Region, Credentials: credentials.NewStaticCredentialsProvider(access, secret, token), HTTPClient: client, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
			o.UsePathStyle = true
		}
	}), nil
}
