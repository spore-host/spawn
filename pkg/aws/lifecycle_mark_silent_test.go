package aws

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/spore-host/libs/lifecycle"

	"github.com/spore-host/spawn/internal/sdkanswer"
	"github.com/spore-host/spawn/pkg/lifecyclemark"
)

// A creator whose lease mark is denied (spawn#1654) creates anyway and says
// nothing to the user: no line on stdout or stderr, one line in spawn's
// diagnostic log. Most users' IAM lacks the lease actions today (spawn#1666),
// and every CLI invocation is a new process, so any user-visible warning
// would be printed on every launch. The mark goes through a real DynamoDB
// client answered in-process, as a launch's does.
func TestADeniedLeaseMarkIsSilentAndTheCreateProceeds(t *testing.T) {
	var ops []string
	cfg := sdkanswer.Config("us-east-1", func(service, op string, _ any) (any, error) {
		ops = append(ops, service+"."+op)
		if service == "STS" {
			return &sts.GetCallerIdentityOutput{Account: sdkaws.String("111122223333"),
				Arn: sdkaws.String("arn:aws:iam::111122223333:user/u"), UserId: sdkaws.String("AIDAEXAMPLE")}, nil
		}
		if service == "DynamoDB" {
			return nil, &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized to perform: dynamodb:DescribeTable"}
		}
		return nil, nil
	})
	var userOut, debug bytes.Buffer
	restore := lifecyclemark.Override(func(c sdkaws.Config, region string) (lifecycle.API, error) {
		c = c.Copy()
		c.Region = region
		return dynamodb.NewFromConfig(c), nil
	}, &userOut, func(format string, args ...any) { fmt.Fprintf(&debug, format+"\n", args...) })
	defer restore()

	stdout, stderr := captureStd(t, func() {
		if err := NewClientFromConfig(cfg).ImportKeyPair(context.Background(), "us-east-1", "k", []byte("ssh-ed25519 AAAA x")); err != nil {
			t.Errorf("the key import failed: %v", err)
		}
	})
	joined := strings.Join(ops, " ")
	if !strings.Contains(joined, "DynamoDB.") || !strings.Contains(joined, "EC2.ImportKeyPair") {
		t.Fatalf("calls %v: want the lease mark tried, then the import", ops)
	}
	if strings.Index(joined, "DynamoDB.") > strings.Index(joined, "EC2.ImportKeyPair") {
		t.Errorf("calls %v: the mark must come before the create", ops)
	}
	if userOut.Len()+len(stdout)+len(stderr) != 0 {
		t.Errorf("a denied mark was visible to the user: out %q, stdout %q, stderr %q", userOut.String(), stdout, stderr)
	}
	if !strings.Contains(debug.String(), "could not mark spawn active") || !strings.Contains(debug.String(), "AccessDenied") {
		t.Errorf("no diagnostic log line for the denied mark: %q", debug.String())
	}
}

// captureStd runs f with os.Stdout and os.Stderr redirected, and returns what
// each received.
func captureStd(t *testing.T, f func()) (string, string) {
	t.Helper()
	read := func(r *os.File, into *string, done chan struct{}) {
		b, _ := io.ReadAll(r)
		*into = string(b)
		close(done)
	}
	or, ow, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	er, ew, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	savedOut, savedErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = ow, ew
	var outS, errS string
	od, ed := make(chan struct{}), make(chan struct{})
	go read(or, &outS, od)
	go read(er, &errS, ed)
	func() {
		defer func() { os.Stdout, os.Stderr = savedOut, savedErr }()
		f()
	}()
	_ = ow.Close()
	_ = ew.Close()
	<-od
	<-ed
	return outS, errS
}
