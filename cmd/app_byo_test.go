package cmd

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/spore-host/libs/catalog"
)

func TestEcrImageAccount(t *testing.T) {
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
		if got := ecrImageAccount(tt.image); got != tt.want {
			t.Errorf("ecrImageAccount(%q) = %q, want %q", tt.image, got, tt.want)
		}
	}
}

func TestSplitImageRef(t *testing.T) {
	tests := []struct {
		ref       string
		wantImage string
		wantTag   string
	}{
		{"public.ecr.aws/f8g1e7l5/paraview:5.13.2", "public.ecr.aws/f8g1e7l5/paraview", "5.13.2"},
		{"public.ecr.aws/f8g1e7l5/paraview", "public.ecr.aws/f8g1e7l5/paraview", ""},
		{"123456789012.dkr.ecr.us-east-1.amazonaws.com/x:1.0", "123456789012.dkr.ecr.us-east-1.amazonaws.com/x", "1.0"},
		{"registry:5000/repo/app:tag", "registry:5000/repo/app", "tag"}, // host:port not mistaken for tag
		{"registry:5000/repo/app", "registry:5000/repo/app", ""},
	}
	for _, tt := range tests {
		img, tag := splitImageRef(tt.ref)
		if img != tt.wantImage || tag != tt.wantTag {
			t.Errorf("splitImageRef(%q) = (%q,%q), want (%q,%q)", tt.ref, img, tag, tt.wantImage, tt.wantTag)
		}
	}
}

func TestEcrRegistryHost(t *testing.T) {
	if got := ecrRegistryHost("123456789012.dkr.ecr.us-east-1.amazonaws.com/x:1"); got != "123456789012.dkr.ecr.us-east-1.amazonaws.com" {
		t.Errorf("ecrRegistryHost = %q", got)
	}
}

func TestAppResolvable(t *testing.T) {
	pub := &catalog.AppEntry{Name: "pub", Image: "public.ecr.aws/x/pub", TagDefault: "1"}
	mine := &catalog.AppEntry{Name: "mine", Image: "111111111111.dkr.ecr.us-east-1.amazonaws.com/mine", TagDefault: "1"}
	theirs := &catalog.AppEntry{Name: "theirs", Image: "222222222222.dkr.ecr.us-east-1.amazonaws.com/theirs", TagDefault: "1"}
	legacy := &catalog.AppEntry{Name: "legacy", LaunchCommand: "/opt/x"}

	cases := []struct {
		name    string
		entry   *catalog.AppEntry
		account string
		want    bool
	}{
		{"public always resolvable", pub, "111111111111", true},
		{"public resolvable even with no creds", pub, "", true},
		{"own private resolvable", mine, "111111111111", true},
		{"others private not resolvable", theirs, "111111111111", false},
		{"private with no account not resolvable", mine, "", false},
		{"legacy launch_command resolvable", legacy, "111111111111", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, _ := appResolvable(c.entry, c.account); got != c.want {
				t.Errorf("appResolvable(%s, %q) = %v, want %v", c.entry.Name, c.account, got, c.want)
			}
		})
	}
}

func TestClassifyForList(t *testing.T) {
	cases := []struct {
		name       string
		entry      catalog.AppEntry
		account    string
		wantShow   bool
		wantStatus string
	}{
		{"public launchable", catalog.AppEntry{Name: "pub", Image: "public.ecr.aws/x/pub", TagDefault: "1"}, "111111111111", true, appStatusLaunchable},
		{"own private launchable", catalog.AppEntry{Name: "mine", Image: "111111111111.dkr.ecr.us-east-1.amazonaws.com/mine", TagDefault: "1"}, "111111111111", true, appStatusLaunchable},
		{"others private hidden", catalog.AppEntry{Name: "theirs", Image: "222222222222.dkr.ecr.us-east-1.amazonaws.com/theirs", TagDefault: "1"}, "111111111111", false, ""},
		{"recipe-only shown as recipe", catalog.AppEntry{Name: "pv", Recipe: "infra/amis/containers/paraview"}, "111111111111", true, appStatusRecipe},
		{"recipe shown even with no creds", catalog.AppEntry{Name: "pv", Recipe: "infra/x"}, "", true, appStatusRecipe},
		{"legacy launch_command launchable", catalog.AppEntry{Name: "igv", LaunchCommand: "/opt/igv"}, "", true, appStatusLaunchable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			show, status := classifyForList(&c.entry, c.account)
			if show != c.wantShow || status != c.wantStatus {
				t.Errorf("classifyForList(%s, %q) = (%v,%q), want (%v,%q)", c.entry.Name, c.account, show, status, c.wantShow, c.wantStatus)
			}
		})
	}
}

// TestBuildContainerDCVUserData_PrivateLogin asserts a private image's user-data
// authenticates to ECR before pulling; a public one does not.
func TestBuildContainerDCVUserData_PrivateLogin(t *testing.T) {
	priv := "111111111111.dkr.ecr.us-east-1.amazonaws.com/paraview:5.13.2"
	dec := func(enc string) string { b, _ := base64.StdEncoding.DecodeString(enc); return string(b) }

	privScript := dec(buildContainerDCVUserData(priv, true, true, "us-east-1", "console"))
	if !strings.Contains(privScript, "aws ecr get-login-password --region us-east-1") ||
		!strings.Contains(privScript, "docker login --username AWS") ||
		!strings.Contains(privScript, "111111111111.dkr.ecr.us-east-1.amazonaws.com") {
		t.Errorf("private user-data missing ECR login\n---\n%s", privScript)
	}

	pubScript := dec(buildContainerDCVUserData("public.ecr.aws/x/paraview:1", true, false, "us-east-1", "console"))
	if strings.Contains(pubScript, "ecr get-login-password") {
		t.Error("public user-data must NOT do an ECR login")
	}
}
