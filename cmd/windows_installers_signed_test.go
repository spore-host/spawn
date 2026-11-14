package cmd

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/sporedinstall"
)

// #1598: the Windows launch bootstrap runs the SSM agent's setup and the AWS CLI
// MSI as SYSTEM, so each must pass the Amazon signature check after its
// download and before it runs. Discovered from the generated user data: every
// Invoke-WebRequest -OutFile $v that is later run (Start-Process $v, or
// msiexec with $v) has an Assert-SpawnAmazonSigned $v in between. The AWS CLI
// MSI is the release the Linux bootstrap pins.
func TestWindowsUserDataRunsOnlyAmazonSignedInstallers(t *testing.T) {
	script, err := buildWindowsUserData("ssh-rsa AAAA test@spawn")
	if err != nil {
		t.Fatal(err)
	}
	download := regexp.MustCompile(`Invoke-WebRequest\b[^\n]*-OutFile (\$\w+)`)
	found := 0
	for _, m := range download.FindAllStringSubmatchIndex(script, -1) {
		v := script[m[2]:m[3]]
		rest := script[m[1]:]
		run := regexp.MustCompile(`Start-Process (` + regexp.QuoteMeta(v) + `\b|msiexec\.exe[^\n]*` + regexp.QuoteMeta(v) + `\b)`).FindStringIndex(rest)
		if run == nil {
			continue
		}
		found++
		if check := strings.Index(rest, sporedinstall.WindowsAmazonSignedFuncName+" "+v); check < 0 || check > run[0] {
			t.Errorf("%s is downloaded and run without %s %s first (#1598)", v, sporedinstall.WindowsAmazonSignedFuncName, v)
		}
	}
	if found < 2 {
		t.Fatalf("found %d downloaded installers that are run, want the SSM agent and the AWS CLI: the discovery is broken", found)
	}
	if !strings.Contains(script, "function "+sporedinstall.WindowsAmazonSignedFuncName) {
		t.Error("the user data calls the signature check without defining it")
	}
	if strings.Contains(script, "__") && regexp.MustCompile(`__[A-Z_]+__`).MatchString(script) {
		t.Errorf("an unreplaced placeholder is left: %v", regexp.MustCompile(`__[A-Z_]+__`).FindAllString(script, -1))
	}
	pin := launcher.AWSCLIPins()[0].Version
	if !strings.Contains(script, sporedinstall.WindowsAWSCLIMSIURL(pin)) {
		t.Errorf("the AWS CLI MSI is not the pinned %s", pin)
	}
}
