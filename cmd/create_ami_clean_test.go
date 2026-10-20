package cmd

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
)

// #1057: `spawn ami create` removes the source instance's --command run state
// over SSM BEFORE CreateImage, and refuses to image (unless --force) when that
// clean cannot be done, rather than bake a stale SPAWN_COMPLETE into the AMI.

type imagerRecord struct {
	calls   []string
	scripts []string
}

func fakeImager(rec *imagerRecord, res *aws.SSMRunResult, ssmErr error) amiImager {
	return amiImager{
		runSSM: func(_ context.Context, region, id, command string) (*aws.SSMRunResult, error) {
			rec.calls = append(rec.calls, "ssm:"+region+":"+id)
			rec.scripts = append(rec.scripts, command)
			return res, ssmErr
		},
		createAMI: func(_ context.Context, region string, in aws.CreateAMIInput) (string, error) {
			rec.calls = append(rec.calls, "create-image:"+region+":"+in.InstanceID)
			return "ami-0new", nil
		},
	}
}

var cleanOK = &aws.SSMRunResult{Status: "Success", Stdout: "removed\n" + launcher.ImageCleanOK + "\n"}

func linuxSource(state string) *aws.InstanceInfo {
	return &aws.InstanceInfo{
		InstanceID: "i-0src", Region: "us-west-2", State: state,
		Tags: map[string]string{"spawn:completion-file": "/data/done.json"},
	}
}

func TestAMICreate_CleansRunStateOverSSMBeforeCreateImage(t *testing.T) {
	rec := &imagerRecord{}
	id, err := fakeImager(rec, cleanOK, nil).createFromInstance(context.Background(), linuxSource("running"),
		aws.CreateAMIInput{InstanceID: "i-0src", Name: "img"}, false, io.Discard)
	if err != nil || id != "ami-0new" {
		t.Fatalf("id %q err %v", id, err)
	}
	want := "ssm:us-west-2:i-0src,create-image:us-west-2:i-0src"
	if got := strings.Join(rec.calls, ","); got != want {
		t.Fatalf("calls %s, want %s (the clean must come first)", got, want)
	}
	// The script is the launcher's, with this instance's ID and its own
	// --completion-file (from the spawn:completion-file tag), not the default.
	if wantScript := launcher.ImageStateCleanupScript("i-0src", "/data/done.json"); rec.scripts[0] != wantScript {
		t.Errorf("SSM ran a different script than launcher.ImageStateCleanupScript(id, tag):\n%s", rec.scripts[0])
	}
}

func TestAMICreate_RefusesWhenTheCleanFails(t *testing.T) {
	cases := map[string]struct {
		inst *aws.InstanceInfo
		res  *aws.SSMRunResult
		err  error
	}{
		"SSM unreachable":     {linuxSource("running"), nil, errors.New("InvalidInstanceId: not managed")},
		"no result":           {linuxSource("running"), nil, nil},
		"script failed":       {linuxSource("running"), &aws.SSMRunResult{Status: "Failed", ResponseCode: 1, Stderr: "spawn: could not remove: /tmp/SPAWN_COMPLETE"}, nil},
		"command running":     {linuxSource("running"), &aws.SSMRunResult{Status: "Failed", ResponseCode: int32(launcher.ImageCleanCommandRunning)}, nil},
		"success, no ok line": {linuxSource("running"), &aws.SSMRunResult{Status: "Success"}, nil},
		"stopped (no SSM)":    {linuxSource("stopped"), cleanOK, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := &imagerRecord{}
			_, err := fakeImager(rec, tc.res, tc.err).createFromInstance(context.Background(), tc.inst,
				aws.CreateAMIInput{InstanceID: "i-0src"}, false, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "--force") {
				t.Fatalf("err = %v; want a refusal that names --force", err)
			}
			for _, c := range rec.calls {
				if strings.HasPrefix(c, "create-image") {
					t.Fatalf("CreateImage was called after a failed clean: %v", rec.calls)
				}
			}

			// --force images anyway.
			rec = &imagerRecord{}
			if _, err := fakeImager(rec, tc.res, tc.err).createFromInstance(context.Background(), tc.inst,
				aws.CreateAMIInput{InstanceID: "i-0src"}, true, io.Discard); err != nil {
				t.Fatalf("--force: %v", err)
			}
			if last := rec.calls[len(rec.calls)-1]; !strings.HasPrefix(last, "create-image") {
				t.Fatalf("--force did not create the image: %v", rec.calls)
			}
		})
	}
}

func TestAMICreate_WindowsHasNoRunStateToClean(t *testing.T) {
	rec := &imagerRecord{}
	inst := &aws.InstanceInfo{InstanceID: "i-0win", Region: "us-east-2", State: "running", Tags: map[string]string{"spawn:os": "windows"}}
	if _, err := fakeImager(rec, nil, errors.New("must not be called")).createFromInstance(context.Background(), inst,
		aws.CreateAMIInput{InstanceID: "i-0win"}, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Join(rec.calls, ",") != "create-image:us-east-2:i-0win" {
		t.Errorf("calls %v", rec.calls)
	}
}

// TestRunCreateAMIGoesThroughTheCleaningImager: the command handler must not
// call CreateAMI directly, or the clean is bypassed.
func TestRunCreateAMIGoesThroughTheCleaningImager(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "create_ami.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found, viaImager, direct bool
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runCreateAMI" {
			continue
		}
		found = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				switch sel.Sel.Name {
				case "createFromInstance":
					viaImager = true
				case "CreateAMI":
					direct = true
				}
			}
			return true
		})
	}
	if !found {
		t.Fatal("runCreateAMI not found in create_ami.go")
	}
	if !viaImager || direct {
		t.Errorf("runCreateAMI: createFromInstance called=%v, direct CreateAMI=%v; want the imager only", viaImager, direct)
	}
}
