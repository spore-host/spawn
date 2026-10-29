package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// pagedScan serves a Scan in pages, as DynamoDB does past 1 MB: each page but
// the last carries a LastEvaluatedKey to send back as ExclusiveStartKey.
type pagedScan struct {
	pages  [][]string // connection IDs per page
	failAt int        // page index that fails; -1 for none
}

func (s *pagedScan) Scan(_ context.Context, in *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	page := 0
	if k, ok := in.ExclusiveStartKey["page"].(*types.AttributeValueMemberN); ok {
		page, _ = strconv.Atoi(k.Value)
	}
	if page == s.failAt {
		return nil, errors.New("page failed")
	}
	out := &dynamodb.ScanOutput{}
	for _, id := range s.pages[page] {
		out.Items = append(out.Items, map[string]types.AttributeValue{
			"connection_id": &types.AttributeValueMemberS{Value: id},
		})
	}
	if page+1 < len(s.pages) {
		out.LastEvaluatedKey = map[string]types.AttributeValue{
			"page": &types.AttributeValueMemberN{Value: fmt.Sprint(page + 1)},
		}
	}
	return out, nil
}

// A connection past the first Scan page used to miss every broadcast (#1421).
func TestGetActiveConnectionsReadsEveryPage(t *testing.T) {
	s := &pagedScan{pages: [][]string{{"c1", "c2"}, {}, {"c3"}}, failAt: -1}
	conns, err := getActiveConnections(context.Background(), s)
	if err != nil || len(conns) != 3 {
		t.Fatalf("got %d connections, err %v; want all 3 across 3 pages", len(conns), err)
	}
}

func TestGetActiveConnectionsFailingPageIsAnError(t *testing.T) {
	s := &pagedScan{pages: [][]string{{"c1", "c2"}, {}, {"c3"}}, failAt: 1}
	conns, err := getActiveConnections(context.Background(), s)
	if err == nil || conns != nil {
		t.Fatalf("a failed second page gave %d connections, err %v; want an error and no list", len(conns), err)
	}
}
