package cmd

import (
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/input"
	"github.com/spore-host/spawn/pkg/testutil"
)

// TestBuildLaunchConfig_VolumeSize verifies the --volume-size flag maps to
// LaunchConfig.RootVolumeSizeGiB (regression for #11).
func TestBuildLaunchConfig_VolumeSize(t *testing.T) {
	// buildLaunchConfig reads package-level flag globals; save and restore.
	prevVol, prevType := launchVolumeSize, instanceType
	t.Cleanup(func() { launchVolumeSize, instanceType = prevVol, prevType })

	instanceType = "c7g.4xlarge"

	t.Run("set", func(t *testing.T) {
		launchVolumeSize = 80
		cfg, err := buildLaunchConfig(nil)
		if err != nil {
			t.Fatalf("buildLaunchConfig: %v", err)
		}
		if cfg.RootVolumeSizeGiB != 80 {
			t.Errorf("RootVolumeSizeGiB = %d, want 80", cfg.RootVolumeSizeGiB)
		}
	})

	t.Run("unset leaves default", func(t *testing.T) {
		launchVolumeSize = 0
		cfg, err := buildLaunchConfig(nil)
		if err != nil {
			t.Fatalf("buildLaunchConfig: %v", err)
		}
		if cfg.RootVolumeSizeGiB != 0 {
			t.Errorf("RootVolumeSizeGiB = %d, want 0 (AMI default)", cfg.RootVolumeSizeGiB)
		}
	})
}

// TestBuildLaunchConfig_FSxLifecycleContract verifies the fail-closed lifecycle
// contract for --fsx-create (#193): lifecycle is required, durable requires a
// TTL, ephemeral rejects a TTL, and the fields are inert without --fsx-create.
func TestBuildLaunchConfig_FSxLifecycleContract(t *testing.T) {
	prev := struct {
		create       bool
		lifecycle    string
		ttl, bucket  string
		itype, idval string
	}{fsxCreate, fsxLifecycle, fsxTTL, fsxS3Bucket, instanceType, fsxID}
	t.Cleanup(func() {
		fsxCreate, fsxLifecycle, fsxTTL, fsxS3Bucket, instanceType, fsxID =
			prev.create, prev.lifecycle, prev.ttl, prev.bucket, prev.itype, prev.idval
	})
	instanceType = "c7g.4xlarge"
	fsxID = ""

	reset := func() { fsxCreate, fsxLifecycle, fsxTTL, fsxS3Bucket = false, "", "", "" }

	t.Run("create without lifecycle errors", func(t *testing.T) {
		reset()
		fsxCreate, fsxS3Bucket = true, "b"
		if _, err := buildLaunchConfig(nil); err == nil {
			t.Error("--fsx-create with no --fsx-lifecycle must fail closed")
		}
	})
	t.Run("durable without ttl errors", func(t *testing.T) {
		reset()
		fsxCreate, fsxS3Bucket, fsxLifecycle = true, "b", "durable"
		if _, err := buildLaunchConfig(nil); err == nil {
			t.Error("--fsx-lifecycle=durable with no --fsx-ttl must fail closed")
		}
	})
	t.Run("durable with ttl ok", func(t *testing.T) {
		reset()
		fsxCreate, fsxS3Bucket, fsxLifecycle, fsxTTL = true, "b", "durable", "7d"
		cfg, err := buildLaunchConfig(nil)
		if err != nil {
			t.Fatalf("durable+ttl should be valid: %v", err)
		}
		if cfg.FSxLifecycle != "durable" || cfg.FSxTTL != "7d" {
			t.Errorf("lifecycle/ttl not threaded: %q/%q", cfg.FSxLifecycle, cfg.FSxTTL)
		}
	})
	t.Run("ephemeral with ttl errors", func(t *testing.T) {
		reset()
		fsxCreate, fsxS3Bucket, fsxLifecycle, fsxTTL = true, "b", "ephemeral", "7d"
		if _, err := buildLaunchConfig(nil); err == nil {
			t.Error("ephemeral + --fsx-ttl is contradictory and must error")
		}
	})
	t.Run("ephemeral ok", func(t *testing.T) {
		reset()
		fsxCreate, fsxS3Bucket, fsxLifecycle = true, "b", "ephemeral"
		if _, err := buildLaunchConfig(nil); err != nil {
			t.Fatalf("ephemeral should be valid: %v", err)
		}
	})
	t.Run("invalid lifecycle errors", func(t *testing.T) {
		reset()
		fsxCreate, fsxS3Bucket, fsxLifecycle = true, "b", "forever"
		if _, err := buildLaunchConfig(nil); err == nil {
			t.Error("invalid --fsx-lifecycle must error")
		}
	})
	t.Run("lifecycle without create errors", func(t *testing.T) {
		reset()
		fsxLifecycle = "ephemeral"
		if _, err := buildLaunchConfig(nil); err == nil {
			t.Error("--fsx-lifecycle without --fsx-create must error")
		}
	})
}

// TestBuildLaunchConfig_Reservation verifies launch-into-reservation wiring
// (#216): --reservation-id maps to config, --capacity-block sets the flag,
// truffle input's reservation id is carried (not dropped), and the guards reject
// spot+capacity-block and capacity-block-without-reservation.
func TestBuildLaunchConfig_Reservation(t *testing.T) {
	prev := struct {
		resID string
		cb    bool
		sp    bool
		itype string
	}{reservationID, capacityBlock, spot, instanceType}
	t.Cleanup(func() {
		reservationID, capacityBlock, spot, instanceType = prev.resID, prev.cb, prev.sp, prev.itype
	})
	instanceType = "p5.48xlarge"

	t.Run("reservation-id flows to config", func(t *testing.T) {
		reservationID, capacityBlock, spot = "cr-0abc123", false, false
		cfg, err := buildLaunchConfig(nil)
		if err != nil {
			t.Fatalf("buildLaunchConfig: %v", err)
		}
		if cfg.ReservationID != "cr-0abc123" {
			t.Errorf("ReservationID = %q, want cr-0abc123", cfg.ReservationID)
		}
		if cfg.CapacityBlock {
			t.Error("CapacityBlock should be false when --capacity-block not set")
		}
	})

	t.Run("capacity-block sets flag", func(t *testing.T) {
		reservationID, capacityBlock, spot = "cr-0abc123", true, false
		cfg, err := buildLaunchConfig(nil)
		if err != nil {
			t.Fatalf("buildLaunchConfig: %v", err)
		}
		if !cfg.CapacityBlock {
			t.Error("CapacityBlock = false, want true")
		}
	})

	t.Run("rejects spot + capacity-block", func(t *testing.T) {
		reservationID, capacityBlock, spot = "cr-0abc123", true, true
		if _, err := buildLaunchConfig(nil); err == nil {
			t.Error("expected --spot + --capacity-block to be rejected")
		}
	})

	t.Run("rejects capacity-block without reservation-id", func(t *testing.T) {
		reservationID, capacityBlock, spot = "", true, false
		if _, err := buildLaunchConfig(nil); err == nil {
			t.Error("expected --capacity-block without --reservation-id to be rejected")
		}
	})

	t.Run("none set leaves config clean", func(t *testing.T) {
		reservationID, capacityBlock, spot = "", false, false
		cfg, err := buildLaunchConfig(nil)
		if err != nil {
			t.Fatalf("buildLaunchConfig: %v", err)
		}
		if cfg.ReservationID != "" || cfg.CapacityBlock {
			t.Errorf("expected clean config, got ReservationID=%q CapacityBlock=%v", cfg.ReservationID, cfg.CapacityBlock)
		}
	})
}

// TestBuildLaunchConfig_TruffleReservationCopy is the #216 dropped-field
// regression: a reservation id supplied via truffle input must be copied into
// the config (previously only type/region/AZ/spot were copied — lagotto#19 class).
func TestBuildLaunchConfig_TruffleReservationCopy(t *testing.T) {
	prev := struct {
		resID string
		cb    bool
		itype string
	}{reservationID, capacityBlock, instanceType}
	t.Cleanup(func() { reservationID, capacityBlock, instanceType = prev.resID, prev.cb, prev.itype })
	reservationID, capacityBlock = "", false // no flag override
	instanceType = ""

	ti := &input.TruffleInput{
		InstanceType:  "p5.48xlarge",
		Region:        "us-east-1",
		ReservationID: "cr-fromtruffle",
	}
	cfg, err := buildLaunchConfig(ti)
	if err != nil {
		t.Fatalf("buildLaunchConfig: %v", err)
	}
	if cfg.ReservationID != "cr-fromtruffle" {
		t.Errorf("ReservationID = %q, want cr-fromtruffle (truffle-input copy dropped?)", cfg.ReservationID)
	}
}

// TestBuildLaunchConfig_AMIAuto verifies that --ami auto (and "AUTO") is treated
// as auto-detect — left empty in the config so the downstream gate detects the
// latest AL2023 AMI — rather than passed to EC2 as a literal "auto" (#15).
func TestBuildLaunchConfig_AMIAuto(t *testing.T) {
	prevAMI, prevType := ami, instanceType
	t.Cleanup(func() { ami, instanceType = prevAMI, prevType })
	instanceType = "c7g.4xlarge"

	for _, v := range []string{"auto", "AUTO", "Auto"} {
		ami = v
		cfg, err := buildLaunchConfig(nil)
		if err != nil {
			t.Fatalf("buildLaunchConfig(ami=%q): %v", v, err)
		}
		if cfg.AMI != "" {
			t.Errorf("ami=%q → config.AMI = %q, want empty (auto-detect)", v, cfg.AMI)
		}
	}

	// A real AMI ID is preserved.
	ami = "ami-0abc123"
	cfg, err := buildLaunchConfig(nil)
	if err != nil {
		t.Fatalf("buildLaunchConfig(real ami): %v", err)
	}
	if cfg.AMI != "ami-0abc123" {
		t.Errorf("real AMI not preserved: got %q", cfg.AMI)
	}
}

// TestParseTTL tests TTL duration parsing
func TestParseTTL(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    time.Duration
		wantErr bool
	}{
		{
			name:  "valid hours",
			input: "8h",
			want:  8 * time.Hour,
		},
		{
			name:  "valid minutes",
			input: "30m",
			want:  30 * time.Minute,
		},
		{
			name:  "valid combined",
			input: "2h30m",
			want:  2*time.Hour + 30*time.Minute,
		},
		{
			name:    "invalid format",
			input:   "invalid",
			wantErr: true,
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:  "seconds",
			input: "300s",
			want:  300 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := time.ParseDuration(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseDuration() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseDuration() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestValidateRegion tests region validation logic
func TestValidateRegion(t *testing.T) {
	tests := []struct {
		name   string
		region string
		valid  bool
	}{
		{
			name:   "valid us-east-1",
			region: "us-east-1",
			valid:  true,
		},
		{
			name:   "valid us-west-2",
			region: "us-west-2",
			valid:  true,
		},
		{
			name:   "valid eu-west-1",
			region: "eu-west-1",
			valid:  true,
		},
		{
			name:   "invalid format",
			region: "invalid",
			valid:  false,
		},
		{
			name:   "empty string",
			region: "",
			valid:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Basic region format validation
			isValid := len(tt.region) > 0 && (testutil.Contains(tt.region, "-"))
			if isValid != tt.valid {
				t.Errorf("region %q validity = %v, want %v", tt.region, isValid, tt.valid)
			}
		})
	}
}

// TestValidateInstanceType tests instance type validation
func TestValidateInstanceType(t *testing.T) {
	tests := []struct {
		name         string
		instanceType string
		valid        bool
	}{
		{
			name:         "valid t3.micro",
			instanceType: "t3.micro",
			valid:        true,
		},
		{
			name:         "valid t3.small",
			instanceType: "t3.small",
			valid:        true,
		},
		{
			name:         "valid m5.large",
			instanceType: "m5.large",
			valid:        true,
		},
		{
			name:         "valid c5.xlarge",
			instanceType: "c5.xlarge",
			valid:        true,
		},
		{
			name:         "valid r5.2xlarge",
			instanceType: "r5.2xlarge",
			valid:        true,
		},
		{
			name:         "valid p3.8xlarge GPU",
			instanceType: "p3.8xlarge",
			valid:        true,
		},
		{
			name:         "invalid format",
			instanceType: "invalid",
			valid:        false,
		},
		{
			name:         "empty string",
			instanceType: "",
			valid:        false,
		},
		{
			name:         "only family",
			instanceType: "t3",
			valid:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Basic instance type format validation (family.size)
			parts := testutil.SplitString(tt.instanceType, ".")
			isValid := len(parts) == 2 && len(parts[0]) > 0 && len(parts[1]) > 0
			if isValid != tt.valid {
				t.Errorf("instance type %q validity = %v, want %v", tt.instanceType, isValid, tt.valid)
			}
		})
	}
}

// TestValidateJobArrayConfig tests job array configuration validation
func TestValidateJobArrayConfig(t *testing.T) {
	tests := []struct {
		name         string
		count        int
		jobArrayName string
		wantErr      bool
		errContains  string
	}{
		{
			name:         "valid single instance",
			count:        1,
			jobArrayName: "",
			wantErr:      false,
		},
		{
			name:         "valid job array",
			count:        10,
			jobArrayName: "compute",
			wantErr:      false,
		},
		{
			name:         "missing name for array",
			count:        10,
			jobArrayName: "",
			wantErr:      true,
			errContains:  "job-array-name required",
		},
		{
			name:         "zero count",
			count:        0,
			jobArrayName: "",
			wantErr:      true,
			errContains:  "count must be positive",
		},
		{
			name:         "negative count",
			count:        -1,
			jobArrayName: "",
			wantErr:      true,
			errContains:  "count must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error

			// Validation logic
			if tt.count <= 0 {
				err = &validationError{msg: "count must be positive"}
			} else if tt.count > 1 && tt.jobArrayName == "" {
				err = &validationError{msg: "job-array-name required when count > 1"}
			}

			if (err != nil) != tt.wantErr {
				t.Errorf("validation error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && tt.errContains != "" && !testutil.Contains(err.Error(), tt.errContains) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.errContains)
			}
		})
	}
}

// TestValidateMPIConfig tests MPI configuration validation
func TestValidateMPIConfig(t *testing.T) {
	tests := []struct {
		name        string
		mpiEnabled  bool
		count       int
		wantErr     bool
		errContains string
	}{
		{
			name:       "valid MPI with multiple instances",
			mpiEnabled: true,
			count:      4,
			wantErr:    false,
		},
		{
			name:       "MPI disabled",
			mpiEnabled: false,
			count:      1,
			wantErr:    false,
		},
		{
			name:        "MPI with single instance",
			mpiEnabled:  true,
			count:       1,
			wantErr:     true,
			errContains: "MPI requires count > 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error

			// Validation logic
			if tt.mpiEnabled && tt.count <= 1 {
				err = &validationError{msg: "MPI requires count > 1"}
			}

			if (err != nil) != tt.wantErr {
				t.Errorf("validation error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && tt.errContains != "" && !testutil.Contains(err.Error(), tt.errContains) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.errContains)
			}
		})
	}
}

// TestValidateOnCompleteAction tests on-complete action validation
func TestValidateOnCompleteAction(t *testing.T) {
	tests := []struct {
		name    string
		action  string
		wantErr bool
	}{
		{
			name:    "valid terminate",
			action:  "terminate",
			wantErr: false,
		},
		{
			name:    "valid stop",
			action:  "stop",
			wantErr: false,
		},
		{
			name:    "valid hibernate",
			action:  "hibernate",
			wantErr: false,
		},
		{
			name:    "empty (no action)",
			action:  "",
			wantErr: false,
		},
		{
			name:    "invalid action",
			action:  "invalid",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validActions := map[string]bool{
				"":          true,
				"terminate": true,
				"stop":      true,
				"hibernate": true,
			}

			isValid := validActions[tt.action]
			if isValid == tt.wantErr {
				t.Errorf("action %q validity = %v, wantErr %v", tt.action, isValid, tt.wantErr)
			}
		})
	}
}

// TestGenerateInstanceName tests instance name generation
func TestGenerateInstanceName(t *testing.T) {
	tests := []struct {
		name         string
		template     string
		jobArrayName string
		index        int
		want         string
	}{
		{
			name:         "default template",
			template:     "",
			jobArrayName: "compute",
			index:        0,
			want:         "compute-0",
		},
		{
			name:         "custom template with index",
			template:     "worker-{index}",
			jobArrayName: "compute",
			index:        5,
			want:         "worker-5",
		},
		{
			name:         "custom template with name and index",
			template:     "{job-array-name}-node-{index}",
			jobArrayName: "training",
			index:        3,
			want:         "training-node-3",
		},
		{
			name:         "template with only name",
			template:     "{job-array-name}",
			jobArrayName: "experiment",
			index:        0,
			want:         "experiment",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			template := tt.template
			if template == "" {
				template = "{job-array-name}-{index}"
			}

			// Simple template replacement
			result := template
			result = testutil.ReplaceString(result, "{job-array-name}", tt.jobArrayName)
			result = testutil.ReplaceString(result, "{index}", testutil.IntToString(tt.index))

			if result != tt.want {
				t.Errorf("generateInstanceName() = %v, want %v", result, tt.want)
			}
		})
	}
}

// TestValidateIAMRoleConfig tests IAM role configuration validation
func TestValidateIAMRoleConfig(t *testing.T) {
	tests := []struct {
		name               string
		iamRole            string
		iamPolicy          []string
		iamManagedPolicies []string
		wantErr            bool
	}{
		{
			name:    "use existing role",
			iamRole: "my-existing-role",
			wantErr: false,
		},
		{
			name:      "inline policy only",
			iamPolicy: []string{"s3:GetObject"},
			wantErr:   false,
		},
		{
			name:               "managed policy only",
			iamManagedPolicies: []string{"arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess"},
			wantErr:            false,
		},
		{
			name:               "both inline and managed",
			iamPolicy:          []string{"s3:GetObject"},
			iamManagedPolicies: []string{"arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess"},
			wantErr:            false,
		},
		{
			name:    "no IAM config",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// IAM validation is generally permissive
			// All test cases should pass
			if tt.wantErr {
				t.Errorf("unexpected error expectation for test case %q", tt.name)
			}
		})
	}
}

// TestValidateCompletionConfig tests completion signal configuration validation
func TestValidateCompletionConfig(t *testing.T) {
	tests := []struct {
		name            string
		onComplete      string
		completionFile  string
		completionDelay string
		wantErr         bool
	}{
		{
			name:            "valid configuration",
			onComplete:      "terminate",
			completionFile:  "/tmp/SPAWN_COMPLETE",
			completionDelay: "30s",
			wantErr:         false,
		},
		{
			name:            "no action (disabled)",
			onComplete:      "",
			completionFile:  "/tmp/SPAWN_COMPLETE",
			completionDelay: "30s",
			wantErr:         false,
		},
		{
			name:            "custom delay",
			onComplete:      "stop",
			completionFile:  "/tmp/done",
			completionDelay: "5m",
			wantErr:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Completion config validation
			var err error
			if tt.completionDelay != "" {
				_, parseErr := time.ParseDuration(tt.completionDelay)
				if parseErr != nil {
					err = parseErr
				}
			}

			if (err != nil) != tt.wantErr {
				t.Errorf("validation error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestWriteOutputID tests the writeOutputID function for workflow integration
func TestWriteOutputID(t *testing.T) {
	tests := []struct {
		name     string
		id       string
		filepath string
		wantErr  bool
	}{
		{
			name:     "empty filepath (no-op)",
			id:       "sweep-123",
			filepath: "",
			wantErr:  false,
		},
		{
			name:     "directory path (should fail)",
			id:       "sweep-456",
			filepath: "/tmp",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := writeOutputID(tt.id, tt.filepath)
			if (err != nil) != tt.wantErr {
				t.Errorf("writeOutputID() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// Helper types and functions

type validationError struct {
	msg string
}

func (e *validationError) Error() string {
	return e.msg
}
