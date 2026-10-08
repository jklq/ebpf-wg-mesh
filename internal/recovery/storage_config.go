package recovery

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

type StorageConfig struct {
	CAFile                string   `json:"caFile,omitempty"`
	Endpoint              string   `json:"endpoint"`
	Bucket                string   `json:"bucket"`
	Prefix                string   `json:"prefix"`
	Region                string   `json:"region"`
	ServerPublicKeySHA256 string   `json:"serverPublicKeySHA256,omitempty"`
	Owner                 string   `json:"owner"` // canonical owner returned by GetBucketAcl
	CredentialsFile       string   `json:"credentialsFile"`
	Profile               string   `json:"profile"`
	Account               string   `json:"account"`
	PrimaryAccount        string   `json:"primaryAccount"`
	FailureDomain         string   `json:"failureDomain"`
	PrimaryDomains        []string `json:"primaryDomains"`
	WriterPrincipal       string   `json:"writerPrincipal"`
}

func (c StorageConfig) Validate() error {
	if c.ServerPublicKeySHA256 != "" && (!digestPattern.MatchString(c.ServerPublicKeySHA256) || c.Endpoint == "") {
		return fmt.Errorf("storage server identity requires an explicit HTTPS endpoint and SHA256 public-key pin")
	}
	if c.CAFile != "" && (!filepath.IsAbs(c.CAFile) || filepath.Clean(c.CAFile) != c.CAFile) {
		return fmt.Errorf("storage TLS CA file must be an absolute clean path")
	}
	if c.Bucket == "" || c.Region == "" || (c.Owner == "" && c.ServerPublicKeySHA256 == "") || !filepath.IsAbs(c.CredentialsFile) || c.Profile == "" || c.Account == "" || c.PrimaryAccount == "" || c.Account == c.PrimaryAccount || c.FailureDomain == "" || c.WriterPrincipal == "" {
		return fmt.Errorf("recovery storage requires a bucket owner, independent account/domain and explicit credentials")
	}
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("recovery S3 endpoint requires an explicit HTTPS service origin")
		}
	}
	if strings.Trim(c.Prefix, "/") != c.Prefix || c.Prefix == "" || strings.Contains(c.Prefix, "..") {
		return fmt.Errorf("invalid recovery storage prefix")
	}
	for _, d := range c.PrimaryDomains {
		if d == c.FailureDomain {
			return fmt.Errorf("recovery storage shares primary failure domain %s", d)
		}
	}
	if len(c.PrimaryDomains) == 0 {
		return fmt.Errorf("primary site and storage failure domains must be declared")
	}
	return nil
}

// Require unconditional resource-policy denies, rather than inferring credential
// scope from the fact that a harmless read succeeded. IAM grants cannot override
// these explicit denies on the production writer principal.
func checkWriterPolicy(raw, writer, bucket string) error {
	var p struct {
		Statement []struct {
			Effect    string
			Principal any
			Action    any
			Resource  any
			Condition map[string]any
		}
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return fmt.Errorf("invalid recovery bucket policy")
	}
	values := func(v any) []string {
		switch x := v.(type) {
		case string:
			return []string{x}
		case []any:
			var out []string
			for _, v := range x {
				if s, ok := v.(string); ok {
					out = append(out, s)
				}
			}
			return out
		}
		return nil
	}
	denied := map[string]bool{}
	for _, statement := range p.Statement {
		if statement.Effect != "Deny" || len(statement.Condition) > 0 {
			continue
		}
		principal := statement.Principal
		if m, ok := principal.(map[string]any); ok {
			principal = m["AWS"]
		}
		applies := false
		for _, v := range values(principal) {
			if v == writer || v == "*" {
				applies = true
			}
		}
		if !applies {
			continue
		}
		for _, resource := range values(statement.Resource) {
			if resource != "*" && resource != "arn:aws:s3:::"+bucket && resource != "arn:aws:s3:::"+bucket+"/*" {
				continue
			}
			for _, action := range values(statement.Action) {
				denied[action+"/"+resource] = true
			}
		}
	}
	for _, action := range []string{"s3:DeleteObjectVersion", "s3:PutBucketVersioning", "s3:PutBucketObjectLockConfiguration", "s3:PutBucketPolicy", "s3:DeleteBucketPolicy", "s3:BypassGovernanceRetention", "s3:DeleteBucket"} {
		resource := "arn:aws:s3:::" + bucket
		if action == "s3:DeleteObjectVersion" || action == "s3:BypassGovernanceRetention" {
			resource += "/*"
		}
		if !denied[action+"/"+resource] && !denied[action+"/*"] && !denied["s3:*/"+resource] && !denied["*/*"] && !denied["s3:*/*"] {
			return fmt.Errorf("recovery bucket must explicitly deny production writer %s", action)
		}
	}
	return nil
}
