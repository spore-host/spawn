package input

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// TruffleInput represents data from truffle
type TruffleInput struct {
	InstanceType      string   `json:"instance_type"`
	Region            string   `json:"region"`
	AvailabilityZone  string   `json:"availability_zone"`
	AvailabilityZones []string `json:"availability_zones"`
	VCPUs             int      `json:"vcpus"`
	MemoryMiB         int64    `json:"memory_mib"`
	Architecture      string   `json:"architecture"`

	// From truffle spot
	Spot          bool    `json:"spot,omitempty"`
	SpotPrice     float64 `json:"spot_price,omitempty"`
	OnDemandPrice float64 `json:"on_demand_price,omitempty"`

	// From truffle capacity
	ReservationID     string `json:"reservation_id,omitempty"`
	CapacityAvailable int32  `json:"available_capacity,omitempty"`
}

// ParseFromStdin reads JSON from stdin
func ParseFromStdin() (*TruffleInput, error) {
	// Check if stdin has data
	stat, err := os.Stdin.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat stdin: %w", err)
	}

	if (stat.Mode() & os.ModeCharDevice) != 0 {
		// No pipe, stdin is a terminal
		return nil, fmt.Errorf("no input from pipe (use: truffle ... | spawn)")
	}

	// Read all from stdin
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, fmt.Errorf("failed to read stdin: %w", err)
	}

	// Parse JSON
	// Could be array or single object
	var input TruffleInput

	// Try parsing as single object first
	err = json.Unmarshal(data, &input)
	if err == nil {
		return &input, nil
	}

	// Try parsing as array
	var inputs []TruffleInput
	err = json.Unmarshal(data, &inputs)
	if err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	if len(inputs) == 0 {
		return nil, fmt.Errorf("empty input array")
	}

	// Return first element
	return &inputs[0], nil
}

// SelectFirst picks the first item from an array
func SelectFirst(data []byte) (*TruffleInput, error) {
	var inputs []TruffleInput

	err := json.Unmarshal(data, &inputs)
	if err != nil {
		// Maybe it's a single object
		var input TruffleInput
		err = json.Unmarshal(data, &input)
		if err != nil {
			return nil, err
		}
		return &input, nil
	}

	if len(inputs) == 0 {
		return nil, fmt.Errorf("empty array")
	}

	return &inputs[0], nil
}

// SelectCheapest picks the cheapest Spot price
func SelectCheapest(data []byte) (*TruffleInput, error) {
	var inputs []TruffleInput

	err := json.Unmarshal(data, &inputs)
	if err != nil {
		return nil, err
	}

	if len(inputs) == 0 {
		return nil, fmt.Errorf("empty array")
	}

	// Find cheapest
	cheapest := &inputs[0]
	for i := range inputs {
		if inputs[i].SpotPrice > 0 && inputs[i].SpotPrice < cheapest.SpotPrice {
			cheapest = &inputs[i]
		}
	}

	return cheapest, nil
}
