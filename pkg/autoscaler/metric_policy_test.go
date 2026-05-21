package autoscaler

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// Mock CloudWatch client for testing
type mockCloudWatchClient struct {
	metricValue float64
	err         error
}

func (m *mockCloudWatchClient) GetMetricStatistics(ctx context.Context, params *cloudwatch.GetMetricStatisticsInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricStatisticsOutput, error) {
	if m.err != nil {
		return nil, m.err
	}

	return &cloudwatch.GetMetricStatisticsOutput{
		Datapoints: []types.Datapoint{
			{
				Timestamp: aws.Time(time.Now()),
				Average:   aws.Float64(m.metricValue),
				Maximum:   aws.Float64(m.metricValue),
				Minimum:   aws.Float64(m.metricValue),
			},
		},
	}, nil
}

func TestCalculateCapacityFromMetric(t *testing.T) {
	tests := []struct {
		name            string
		currentMetric   float64
		targetMetric    float64
		currentCapacity int
		want            int
	}{
		{
			name:            "at target",
			currentMetric:   70.0,
			targetMetric:    70.0,
			currentCapacity: 5,
			want:            5,
		},
		{
			name:            "above target - scale up",
			currentMetric:   80.0,
			targetMetric:    70.0,
			currentCapacity: 5,
			want:            6,
		},
		{
			name:            "below target - scale down",
			currentMetric:   50.0,
			targetMetric:    70.0,
			currentCapacity: 5,
			want:            4,
		},
		{
			name:            "significantly above target",
			currentMetric:   90.0,
			targetMetric:    70.0,
			currentCapacity: 10,
			want:            13,
		},
		{
			name:            "low utilization",
			currentMetric:   20.0,
			targetMetric:    70.0,
			currentCapacity: 10,
			want:            3,
		},
		{
			name:            "zero target",
			currentMetric:   80.0,
			targetMetric:    0,
			currentCapacity: 5,
			want:            5,
		},
		{
			name:            "zero capacity",
			currentMetric:   80.0,
			targetMetric:    70.0,
			currentCapacity: 0,
			want:            0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			me := &MetricEvaluator{}
			got := me.calculateCapacityFromMetric(tt.currentMetric, tt.targetMetric, tt.currentCapacity)

			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEvaluateMetricPolicy_MinMaxBounds(t *testing.T) {
	tests := []struct {
		name            string
		metricValue     float64
		targetValue     float64
		currentCapacity int
		min             int
		max             int
		wantCapacity    int
		wantChanged     bool
	}{
		{
			name:            "clamped to min",
			metricValue:     20.0,
			targetValue:     70.0,
			currentCapacity: 5,
			min:             3,
			max:             20,
			wantCapacity:    3,
			wantChanged:     true,
		},
		{
			name:            "clamped to max",
			metricValue:     95.0,
			targetValue:     70.0,
			currentCapacity: 10,
			min:             0,
			max:             12,
			wantCapacity:    12,
			wantChanged:     true,
		},
		{
			name:            "within bounds",
			metricValue:     80.0,
			targetValue:     70.0,
			currentCapacity: 5,
			min:             0,
			max:             20,
			wantCapacity:    6,
			wantChanged:     true,
		},
		{
			name:            "no change needed",
			metricValue:     70.0,
			targetValue:     70.0,
			currentCapacity: 5,
			min:             0,
			max:             20,
			wantCapacity:    5,
			wantChanged:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockCW := &mockCloudWatchClient{metricValue: tt.metricValue}
			me := NewMetricEvaluator(mockCW)

			group := &AutoScaleGroup{
				DesiredCapacity: tt.currentCapacity,
				MinCapacity:     tt.min,
				MaxCapacity:     tt.max,
				MetricPolicy: &MetricScalingPolicy{
					MetricType:    "cpu",
					MetricName:    "CPUUtilization",
					Namespace:     "AWS/EC2",
					Statistic:     "Average",
					TargetValue:   tt.targetValue,
					PeriodSeconds: 300,
				},
			}

			gotCapacity, gotValue, gotChanged, err := me.EvaluateMetricPolicy(context.Background(), group, []string{"i-test"})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if gotCapacity != tt.wantCapacity {
				t.Errorf("capacity: got %d, want %d", gotCapacity, tt.wantCapacity)
			}

			if gotValue != tt.metricValue {
				t.Errorf("metric value: got %f, want %f", gotValue, tt.metricValue)
			}

			if gotChanged != tt.wantChanged {
				t.Errorf("changed: got %v, want %v", gotChanged, tt.wantChanged)
			}
		})
	}
}

func TestEvaluateMetricPolicy_NilPolicy(t *testing.T) {
	me := &MetricEvaluator{}

	group := &AutoScaleGroup{
		DesiredCapacity: 5,
		MetricPolicy:    nil,
	}

	gotCapacity, gotValue, gotChanged, err := me.EvaluateMetricPolicy(context.Background(), group, []string{"i-test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotCapacity != 5 {
		t.Errorf("capacity: got %d, want 5", gotCapacity)
	}

	if gotValue != 0 {
		t.Errorf("metric value: got %f, want 0", gotValue)
	}

	if gotChanged {
		t.Errorf("changed: got true, want false")
	}
}

func TestEvaluateMetricPolicy_NoInstances(t *testing.T) {
	mockCW := &mockCloudWatchClient{metricValue: 80.0}
	me := NewMetricEvaluator(mockCW)

	group := &AutoScaleGroup{
		DesiredCapacity: 5,
		MinCapacity:     0,
		MaxCapacity:     10,
		MetricPolicy: &MetricScalingPolicy{
			MetricType:    "cpu",
			MetricName:    "CPUUtilization",
			Namespace:     "AWS/EC2",
			Statistic:     "Average",
			TargetValue:   70.0,
			PeriodSeconds: 300,
		},
	}

	// Empty instance list should return 0 metric value
	gotCapacity, gotValue, gotChanged, err := me.EvaluateMetricPolicy(context.Background(), group, []string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotValue != 0 {
		t.Errorf("metric value: got %f, want 0", gotValue)
	}

	if gotChanged {
		t.Errorf("changed: got true, want false")
	}

	// When there are no instances, capacity should stay at current
	if gotCapacity != group.DesiredCapacity {
		t.Errorf("capacity: got %d, want %d", gotCapacity, group.DesiredCapacity)
	}
}

func TestGetMetricPolicyDefaults(t *testing.T) {
	tests := []struct {
		name           string
		metricType     string
		wantMetricName string
		wantNamespace  string
		wantTarget     float64
	}{
		{
			name:           "cpu default",
			metricType:     "cpu",
			wantMetricName: "CPUUtilization",
			wantNamespace:  "AWS/EC2",
			wantTarget:     70.0,
		},
		{
			name:           "memory default",
			metricType:     "memory",
			wantMetricName: "mem_used_percent",
			wantNamespace:  "CWAgent",
			wantTarget:     80.0,
		},
		{
			name:       "unknown type",
			metricType: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := GetMetricPolicyDefaults(tt.metricType)

			if tt.wantMetricName == "" {
				if policy != nil {
					t.Errorf("expected nil policy for unknown type, got %+v", policy)
				}
				return
			}

			if policy == nil {
				t.Fatalf("expected non-nil policy")
			}

			if policy.MetricName != tt.wantMetricName {
				t.Errorf("metric name: got %s, want %s", policy.MetricName, tt.wantMetricName)
			}

			if policy.Namespace != tt.wantNamespace {
				t.Errorf("namespace: got %s, want %s", policy.Namespace, tt.wantNamespace)
			}

			if policy.TargetValue != tt.wantTarget {
				t.Errorf("target: got %f, want %f", policy.TargetValue, tt.wantTarget)
			}
		})
	}
}
