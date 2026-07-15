package aws

import "fmt"

// GetEFSDNSName constructs the EFS DNS name from filesystem ID and region.
func GetEFSDNSName(filesystemID, region string) string {
	return fmt.Sprintf("%s.efs.%s.amazonaws.com", filesystemID, region)
}
