package cmd

import (
	"reflect"
	"testing"
)

func TestMissingIndexes(t *testing.T) {
	cases := []struct {
		name    string
		present []int
		size    int
		want    []int
	}{
		{"full", []int{0, 1, 2, 3}, 4, nil},
		{"sparse", []int{0, 1, 2, 4, 5, 7}, 8, []int{3, 6}},
		{"none launched", []int{}, 3, []int{0, 1, 2}},
		{"one missing at end", []int{0, 1}, 3, []int{2}},
		{"size zero", []int{}, 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			members := make([]arrayMember, 0, len(tc.present))
			for _, i := range tc.present {
				members = append(members, arrayMember{Index: i})
			}
			got := missingIndexes(members, tc.size)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("missingIndexes(%v, %d) = %v, want %v", tc.present, tc.size, got, tc.want)
			}
		})
	}
}
