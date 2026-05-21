package input

import (
	"encoding/json"
	"testing"
)

func mustMarshal(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

func TestSelectFirst(t *testing.T) {
	t.Run("array of inputs returns first", func(t *testing.T) {
		inputs := []TruffleInput{
			{InstanceType: "c5.large", Region: "us-east-1"},
			{InstanceType: "m5.large", Region: "us-west-2"},
		}
		got, err := SelectFirst(mustMarshal(t, inputs))
		if err != nil {
			t.Fatalf("SelectFirst: %v", err)
		}
		if got.InstanceType != "c5.large" {
			t.Errorf("InstanceType = %q, want c5.large", got.InstanceType)
		}
	})

	t.Run("single object returned directly", func(t *testing.T) {
		input := TruffleInput{InstanceType: "t3.micro", Region: "us-east-1"}
		got, err := SelectFirst(mustMarshal(t, input))
		if err != nil {
			t.Fatalf("SelectFirst: %v", err)
		}
		if got.InstanceType != "t3.micro" {
			t.Errorf("InstanceType = %q, want t3.micro", got.InstanceType)
		}
	})

	t.Run("empty array returns error", func(t *testing.T) {
		_, err := SelectFirst(mustMarshal(t, []TruffleInput{}))
		if err == nil {
			t.Error("expected error for empty array")
		}
	})

	t.Run("malformed JSON returns error", func(t *testing.T) {
		_, err := SelectFirst([]byte(`{not valid json`))
		if err == nil {
			t.Error("expected error for malformed JSON")
		}
	})

	t.Run("preserves all fields", func(t *testing.T) {
		inputs := []TruffleInput{{
			InstanceType: "c5.xlarge",
			Region:       "eu-west-1",
			VCPUs:        4,
			MemoryMiB:    8192,
			Spot:         true,
			SpotPrice:    0.05,
		}}
		got, err := SelectFirst(mustMarshal(t, inputs))
		if err != nil {
			t.Fatalf("SelectFirst: %v", err)
		}
		if got.VCPUs != 4 || got.MemoryMiB != 8192 || !got.Spot || got.SpotPrice != 0.05 {
			t.Errorf("fields not preserved: %+v", got)
		}
	})
}

func TestSelectCheapest(t *testing.T) {
	t.Run("selects lowest spot price", func(t *testing.T) {
		inputs := []TruffleInput{
			{InstanceType: "c5.large", SpotPrice: 0.05},
			{InstanceType: "c5.xlarge", SpotPrice: 0.02},
			{InstanceType: "m5.large", SpotPrice: 0.08},
		}
		got, err := SelectCheapest(mustMarshal(t, inputs))
		if err != nil {
			t.Fatalf("SelectCheapest: %v", err)
		}
		if got.InstanceType != "c5.xlarge" {
			t.Errorf("InstanceType = %q, want c5.xlarge (lowest price)", got.InstanceType)
		}
	})

	t.Run("comparison is relative to first item baseline", func(t *testing.T) {
		// SelectCheapest uses the first item as the baseline; items are only
		// replaced when their price is both > 0 and < current cheapest.
		inputs := []TruffleInput{
			{InstanceType: "spot-1", SpotPrice: 0.10},
			{InstanceType: "spot-2", SpotPrice: 0.07},
			{InstanceType: "spot-3", SpotPrice: 0.15},
		}
		got, err := SelectCheapest(mustMarshal(t, inputs))
		if err != nil {
			t.Fatalf("SelectCheapest: %v", err)
		}
		if got.InstanceType != "spot-2" {
			t.Errorf("InstanceType = %q, want spot-2", got.InstanceType)
		}
	})

	t.Run("single item returned", func(t *testing.T) {
		inputs := []TruffleInput{{InstanceType: "t3.micro", SpotPrice: 0.01}}
		got, err := SelectCheapest(mustMarshal(t, inputs))
		if err != nil {
			t.Fatalf("SelectCheapest: %v", err)
		}
		if got.InstanceType != "t3.micro" {
			t.Errorf("InstanceType = %q, want t3.micro", got.InstanceType)
		}
	})

	t.Run("empty array returns error", func(t *testing.T) {
		_, err := SelectCheapest(mustMarshal(t, []TruffleInput{}))
		if err == nil {
			t.Error("expected error for empty array")
		}
	})

	t.Run("malformed JSON returns error", func(t *testing.T) {
		_, err := SelectCheapest([]byte(`[invalid`))
		if err == nil {
			t.Error("expected error for malformed JSON")
		}
	})
}

func TestTruffleInputJSONRoundtrip(t *testing.T) {
	original := TruffleInput{
		InstanceType:      "p4d.24xlarge",
		Region:            "us-east-1",
		AvailabilityZone:  "us-east-1a",
		AvailabilityZones: []string{"us-east-1a", "us-east-1b"},
		VCPUs:             96,
		MemoryMiB:         1152000,
		Architecture:      "x86_64",
		Spot:              true,
		SpotPrice:         3.20,
		OnDemandPrice:     32.77,
		ReservationID:     "cr-abc123",
		CapacityAvailable: 4,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got TruffleInput
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.InstanceType != original.InstanceType ||
		got.Region != original.Region ||
		got.VCPUs != original.VCPUs ||
		got.MemoryMiB != original.MemoryMiB ||
		got.SpotPrice != original.SpotPrice ||
		got.ReservationID != original.ReservationID ||
		got.CapacityAvailable != original.CapacityAvailable {
		t.Errorf("round-trip mismatch:\n  got  %+v\n  want %+v", got, original)
	}
}
