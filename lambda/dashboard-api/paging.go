package main

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// A DynamoDB Query or Scan returns at most 1 MB per call, and a Scan's Limit
// counts items EVALUATED, before any FilterExpression. Reading only the first
// page therefore drops items silently, and the dashboard showed the rest as a
// complete list (#1421, the bug lagotto fixed in its own store as lagotto#181).
//
// Every helper here follows LastEvaluatedKey to the end (or to its stated
// bound) and returns an error, never a partial result, when any page fails.
// TestDashboardDynamoCallsArePaginated holds every Query/Scan in this package
// to these helpers, or to a `single-page:` justification beside the call.

// queryAll runs a Query to completion across every page.
func queryAll(ctx context.Context, api dynamodb.QueryAPIClient, input *dynamodb.QueryInput) ([]map[string]types.AttributeValue, error) {
	var items []map[string]types.AttributeValue
	p := dynamodb.NewQueryPaginator(api, input)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
	}
	return items, nil
}

// queryUpTo returns at most limit items, following pages until it has them or
// the Query is exhausted. truncated reports that more items exist beyond the
// limit, so a deliberately bounded "latest N" read can say it is partial
// instead of passing for the whole set. It reads limit+1 items to know.
func queryUpTo(ctx context.Context, api dynamodb.QueryAPIClient, input *dynamodb.QueryInput, limit int32) (items []map[string]types.AttributeValue, truncated bool, err error) {
	in := *input
	in.Limit = aws.Int32(limit + 1)
	p := dynamodb.NewQueryPaginator(api, &in)
	for p.HasMorePages() && len(items) <= int(limit) {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, false, err
		}
		items = append(items, page.Items...)
	}
	if len(items) > int(limit) {
		return items[:limit], true, nil
	}
	return items, false, nil
}

// scanAll runs a Scan to completion across every page. With a FilterExpression
// an early page can hold no matches while a later one does, so stopping early is
// never an answer.
func scanAll(ctx context.Context, api dynamodb.ScanAPIClient, input *dynamodb.ScanInput) ([]map[string]types.AttributeValue, error) {
	var items []map[string]types.AttributeValue
	p := dynamodb.NewScanPaginator(api, input)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
	}
	return items, nil
}

// scanFirst returns the first item a filtered Scan matches, following pages
// until one matches or the table is exhausted (nil, nil: no match).
func scanFirst(ctx context.Context, api dynamodb.ScanAPIClient, input *dynamodb.ScanInput) (map[string]types.AttributeValue, error) {
	p := dynamodb.NewScanPaginator(api, input)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		if len(page.Items) > 0 {
			return page.Items[0], nil
		}
	}
	return nil, nil
}
