package aws

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// registrySource is the in-instance hybrid-registry client. The tests below read
// it rather than importing it, because what matters is which DynamoDB calls it
// makes and which table it names — the two facts the spored role must agree with.
const registrySource = "../registry/dynamodb.go"

// registryClientActions discovers the DynamoDB actions PeerRegistry (the type
// spored constructs) can issue, from `r.client.<Op>(` calls on its receiver.
// Fails when it finds none: an empty discovery has broken, not proven anything.
func registryClientActions(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(registrySource)
	if err != nil {
		t.Fatalf("read %s: %v", registrySource, err)
	}
	re := regexp.MustCompile(`\br\.client\.([A-Z][A-Za-z]+)\(`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		seen["dynamodb:"+m[1]] = true
	}
	if len(seen) == 0 {
		t.Fatalf("found no r.client.<Op>( calls in %s — the discovery is broken", registrySource)
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// registryTableName reads the table-name constant from the registry source.
func registryTableName(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(registrySource)
	if err != nil {
		t.Fatalf("read %s: %v", registrySource, err)
	}
	m := regexp.MustCompile(`(?m)^\s*TableName\s*=\s*"([^"]+)"`).FindStringSubmatch(string(src))
	if m == nil {
		t.Fatalf("no TableName constant in %s", registrySource)
	}
	return m[1]
}

// TestBothSporedPolicyPathsGrantTheHybridRegistry is the #905 IAM half.
//
// Every spored on an instance tagged spawn:job-array-id registers itself in the
// hybrid registry (pkg/agent), and `spawn burst` exists for exactly that: cloud
// members joining a registry-coordinated array. Neither spored policy granted any
// action on the table, so registration could never succeed. Both resolution paths
// are checked, because the no-flags path (SetupSporedIAMRole, the static policy)
// is the one `spawn burst` uses and the one a fix to the dynamic baseline alone
// would have missed (the #622 lesson).
func TestBothSporedPolicyPathsGrantTheHybridRegistry(t *testing.T) {
	wantActions := registryClientActions(t)
	table := registryTableName(t)
	client := &Client{}

	for name, doc := range map[string]string{
		"dynamic baseline (CreateOrGetInstanceProfile)": client.sporedBaselinePolicyDoc(),
		"static no-flags role (SetupSporedIAMRole)":     sporedDCVRolePolicy,
	} {
		t.Run(name, func(t *testing.T) {
			var parsed struct {
				Statement []struct {
					Effect   string      `json:"Effect"`
					Action   interface{} `json:"Action"`
					Resource interface{} `json:"Resource"`
				} `json:"Statement"`
			}
			if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
				t.Fatalf("policy is not valid JSON: %v\n%s", err, doc)
			}
			for _, want := range wantActions {
				granted := false
				for _, st := range parsed.Statement {
					if st.Effect != "Allow" || !actionsContain(st.Action, want) {
						continue
					}
					if resourcesMatchTable(st.Resource, table) {
						granted = true
					}
				}
				if !granted {
					t.Errorf("policy does not grant %s on table/%s — spored's registry client calls it, "+
						"so a job-array member cannot use the hybrid registry (#905)", want, table)
				}
			}
		})
	}
}

// TestHybridRegistryGrantIsNarrow: the grant is for one table in the caller's own
// account, and only the item-level calls the client makes. A broader grant (any
// table, dynamodb:*, another account) would give every spore in the account read
// and write over unrelated tables.
func TestHybridRegistryGrantIsNarrow(t *testing.T) {
	st := sporedHybridRegistryStatement()
	res, _ := st["Resource"].(string)
	want := "arn:aws:dynamodb:*:${aws:PrincipalAccount}:table/" + registryTableName(t)
	if res != want {
		t.Errorf("registry grant resource = %q, want exactly %q (one table, own account, no index or wildcard)", res, want)
	}

	got := append([]string(nil), sporedHybridRegistryActions...)
	sort.Strings(got)
	wantActions := registryClientActions(t)
	if strings.Join(got, ",") != strings.Join(wantActions, ",") {
		t.Errorf("registry grant actions = %v, but the registry client calls exactly %v — grant what it calls, nothing more", got, wantActions)
	}
	for _, a := range got {
		if strings.Contains(a, "*") {
			t.Errorf("registry grant contains wildcard action %q", a)
		}
	}
}

func resourcesMatchTable(resource interface{}, table string) bool {
	suffix := ":table/" + table
	switch v := resource.(type) {
	case string:
		return strings.HasSuffix(v, suffix) && strings.HasPrefix(v, "arn:aws:dynamodb:")
	case []interface{}:
		for _, r := range v {
			if s, ok := r.(string); ok && strings.HasSuffix(s, suffix) && strings.HasPrefix(s, "arn:aws:dynamodb:") {
				return true
			}
		}
	}
	return false
}
