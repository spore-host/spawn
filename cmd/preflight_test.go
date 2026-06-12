package cmd

import "testing"

func TestInstanceFamilyHint(t *testing.T) {
	cases := map[string]string{
		"c5n.18xlarge":   "c5n*",
		"hpc6a.48xlarge": "hpc6a*",
		"m7i.xlarge":     "m7i*",
		"weird":          "weird", // no dot
	}
	for in, want := range cases {
		if got := instanceFamilyHint(in); got != want {
			t.Errorf("instanceFamilyHint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsHPCInstanceType(t *testing.T) {
	hpc := []string{"hpc6a.48xlarge", "hpc7a.96xlarge", "hpc7g.16xlarge"}
	notHPC := []string{"c5n.18xlarge", "m7i.xlarge", "hpc.weird", "c6i.32xlarge"}
	for _, it := range hpc {
		if !isHPCInstanceType(it) {
			t.Errorf("%s should be HPC", it)
		}
	}
	for _, it := range notHPC {
		if isHPCInstanceType(it) {
			t.Errorf("%s should NOT be HPC", it)
		}
	}
}
