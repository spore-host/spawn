package cmd

import (
	"reflect"
	"testing"
)

func TestMemberByIndex(t *testing.T) {
	members := []arrayMember{
		{Index: 0, InstanceID: "i-0"},
		{Index: 2, InstanceID: "i-2"},
		{Index: 5, InstanceID: "i-5"},
	}
	cases := []struct {
		name   string
		index  int
		wantOK bool
		wantID string
	}{
		{"first", 0, true, "i-0"},
		{"middle sparse", 2, true, "i-2"},
		{"last", 5, true, "i-5"},
		{"gap", 1, false, ""},
		{"beyond", 6, false, ""},
		{"negative", -1, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := memberByIndex(members, tc.index)
			if ok != tc.wantOK {
				t.Fatalf("memberByIndex(%d) ok = %v, want %v", tc.index, ok, tc.wantOK)
			}
			if ok && got.InstanceID != tc.wantID {
				t.Errorf("memberByIndex(%d) = %q, want %q", tc.index, got.InstanceID, tc.wantID)
			}
		})
	}
}

func TestArrayLogPath(t *testing.T) {
	cases := []struct {
		which   string
		want    string
		wantErr bool
	}{
		{"command", commandLogRemotePath, false},
		{"", commandLogRemotePath, false}, // default
		{"spored", sporedLogRemotePath, false},
		{"nonsense", "", true},
	}
	for _, tc := range cases {
		got, err := arrayLogPath(tc.which)
		if tc.wantErr {
			if err == nil {
				t.Errorf("arrayLogPath(%q): expected error, got path %q", tc.which, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("arrayLogPath(%q): unexpected error %v", tc.which, err)
		}
		if got != tc.want {
			t.Errorf("arrayLogPath(%q) = %q, want %q", tc.which, got, tc.want)
		}
	}
}

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
