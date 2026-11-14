package main

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The registry read and its IAM grant must agree (#1611): the grant the old
// Scan needed was never on the live role, so the updater silently fell back to
// DNS_ALLOWED_ACCOUNTS on every request. The request is one GetItem; the grant
// in scripts/deploy-custom-dns.sh must allow exactly that request and no
// wider one: that action, that table, and only the attributes it projects.
func TestRegistryGrantCoversExactlyTheRequest(t *testing.T) {
	b, err := os.ReadFile("../../scripts/deploy-custom-dns.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)cat > /tmp/spawn-dns-registry-policy\.json <<EOF\n(.*?)\nEOF\n`).FindSubmatch(b)
	if m == nil {
		t.Fatal("SpawnDNSAccountRegistryRead's policy heredoc not found in scripts/deploy-custom-dns.sh")
	}
	if !strings.Contains(string(b), `--policy-name "SpawnDNSAccountRegistryRead"`) {
		t.Fatal("the registry policy is not attached as SpawnDNSAccountRegistryRead")
	}
	doc := strings.NewReplacer("${REGION}", "us-east-1", "${ACCOUNT_ID}", "111122223333",
		"${DNS_ACCOUNTS_TABLE}", registryTableDefault).Replace(string(m[1]))
	var pol struct {
		Statement []struct {
			Effect    string
			Action    any
			Resource  any
			Condition map[string]map[string]any
		}
	}
	if err := json.Unmarshal([]byte(doc), &pol); err != nil {
		t.Fatalf("policy is not JSON: %v\n%s", err, doc)
	}
	if len(pol.Statement) != 1 {
		t.Fatalf("want one statement, got %d", len(pol.Statement))
	}
	st := pol.Statement[0]
	if st.Effect != "Allow" || st.Action != "dynamodb:GetItem" {
		t.Errorf("statement allows %v (%s); want exactly dynamodb:GetItem, the one call the updater makes", st.Action, st.Effect)
	}
	if st.Resource != "arn:aws:dynamodb:us-east-1:111122223333:table/"+registryTableDefault {
		t.Errorf("resource %v, want the registry table alone", st.Resource)
	}

	// The attributes the grant allows are the attributes the request projects.
	var granted []string
	if raw, ok := st.Condition["ForAllValues:StringEquals"]["dynamodb:Attributes"].([]any); ok {
		for _, a := range raw {
			granted = append(granted, a.(string))
		}
	}
	in := registryGetItemInput(registryTableDefault, "111122223333")
	var projected []string
	for _, v := range in.ExpressionAttributeNames {
		projected = append(projected, v)
	}
	sort.Strings(granted)
	sort.Strings(projected)
	if !slices.Equal(granted, projected) {
		t.Errorf("grant allows attributes %v, the request projects %v: they must be the same set", granted, projected)
	}
	if want := []string{registryKeyAttr, registryStatusAttr}; !slices.Equal(projected, want) {
		t.Errorf("request projects %v, want %v", projected, want)
	}
	// ForAllValues is vacuously true of a request naming no attributes (a
	// GetItem with no projection returns the whole item), so the key's
	// absence must deny.
	if st.Condition["Null"]["dynamodb:Attributes"] != "false" {
		t.Error(`no "Null": {"dynamodb:Attributes": "false"}: a GetItem with no ProjectionExpression would pass and read the whole row`)
	}
	if in.ProjectionExpression == nil || *in.ProjectionExpression == "" {
		t.Error("the request has no ProjectionExpression, so the grant denies it")
	}
	if k, ok := in.Key[registryKeyAttr]; !ok || len(in.Key) != 1 || k == nil {
		t.Errorf("request key %v, want exactly %s", in.Key, registryKeyAttr)
	}
}
