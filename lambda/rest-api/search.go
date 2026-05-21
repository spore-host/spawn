package main

import (
	"context"
	"net/http"
	"strconv"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
	"github.com/spore-host/truffle/pkg/find"
	"github.com/spore-host/truffle/pkg/quotas"
)

func handleSearch(ctx context.Context, cfg aws.Config, req events.APIGatewayV2HTTPRequest, p *Principal) (events.APIGatewayV2HTTPResponse, error) {
	q := req.QueryStringParameters["q"]
	if q == "" {
		return errResp(http.StatusBadRequest, "q parameter required"), nil
	}
	regionStr := req.QueryStringParameters["region"]
	regions := []string{"us-east-1"}
	if regionStr != "" {
		regions = splitCSV(regionStr)
	}

	pq, err := find.ParseQuery(q)
	if err != nil {
		return errResp(http.StatusBadRequest, "invalid query"), nil
	}
	criteria, err := pq.BuildCriteria()
	if err != nil {
		return errResp(http.StatusInternalServerError, "build criteria failed"), nil
	}

	client := truffleaws.NewClientFromConfig(cfg)

	results, err := client.SearchInstanceTypes(ctx, regions, criteria.InstanceTypePattern, criteria.FilterOptions)
	if err != nil {
		return errResp(http.StatusInternalServerError, "search failed"), nil
	}

	return jsonResp(http.StatusOK, map[string]any{
		"results": results,
		"count":   len(results),
		"query":   q,
		"regions": regions,
	}), nil
}

func handleSpot(ctx context.Context, cfg aws.Config, req events.APIGatewayV2HTTPRequest, p *Principal) (events.APIGatewayV2HTTPResponse, error) {
	instanceType := req.QueryStringParameters["type"]
	if instanceType == "" {
		return errResp(http.StatusBadRequest, "type parameter required"), nil
	}
	regionStr := req.QueryStringParameters["region"]
	regions := []string{"us-east-1"}
	if regionStr != "" {
		regions = splitCSV(regionStr)
	}

	client := truffleaws.NewClientFromConfig(cfg)

	// Find the instance type first
	results, err := client.SearchInstanceTypes(ctx, regions, nil, truffleaws.FilterOptions{})
	if err != nil {
		return errResp(http.StatusInternalServerError, "search failed"), nil
	}
	var filtered []truffleaws.InstanceTypeResult
	for _, r := range results {
		if r.InstanceType == instanceType {
			filtered = append(filtered, r)
		}
	}

	prices, err := client.GetSpotPricing(ctx, filtered, truffleaws.SpotOptions{ShowSavings: true})
	if err != nil {
		return errResp(http.StatusInternalServerError, "spot pricing failed"), nil
	}

	return jsonResp(http.StatusOK, map[string]any{"prices": prices, "instance_type": instanceType}), nil
}

func handleQuota(ctx context.Context, cfg aws.Config, req events.APIGatewayV2HTTPRequest, p *Principal) (events.APIGatewayV2HTTPResponse, error) {
	instanceType := req.QueryStringParameters["type"]
	region := req.QueryStringParameters["region"]
	if instanceType == "" || region == "" {
		return errResp(http.StatusBadRequest, "type and region required"), nil
	}
	isSpot := req.QueryStringParameters["spot"] == "true"

	qc := quotas.NewClientFromConfig(cfg)
	info, err := qc.GetQuotas(ctx, region)
	if err != nil {
		return errResp(http.StatusInternalServerError, "quota lookup failed"), nil
	}

	// Find vCPUs for this instance type
	tclient := truffleaws.NewClientFromConfig(cfg)
	results, _ := tclient.SearchInstanceTypes(ctx, []string{region}, nil, truffleaws.FilterOptions{})
	var vcpus int32
	for _, r := range results {
		if r.InstanceType == instanceType {
			vcpus = r.VCPUs
			break
		}
	}

	canLaunch, msg := qc.CanLaunch(instanceType, vcpus, info, isSpot)
	return jsonResp(http.StatusOK, map[string]any{
		"instance_type": instanceType,
		"region":        region,
		"spot":          isSpot,
		"can_launch":    canLaunch,
		"message":       msg,
		"vcpus":         strconv.Itoa(int(vcpus)),
	}), nil
}

func splitCSV(s string) []string {
	parts := []string{}
	for _, p := range splitOn(s, ',') {
		if t := trim(p); t != "" {
			parts = append(parts, t)
		}
	}
	return parts
}

func splitOn(s string, sep rune) []string {
	return splitString(s, string(sep))
}
