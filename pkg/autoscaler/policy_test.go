package autoscaler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// Mock SQS client for testing
type mockSQSClient struct {
	queueDepth int
	err        error
}

func (m *mockSQSClient) GetQueueAttributes(ctx context.Context, params *sqs.GetQueueAttributesInput, optFns ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	if m.err != nil {
		return nil, m.err
	}

	// Split queue depth between visible and in-flight (arbitrary split for testing)
	visible := m.queueDepth / 2
	inFlight := m.queueDepth - visible

	return &sqs.GetQueueAttributesOutput{
		Attributes: map[string]string{
			string(types.QueueAttributeNameApproximateNumberOfMessages):           fmt.Sprintf("%d", visible),
			string(types.QueueAttributeNameApproximateNumberOfMessagesNotVisible): fmt.Sprintf("%d", inFlight),
		},
	}, nil
}

func TestCalculateDesiredCapacity(t *testing.T) {
	tests := []struct {
		name       string
		queueDepth int
		target     int
		want       int
	}{
		{"empty queue", 0, 10, 0},
		{"partial load", 50, 10, 5},
		{"exact multiple", 100, 10, 10},
		{"ceiling division", 105, 10, 11},
		{"single message", 1, 10, 1},
		{"target is zero", 50, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pe := &PolicyEvaluator{}
			got := pe.calculateDesiredCapacity(tt.queueDepth, tt.target)

			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEvaluatePolicy_MinMaxBounds(t *testing.T) {
	tests := []struct {
		name        string
		queueDepth  int
		target      int
		min         int
		max         int
		current     int
		wantDesired int
		wantChanged bool
	}{
		{
			name:        "clamped to min",
			queueDepth:  5,
			target:      10,
			min:         2,
			max:         20,
			current:     2,
			wantDesired: 2,
			wantChanged: false,
		},
		{
			name:        "clamped to max",
			queueDepth:  300,
			target:      10,
			min:         0,
			max:         15,
			current:     10,
			wantDesired: 15,
			wantChanged: true,
		},
		{
			name:        "within bounds",
			queueDepth:  50,
			target:      10,
			min:         0,
			max:         20,
			current:     0,
			wantDesired: 5,
			wantChanged: true,
		},
		{
			name:        "no change needed",
			queueDepth:  50,
			target:      10,
			min:         0,
			max:         20,
			current:     5,
			wantDesired: 5,
			wantChanged: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Mock SQS client
			mockSQS := &mockSQSClient{queueDepth: tt.queueDepth}
			pe := NewPolicyEvaluator(mockSQS)

			group := &AutoScaleGroup{
				DesiredCapacity: tt.current,
				MinCapacity:     tt.min,
				MaxCapacity:     tt.max,
				ScalingPolicy: &ScalingPolicy{
					PolicyType:                "queue-depth",
					QueueURL:                  "https://sqs.us-east-1.amazonaws.com/test/queue",
					TargetMessagesPerInstance: tt.target,
					ScaleUpCooldownSeconds:    60,
					ScaleDownCooldownSeconds:  300,
				},
			}

			gotDesired, _, gotChanged, err := pe.EvaluatePolicy(context.Background(), group)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if gotDesired != tt.wantDesired {
				t.Errorf("desired capacity: got %d, want %d", gotDesired, tt.wantDesired)
			}

			if gotChanged != tt.wantChanged {
				t.Errorf("changed: got %v, want %v", gotChanged, tt.wantChanged)
			}
		})
	}
}

func TestInCooldown(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name    string
		state   *ScalingState
		policy  *ScalingPolicy
		scaleUp bool
		want    bool
	}{
		{
			name:    "no state - not in cooldown",
			state:   nil,
			policy:  &ScalingPolicy{ScaleUpCooldownSeconds: 60, ScaleDownCooldownSeconds: 300},
			scaleUp: true,
			want:    false,
		},
		{
			name: "scale up - in cooldown",
			state: &ScalingState{
				LastScaleUp: now.Add(-30 * time.Second),
			},
			policy:  &ScalingPolicy{ScaleUpCooldownSeconds: 60, ScaleDownCooldownSeconds: 300},
			scaleUp: true,
			want:    true,
		},
		{
			name: "scale up - cooldown expired",
			state: &ScalingState{
				LastScaleUp: now.Add(-90 * time.Second),
			},
			policy:  &ScalingPolicy{ScaleUpCooldownSeconds: 60, ScaleDownCooldownSeconds: 300},
			scaleUp: true,
			want:    false,
		},
		{
			name: "scale down - in cooldown",
			state: &ScalingState{
				LastScaleDown: now.Add(-2 * time.Minute),
			},
			policy:  &ScalingPolicy{ScaleUpCooldownSeconds: 60, ScaleDownCooldownSeconds: 300},
			scaleUp: false,
			want:    true,
		},
		{
			name: "scale down - cooldown expired",
			state: &ScalingState{
				LastScaleDown: now.Add(-6 * time.Minute),
			},
			policy:  &ScalingPolicy{ScaleUpCooldownSeconds: 60, ScaleDownCooldownSeconds: 300},
			scaleUp: false,
			want:    false,
		},
		{
			name: "scale up during scale down cooldown - allowed",
			state: &ScalingState{
				LastScaleDown: now.Add(-1 * time.Minute),
			},
			policy:  &ScalingPolicy{ScaleUpCooldownSeconds: 60, ScaleDownCooldownSeconds: 300},
			scaleUp: true,
			want:    false,
		},
		{
			name: "no previous scale event",
			state: &ScalingState{
				LastQueueDepth: 50,
			},
			policy:  &ScalingPolicy{ScaleUpCooldownSeconds: 60, ScaleDownCooldownSeconds: 300},
			scaleUp: true,
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pe := &PolicyEvaluator{}
			got := pe.inCooldown(tt.state, tt.policy, tt.scaleUp)

			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvaluatePolicy_NilPolicy(t *testing.T) {
	pe := &PolicyEvaluator{}

	group := &AutoScaleGroup{
		DesiredCapacity: 5,
		ScalingPolicy:   nil,
	}

	gotDesired, gotQueueDepth, gotChanged, err := pe.EvaluatePolicy(context.Background(), group)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotDesired != 5 {
		t.Errorf("desired capacity: got %d, want 5", gotDesired)
	}

	if gotQueueDepth != 0 {
		t.Errorf("queue depth: got %d, want 0", gotQueueDepth)
	}

	if gotChanged {
		t.Errorf("changed: got true, want false")
	}
}

func TestParseIntAttr(t *testing.T) {
	tests := []struct {
		name  string
		attrs map[string]string
		key   string
		want  int
	}{
		{"valid integer", map[string]string{"foo": "42"}, "foo", 42},
		{"missing key", map[string]string{"foo": "42"}, "bar", 0},
		{"invalid integer", map[string]string{"foo": "abc"}, "foo", 0},
		{"empty string", map[string]string{"foo": ""}, "foo", 0},
		{"zero", map[string]string{"foo": "0"}, "foo", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseIntAttr(tt.attrs, tt.key)

			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestNormalizeQueues(t *testing.T) {
	pe := &PolicyEvaluator{}

	tests := []struct {
		name   string
		policy *ScalingPolicy
		want   []QueueConfig
	}{
		{
			name: "multi-queue with weights",
			policy: &ScalingPolicy{
				Queues: []QueueConfig{
					{QueueURL: "https://sqs/queue1", Weight: 0.6},
					{QueueURL: "https://sqs/queue2", Weight: 0.4},
				},
			},
			want: []QueueConfig{
				{QueueURL: "https://sqs/queue1", Weight: 0.6},
				{QueueURL: "https://sqs/queue2", Weight: 0.4},
			},
		},
		{
			name: "multi-queue with default weights",
			policy: &ScalingPolicy{
				Queues: []QueueConfig{
					{QueueURL: "https://sqs/queue1"},
					{QueueURL: "https://sqs/queue2"},
				},
			},
			want: []QueueConfig{
				{QueueURL: "https://sqs/queue1", Weight: 1.0},
				{QueueURL: "https://sqs/queue2", Weight: 1.0},
			},
		},
		{
			name: "backward compat - single queue",
			policy: &ScalingPolicy{
				QueueURL: "https://sqs/queue",
			},
			want: []QueueConfig{
				{QueueURL: "https://sqs/queue", Weight: 1.0},
			},
		},
		{
			name:   "no queues configured",
			policy: &ScalingPolicy{},
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pe.normalizeQueues(tt.policy)

			if len(got) != len(tt.want) {
				t.Errorf("got %d queues, want %d", len(got), len(tt.want))
				return
			}

			for i := range got {
				if got[i].QueueURL != tt.want[i].QueueURL {
					t.Errorf("queue %d: got URL %s, want %s", i, got[i].QueueURL, tt.want[i].QueueURL)
				}
				if got[i].Weight != tt.want[i].Weight {
					t.Errorf("queue %d: got weight %.2f, want %.2f", i, got[i].Weight, tt.want[i].Weight)
				}
			}
		})
	}
}

func TestGetWeightedQueueDepth(t *testing.T) {
	tests := []struct {
		name      string
		queues    []QueueConfig
		mockDepth int
		want      int
		wantErr   bool
	}{
		{
			name: "single queue",
			queues: []QueueConfig{
				{QueueURL: "https://sqs/queue1", Weight: 1.0},
			},
			mockDepth: 100,
			want:      100,
		},
		{
			name: "equal weights",
			queues: []QueueConfig{
				{QueueURL: "https://sqs/queue1", Weight: 1.0},
				{QueueURL: "https://sqs/queue2", Weight: 1.0},
			},
			mockDepth: 50,
			want:      100, // 50*1.0 + 50*1.0
		},
		{
			name: "weighted 60/40",
			queues: []QueueConfig{
				{QueueURL: "https://sqs/queue1", Weight: 0.6},
				{QueueURL: "https://sqs/queue2", Weight: 0.4},
			},
			mockDepth: 100,
			want:      100, // 100*0.6 + 100*0.4
		},
		{
			name: "priority queue (80/20)",
			queues: []QueueConfig{
				{QueueURL: "https://sqs/high", Weight: 0.8},
				{QueueURL: "https://sqs/low", Weight: 0.2},
			},
			mockDepth: 100,
			want:      100, // 100*0.8 + 100*0.2
		},
		{
			name: "three queues",
			queues: []QueueConfig{
				{QueueURL: "https://sqs/q1", Weight: 0.5},
				{QueueURL: "https://sqs/q2", Weight: 0.3},
				{QueueURL: "https://sqs/q3", Weight: 0.2},
			},
			mockDepth: 100,
			want:      100, // 100*0.5 + 100*0.3 + 100*0.2
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockSQS := &mockSQSClient{queueDepth: tt.mockDepth}
			pe := NewPolicyEvaluator(mockSQS)

			got, err := pe.getWeightedQueueDepth(context.Background(), tt.queues)
			if (err != nil) != tt.wantErr {
				t.Errorf("got error %v, wantErr %v", err, tt.wantErr)
				return
			}

			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEvaluatePolicy_MultiQueue(t *testing.T) {
	tests := []struct {
		name           string
		queues         []QueueConfig
		queueDepth     int
		target         int
		currentDesired int
		min            int
		max            int
		wantDesired    int
		wantChanged    bool
	}{
		{
			name: "two queues equal weights",
			queues: []QueueConfig{
				{QueueURL: "https://sqs/q1", Weight: 1.0},
				{QueueURL: "https://sqs/q2", Weight: 1.0},
			},
			queueDepth:     50, // Each queue has 50, total = 100
			target:         10, // 100 / 10 = 10 instances
			currentDesired: 5,
			min:            0,
			max:            20,
			wantDesired:    10,
			wantChanged:    true,
		},
		{
			name: "priority queue 80/20",
			queues: []QueueConfig{
				{QueueURL: "https://sqs/high", Weight: 0.8},
				{QueueURL: "https://sqs/low", Weight: 0.2},
			},
			queueDepth:     100, // Weighted: 100*0.8 + 100*0.2 = 100
			target:         10,
			currentDesired: 5,
			min:            0,
			max:            20,
			wantDesired:    10,
			wantChanged:    true,
		},
		{
			name: "backward compat single queue",
			queues: []QueueConfig{
				{QueueURL: "https://sqs/queue", Weight: 1.0},
			},
			queueDepth:     50,
			target:         10,
			currentDesired: 3,
			min:            0,
			max:            20,
			wantDesired:    5,
			wantChanged:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockSQS := &mockSQSClient{queueDepth: tt.queueDepth}
			pe := NewPolicyEvaluator(mockSQS)

			group := &AutoScaleGroup{
				DesiredCapacity: tt.currentDesired,
				MinCapacity:     tt.min,
				MaxCapacity:     tt.max,
				ScalingPolicy: &ScalingPolicy{
					PolicyType:                "queue-depth",
					Queues:                    tt.queues,
					TargetMessagesPerInstance: tt.target,
					ScaleUpCooldownSeconds:    60,
					ScaleDownCooldownSeconds:  300,
				},
			}

			gotDesired, _, gotChanged, err := pe.EvaluatePolicy(context.Background(), group)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if gotDesired != tt.wantDesired {
				t.Errorf("got desired %d, want %d", gotDesired, tt.wantDesired)
			}
			if gotChanged != tt.wantChanged {
				t.Errorf("got changed %v, want %v", gotChanged, tt.wantChanged)
			}
		})
	}
}
