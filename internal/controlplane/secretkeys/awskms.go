package secretkeys

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
)

// DefaultKMSMaxAttempts bounds SDK retries for KMS wrap/unwrap calls.
const DefaultKMSMaxAttempts = 5

// DefaultKMSTimeout bounds a single KMS wrap/unwrap call.
const DefaultKMSTimeout = 10 * time.Second

// KMSConfig configures the production AWS KMS provider. Credentials are
// resolved through the SDK default chain (environment, shared config,
// container or instance workload identity) and are never logged.
type KMSConfig struct {
	// Region is the AWS region hosting the KMS keys. Required.
	Region string
	// Endpoint overrides the KMS endpoint for VPC endpoints or
	// KMS-compatible test doubles. Empty selects the regional endpoint.
	Endpoint string
	// KeyID is the default KMS key (ID, ARN, or alias) used for the
	// installation's first active envelope key. Required.
	KeyID string
	// Timeout bounds each KMS call. Defaults to DefaultKMSTimeout.
	Timeout time.Duration
	// MaxAttempts bounds SDK retries per call. Defaults to DefaultKMSMaxAttempts.
	MaxAttempts int
}

func (c KMSConfig) withDefaults() KMSConfig {
	if c.Timeout <= 0 {
		c.Timeout = DefaultKMSTimeout
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = DefaultKMSMaxAttempts
	}
	return c
}

// kmsAPI is the narrow KMS surface the provider needs. *kms.Client satisfies
// it; tests substitute a fake.
type kmsAPI interface {
	Encrypt(ctx context.Context, params *kms.EncryptInput, optFns ...func(*kms.Options)) (*kms.EncryptOutput, error)
	Decrypt(ctx context.Context, params *kms.DecryptInput, optFns ...func(*kms.Options)) (*kms.DecryptOutput, error)
}

// KMSProvider is the production KeyProvider. Data-encryption keys are
// wrapped with KMS Encrypt and unwrapped with KMS Decrypt; root key material
// never leaves KMS.
type KMSProvider struct {
	client kmsAPI
	config KMSConfig
}

// NewKMSProvider builds the production provider from operator configuration.
func NewKMSProvider(ctx context.Context, cfg KMSConfig) (*KMSProvider, error) {
	cfg = cfg.withDefaults()
	if strings.TrimSpace(cfg.Region) == "" {
		return nil, errors.New("aws kms region is required")
	}
	if strings.TrimSpace(cfg.KeyID) == "" {
		return nil, errors.New("aws kms key id is required")
	}
	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithRetryer(func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.MaxAttempts = cfg.MaxAttempts
			})
		}),
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	var kmsOpts []func(*kms.Options)
	if endpoint := strings.TrimSpace(cfg.Endpoint); endpoint != "" {
		kmsOpts = append(kmsOpts, func(o *kms.Options) {
			o.BaseEndpoint = aws.String(endpoint)
		})
	}
	return &KMSProvider{client: kms.NewFromConfig(awsCfg, kmsOpts...), config: cfg}, nil
}

// newKMSProviderForTest substitutes the KMS client. It stays unexported: only
// the constructor above builds production providers.
func newKMSProviderForTest(client kmsAPI, cfg KMSConfig) *KMSProvider {
	return &KMSProvider{client: client, config: cfg.withDefaults()}
}

// Name implements Provider.
func (p *KMSProvider) Name() string { return ProviderAWSKMS }

// DefaultKeyID reports the configured installation key used when no active
// envelope key exists yet.
func (p *KMSProvider) DefaultKeyID() string { return p.config.KeyID }

// ProvisionKey implements Provider. KMS keys are created out of band (AWS
// console, Terraform); hint carries the operator-supplied key ID, ARN, or
// alias, and ProvisionKey verifies it round-trips before the registry
// records it. An empty hint selects the configured default key.
func (p *KMSProvider) ProvisionKey(ctx context.Context, hint string) (string, error) {
	ref := strings.TrimSpace(hint)
	if ref == "" {
		ref = strings.TrimSpace(p.config.KeyID)
	}
	if ref == "" {
		return "", errors.New("aws kms key id is required")
	}
	proof := make([]byte, DEKSize)
	if _, err := rand.Read(proof); err != nil {
		return "", fmt.Errorf("generate kms key proof: %w", err)
	}
	wrapped, err := p.Wrap(ctx, ref, proof)
	if err != nil {
		return "", fmt.Errorf("verify kms key %q: %w", ref, err)
	}
	opened, err := p.Unwrap(ctx, ref, wrapped)
	if err != nil {
		return "", fmt.Errorf("verify kms key %q: %w", ref, err)
	}
	if len(opened) != len(proof) {
		return "", fmt.Errorf("verify kms key %q: round-trip mismatch", ref)
	}
	return ref, nil
}

// Wrap implements Provider via KMS Encrypt.
func (p *KMSProvider) Wrap(ctx context.Context, ref string, plaintext []byte) ([]byte, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, errors.New("aws kms wrap requires a key reference")
	}
	if len(plaintext) == 0 || len(plaintext) > 4096 {
		return nil, errors.New("aws kms wrap requires 1-4096 bytes of key material")
	}
	ctx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	out, err := p.client.Encrypt(ctx, &kms.EncryptInput{
		KeyId:     aws.String(ref),
		Plaintext: plaintext,
	})
	if err != nil {
		return nil, classifyKMSError("wrap", ref, err)
	}
	if len(out.CiphertextBlob) == 0 {
		return nil, fmt.Errorf("kms wrap with key %q returned empty ciphertext", ref)
	}
	return out.CiphertextBlob, nil
}

// Unwrap implements Provider via KMS Decrypt.
func (p *KMSProvider) Unwrap(ctx context.Context, ref string, wrapped []byte) ([]byte, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, errors.New("aws kms unwrap requires a key reference")
	}
	if len(wrapped) == 0 {
		return nil, errors.New("aws kms unwrap requires wrapped key material")
	}
	ctx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	out, err := p.client.Decrypt(ctx, &kms.DecryptInput{
		KeyId:          aws.String(ref),
		CiphertextBlob: wrapped,
	})
	if err != nil {
		return nil, classifyKMSError("unwrap", ref, err)
	}
	if len(out.Plaintext) == 0 {
		return nil, fmt.Errorf("kms unwrap with key %q returned empty plaintext", ref)
	}
	return out.Plaintext, nil
}

// Close implements Provider.
func (p *KMSProvider) Close() error { return nil }

func classifyKMSError(op, ref string, err error) error {
	var notFound *types.NotFoundException
	if errors.As(err, &notFound) {
		return fmt.Errorf("%w: kms %s key %q: %w", ErrProviderKeyNotFound, op, ref, err)
	}
	var disabled *types.DisabledException
	if errors.As(err, &disabled) {
		return fmt.Errorf("%w: kms %s key %q is disabled: %w", ErrProviderKeyNotFound, op, ref, err)
	}
	var invalidKey *types.InvalidKeyUsageException
	if errors.As(err, &invalidKey) {
		return fmt.Errorf("%w: kms %s key %q cannot encrypt: %w", ErrProviderKeyNotFound, op, ref, err)
	}
	var invalidCipher *types.InvalidCiphertextException
	if errors.As(err, &invalidCipher) {
		return fmt.Errorf("%w: kms %s under key %q: %w", ErrCiphertextInvalid, op, ref, err)
	}
	var kmsInvalid *types.KMSInvalidStateException
	if errors.As(err, &kmsInvalid) {
		return fmt.Errorf("%w: kms %s key %q: %w", ErrProviderKeyNotFound, op, ref, err)
	}
	return fmt.Errorf("kms %s with key %q: %w", op, ref, err)
}
