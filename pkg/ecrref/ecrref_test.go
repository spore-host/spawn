package ecrref

import "testing"

func TestAccount(t *testing.T) {
	tests := []struct {
		image string
		want  string
	}{
		{"123456789012.dkr.ecr.us-east-1.amazonaws.com/paraview:5.13.2", "123456789012"},
		{"123456789012.dkr.ecr.eu-west-1.amazonaws.com/x", "123456789012"},
		{"public.ecr.aws/f8g1e7l5/paraview:5.13.2", ""},
		{"myorg/paraview", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := Account(tt.image); got != tt.want {
			t.Errorf("Account(%q) = %q, want %q", tt.image, got, tt.want)
		}
	}
}

func TestRegistryHost(t *testing.T) {
	if got := RegistryHost("123456789012.dkr.ecr.us-east-1.amazonaws.com/x:1"); got != "123456789012.dkr.ecr.us-east-1.amazonaws.com" {
		t.Errorf("RegistryHost = %q", got)
	}
	if got := RegistryHost("nginx:latest"); got != "nginx:latest" {
		t.Errorf("RegistryHost with no slash should return the input unchanged, got %q", got)
	}
}

func TestAuthHost(t *testing.T) {
	tests := []struct {
		name           string
		image          string
		fallbackRegion string
		wantHost       string
		wantRegion     string
		wantOK         bool
	}{
		{
			name:           "private ECR, image's own region wins",
			image:          "123456789012.dkr.ecr.us-west-2.amazonaws.com/myimage:v1",
			fallbackRegion: "us-east-1",
			wantHost:       "123456789012.dkr.ecr.us-west-2.amazonaws.com",
			wantRegion:     "us-west-2",
			wantOK:         true,
		},
		{
			name:           "public image, no auth needed",
			image:          "nginx:latest",
			fallbackRegion: "us-east-1",
			wantOK:         false,
		},
		{
			name:           "public ECR alias, not private",
			image:          "public.ecr.aws/f8g1e7l5/paraview:5.13.2",
			fallbackRegion: "us-east-1",
			wantOK:         false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, region, ok := AuthHost(tt.image, tt.fallbackRegion)
			if ok != tt.wantOK {
				t.Fatalf("AuthHost(%q) ok = %v, want %v", tt.image, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if host != tt.wantHost || region != tt.wantRegion {
				t.Errorf("AuthHost(%q) = (%q, %q), want (%q, %q)", tt.image, host, region, tt.wantHost, tt.wantRegion)
			}
		})
	}
}
