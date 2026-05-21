package config

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

func TestLoadFromSSMWithClient_DomainRoundTrip(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	client := env.SSMClient()

	_, err := client.PutParameter(ctx, &ssm.PutParameterInput{
		Name:  aws.String(ssmDomainPath),
		Value: aws.String("substrate-test.example.com"),
		Type:  ssmtypes.ParameterTypeString,
	})
	if err != nil {
		t.Fatalf("PutParameter domain: %v", err)
	}

	cfg, err := loadFromSSMWithClient(ctx, client)
	if err != nil {
		t.Fatalf("loadFromSSMWithClient() error = %v", err)
	}
	if cfg.Domain != "substrate-test.example.com" {
		t.Errorf("Domain = %q, want %q", cfg.Domain, "substrate-test.example.com")
	}
}

func TestLoadFromSSMWithClient_BothParams(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	client := env.SSMClient()

	for _, p := range []struct{ name, val string }{
		{ssmDomainPath, "test.spore.host"},
		{ssmAPIEndpointPath, "https://api.test.spore.host/update-dns"},
	} {
		if _, err := client.PutParameter(ctx, &ssm.PutParameterInput{
			Name:  aws.String(p.name),
			Value: aws.String(p.val),
			Type:  ssmtypes.ParameterTypeString,
		}); err != nil {
			t.Fatalf("PutParameter %s: %v", p.name, err)
		}
	}

	cfg, err := loadFromSSMWithClient(ctx, client)
	if err != nil {
		t.Fatalf("loadFromSSMWithClient() error = %v", err)
	}
	if cfg.Domain != "test.spore.host" {
		t.Errorf("Domain = %q, want %q", cfg.Domain, "test.spore.host")
	}
	if cfg.APIEndpoint != "https://api.test.spore.host/update-dns" {
		t.Errorf("APIEndpoint = %q, want %q", cfg.APIEndpoint, "https://api.test.spore.host/update-dns")
	}
}

func TestLoadFromSSMWithClient_NoParams(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	cfg, err := loadFromSSMWithClient(ctx, env.SSMClient())
	if err == nil {
		t.Error("expected error when no SSM parameters exist, got nil")
	}
	if cfg != nil {
		t.Errorf("expected nil config when no parameters exist, got %+v", cfg)
	}
}

func TestLoadFromSSMWithClient_APIEndpointOnly(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	client := env.SSMClient()

	_, err := client.PutParameter(ctx, &ssm.PutParameterInput{
		Name:  aws.String(ssmAPIEndpointPath),
		Value: aws.String("https://custom-api.example.com/dns"),
		Type:  ssmtypes.ParameterTypeString,
	})
	if err != nil {
		t.Fatalf("PutParameter api_endpoint: %v", err)
	}

	cfg, err := loadFromSSMWithClient(ctx, client)
	if err != nil {
		t.Fatalf("loadFromSSMWithClient() error = %v", err)
	}
	if cfg.APIEndpoint != "https://custom-api.example.com/dns" {
		t.Errorf("APIEndpoint = %q, want %q", cfg.APIEndpoint, "https://custom-api.example.com/dns")
	}
	if cfg.Domain != "" {
		t.Errorf("Domain = %q, want empty", cfg.Domain)
	}
}
