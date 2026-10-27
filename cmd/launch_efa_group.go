package cmd

import (
	"context"
	"fmt"
	"regexp"

	"github.com/spore-host/spawn/pkg/audit"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/progress"
)

// --efa-group NAME (#1168).
//
// EFA needs a security group that admits all traffic from itself, in and out.
// spawn made one only for --mpi (spawn-mpi-<job-array-name>), and an MPI job
// array is one instance type. A fleet of different types (a GPU client plus
// GPU-less EFA proxies) is several `spawn launch --efa` calls, so every user
// built that group by hand and had to remember to delete it. --efa-group names
// a group the launches create or join; it is deleted by the reaper's managed
// network sweep once no instance has used it for the grace period.

// efaGroupNameRe is the NAME part of spawn-efa-NAME. Narrower than what EC2
// accepts on purpose: it is also a CLI argument and appears in log lines.
var efaGroupNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// validateEFAGroup checks --efa-group before anything is created.
func validateEFAGroup(name string, efa bool) error {
	if name == "" {
		return nil
	}
	if !efa {
		return fmt.Errorf("--efa-group requires --efa")
	}
	if !efaGroupNameRe.MatchString(name) {
		return fmt.Errorf("--efa-group %q: use 1-63 letters, digits, '.', '_' or '-', starting with a letter or digit", name)
	}
	return nil
}

// efaGroupAPI is what ensureEFAGroup needs of *aws.Client.
type efaGroupAPI interface {
	ResolveVPC(ctx context.Context, region, explicitVPC string) (string, error)
	CreateOrGetEFASecurityGroup(ctx context.Context, region, vpcID, groupName string) (string, error)
}

// ensureEFAGroup creates or joins spawn-efa-<name> and adds it to the launch's
// groups, keeping any it already has (an MPI group, --security-group-ids).
// A no-op when name is empty.
func ensureEFAGroup(ctx context.Context, api efaGroupAPI, config *aws.LaunchConfig, name string, prog *progress.Progress, auditLog *audit.AuditLogger) error {
	if name == "" {
		return nil
	}
	const step = "Creating or joining EFA security group"
	prog.Start(step)
	vpcID, err := api.ResolveVPC(ctx, config.Region, config.VPCID)
	if err != nil {
		prog.Error(step, err)
		return err
	}
	sgName := aws.EFAGroupPrefix + name
	sgID, err := api.CreateOrGetEFASecurityGroup(ctx, config.Region, vpcID, sgName)
	if err != nil {
		prog.Error(step, err)
		auditLog.LogOperationWithRegion("create_security_group", sgName, config.Region, "failed", err)
		return fmt.Errorf("failed to create or join EFA security group %s: %w", sgName, err)
	}
	config.SecurityGroupIDs = mergeManagedSecurityGroup(config.SecurityGroupIDs, sgID)
	auditLog.LogOperationWithData("create_security_group", sgName, "success",
		map[string]interface{}{"security_group_id": sgID, "region": config.Region}, nil)
	prog.Complete(step)
	return nil
}
