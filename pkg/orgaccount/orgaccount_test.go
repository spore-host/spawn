package orgaccount

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	"github.com/aws/aws-sdk-go-v2/service/organizations/types"
)

type fakeOrg struct {
	out *organizations.DescribeOrganizationOutput
	err error
}

func (f fakeOrg) DescribeOrganization(context.Context, *organizations.DescribeOrganizationInput, ...func(*organizations.Options)) (*organizations.DescribeOrganizationOutput, error) {
	return f.out, f.err
}

func TestManagementAccount(t *testing.T) {
	org := func(mgmt string) *organizations.DescribeOrganizationOutput {
		return &organizations.DescribeOrganizationOutput{Organization: &types.Organization{MasterAccountId: aws.String(mgmt)}}
	}
	cases := []struct {
		name    string
		api     fakeOrg
		want    string
		wantErr bool
	}{
		{"in an organization", fakeOrg{out: org("111122223333")}, "111122223333", false},
		{"in no organization", fakeOrg{err: &types.AWSOrganizationsNotInUseException{Message: aws.String("not in use")}}, "", false},
		{"denied", fakeOrg{err: &types.AccessDeniedException{Message: aws.String("denied")}}, "", true},
		{"other failure", fakeOrg{err: errors.New("dial tcp: timeout")}, "", true},
		{"no management account in the answer", fakeOrg{out: &organizations.DescribeOrganizationOutput{}}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ManagementAccount(t.Context(), c.api)
			if got != c.want || (err != nil) != c.wantErr {
				t.Errorf("ManagementAccount = %q, %v; want %q, error %v", got, err, c.want, c.wantErr)
			}
		})
	}
}
