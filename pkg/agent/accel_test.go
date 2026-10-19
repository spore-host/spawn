package agent

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// fakePCI builds a /sys/bus/pci/devices with one directory per device holding
// the kernel's vendor and class files.
// Directories are named like PCI addresses ("0000:00:1e.0") but with "-" for
// ":", which Windows refuses in a file name; detectAccelerators never reads the
// name (#971).
func fakePCI(t *testing.T, devices map[string][2]string) string {
	t.Helper()
	root := t.TempDir()
	for addr, vc := range devices {
		d := filepath.Join(root, addr)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		for f, v := range map[string]string{"vendor": vc[0], "class": vc[1]} {
			if err := os.WriteFile(filepath.Join(d, f), []byte(v+"\n"), 0o444); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func fakeDevNodes(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(root, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// Every EC2 instance carries Annapurna Labs PCI devices (ENA, EBS NVMe), and AMD
// CPUs carry AMD host bridges; none of them is an accelerator. Accelerators are
// found by vendor and class, Neuron by its device nodes.
func TestDetectAccelerators_ByStructure(t *testing.T) {
	baseline := map[string][2]string{
		"0000-00-05.0": {"0x1d0f", "0x020000"}, // ENA
		"0000-00-04.0": {"0x1d0f", "0x010802"}, // EBS NVMe
		"0000-00-00.0": {"0x1022", "0x060000"}, // AMD EPYC host bridge
		"0000-00-03.0": {"0x1d0f", "0x030000"}, // Nitro VGA (Annapurna)
	}
	with := func(extra map[string][2]string) map[string][2]string {
		m := map[string][2]string{}
		for k, v := range baseline {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for _, tc := range []struct {
		name string
		pci  map[string][2]string
		dev  []string
		want []string
	}{
		{"plain instance", baseline, []string{"nvme0", "nvme0n1", "null"}, []string{}},
		{"g4dn: NVIDIA T4 (3D controller)", with(map[string][2]string{"0000-00-1e.0": {"0x10de", "0x030200"}}), nil, []string{"nvidia"}},
		{"g4ad: AMD Radeon Pro V520", with(map[string][2]string{"0000-00-1e.0": {"0x1002", "0x030000"}}), nil, []string{"amd"}},
		{"dl1: Gaudi", with(map[string][2]string{"0000-10-1d.0": {"0x1da3", "0x120000"}}), nil, []string{"gaudi"}},
		{"trn1: Neuron device nodes", baseline, []string{"neuron0", "neuron1"}, []string{"neuron"}},
		{"neuron-like names that are not devices", baseline, []string{"neuron", "neuronx"}, []string{}},
		{"NVIDIA audio function is not a GPU", with(map[string][2]string{"0000-00-1f.1": {"0x10de", "0x040300"}}), nil, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := detectAccelerators(fakePCI(t, tc.pci), fakeDevNodes(t, tc.dev...))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("detectAccelerators = %v, want %v", got, tc.want)
			}
		})
	}
}

// Only numbers can read as idle. [N/A] and [Not Supported] used to be skipped,
// so a GPU that could not be read reported 0% (#954).
func TestParsePercentLines(t *testing.T) {
	for _, tc := range []struct {
		name         string
		out          string
		wantMeasured bool
		wantPct      float64
	}{
		{"one GPU busy", "50\n", true, 50},
		{"max across GPUs", "25\n75\n", true, 75},
		{"idle GPUs", "0\n3\n", true, 3},
		{"hl-smi with a percent sign", "0 %\n2 %\n", true, 2},
		{"every GPU [N/A]", "[N/A]\n[N/A]\n", false, 100},
		{"[Not Supported]", "[Not Supported]\n", false, 100},
		{"one idle, one unreadable", "0\n[N/A]\n", false, 100},
		{"one busy, one unreadable: busy is known", "80\n[N/A]\n", true, 80},
		{"empty output", "", false, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := parsePercentLines("nvidia-smi", []byte(tc.out))
			if r.measured != tc.wantMeasured || r.pct != tc.wantPct {
				t.Errorf("= (%v, measured=%v, %q), want (%v, measured=%v)", r.pct, r.measured, r.why, tc.wantPct, tc.wantMeasured)
			}
			if !r.measured && r.why == "" {
				t.Error("an unknown reading carries no reason")
			}
		})
	}
}

func TestParseRocmSmiJSON(t *testing.T) {
	r := parseRocmSmiJSON([]byte(`{"card0": {"GPU use (%)": "12"}, "card1": {"GPU use (%)": "1"}}`))
	if !r.measured || r.pct != 12 {
		t.Errorf("= %+v, want measured 12", r)
	}
	r = parseRocmSmiJSON([]byte(`{"card0": {"GPU use (%)": "0"}}`))
	if !r.measured || r.pct != 0 {
		t.Errorf("idle card = %+v, want measured 0", r)
	}
	for _, bad := range []string{`{"card0": {"Temperature": "40"}}`, `{}`, `not json`} {
		if r := parseRocmSmiJSON([]byte(bad)); r.measured {
			t.Errorf("%s read as measured %+v", bad, r)
		}
	}
}

func TestParseNeuronMonitor(t *testing.T) {
	busy := `{"neuron_runtime_data":[{"pid":4242,"error":"","report":{"neuroncore_counters":{"period":5.0,"error":"",` +
		`"neuroncores_in_use":{"0":{"neuroncore_utilization":42.5},"1":{"neuroncore_utilization":3.1}}}}}],` +
		`"system_data":{},"neuron_hardware_info":{"neuron_device_count":1}}`
	if r := parseNeuronMonitor([]byte(busy)); !r.measured || r.pct != 42.5 {
		t.Errorf("busy NeuronCore = %+v, want measured 42.5", r)
	}
	idle := `{"neuron_runtime_data":[],"system_data":{}}`
	if r := parseNeuronMonitor([]byte(idle)); !r.measured || r.pct != 0 {
		t.Errorf("no Neuron runtime = %+v, want measured 0", r)
	}
	for name, bad := range map[string]string{
		"no runtime list":   `{"system_data":{}}`,
		"runtime error":     `{"neuron_runtime_data":[{"error":"permission denied","report":{}}]}`,
		"no counters":       `{"neuron_runtime_data":[{"error":"","report":{}}]}`,
		"no utilization":    `{"neuron_runtime_data":[{"error":"","report":{"neuroncore_counters":{"neuroncores_in_use":{"0":{}}}}}]}`,
		"not json":          `Error: neuron driver not loaded`,
		"counters in error": `{"neuron_runtime_data":[{"error":"","report":{"neuroncore_counters":{"error":"x","neuroncores_in_use":{}}}}]}`,
	} {
		if r := parseNeuronMonitor([]byte(bad)); r.measured {
			t.Errorf("%s read as measured %+v", name, r)
		}
	}
}

func stubAccelerators(t *testing.T, families []string, tool func(name string) ([]byte, error)) *[]string {
	t.Helper()
	origF, origT := acceleratorFamilies, runAccelTool
	t.Cleanup(func() { acceleratorFamilies, runAccelTool = origF, origT })
	acceleratorFamilies = func() []string { return families }
	var calls []string
	runAccelTool = func(_ context.Context, _ bool, name string, _ ...string) ([]byte, error) {
		calls = append(calls, name)
		return tool(name)
	}
	return &calls
}

// THE bug. nvidia-smi present but failing returned 0, a measured "idle".
func TestGPUUtilization_FailingToolIsUnknownAndWarnedOnce(t *testing.T) {
	stubAccelerators(t, []string{accelNVIDIA}, func(string) ([]byte, error) {
		return []byte("NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver.\n"),
			errors.New("exit status 9")
	})
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	a := newTestAgent(t, nil)
	for i := 0; i < 3; i++ {
		pct, measured, why := a.gpuUtilization(context.Background())
		if measured || pct <= idleGPUPercent || why == "" {
			t.Fatalf("call %d = (%v, measured=%v, %q), want unknown and assumed active", i+1, pct, measured, why)
		}
	}
	if n := strings.Count(buf.String(), "WARNING"); n != 1 {
		t.Errorf("warned %d times over 3 calls, want once:\n%s", n, buf.String())
	}
}

// A device whose tool is not installed cannot be measured; it is not idle.
func TestGPUUtilization_DeviceWithoutToolIsUnknown(t *testing.T) {
	for _, f := range []string{accelNVIDIA, accelAMD, accelGaudi, accelNeuron} {
		stubAccelerators(t, []string{f}, func(name string) ([]byte, error) {
			return nil, &exec.Error{Name: name, Err: exec.ErrNotFound}
		})
		a := newTestAgent(t, nil)
		if _, measured, why := a.gpuUtilization(context.Background()); measured || !strings.Contains(why, "not installed") {
			t.Errorf("%s with no tool = (measured=%v, %q), want unknown, tool not installed", f, measured, why)
		}
	}
}

func TestGPUUtilization_Combinations(t *testing.T) {
	t.Run("no accelerator is a measured 0, and runs no tool", func(t *testing.T) {
		calls := stubAccelerators(t, nil, func(string) ([]byte, error) { t.Fatal("tool run"); return nil, nil })
		a := newTestAgent(t, nil)
		if pct, measured, _ := a.gpuUtilization(context.Background()); !measured || pct != 0 || len(*calls) != 0 {
			t.Errorf("= (%v, %v), calls %v", pct, measured, *calls)
		}
	})
	t.Run("a busy device decides, even beside an unreadable one", func(t *testing.T) {
		stubAccelerators(t, []string{accelNVIDIA, accelNeuron}, func(name string) ([]byte, error) {
			if name == "nvidia-smi" {
				return nil, errors.New("exit status 6")
			}
			return []byte(`{"neuron_runtime_data":[{"error":"","report":{"neuroncore_counters":{"neuroncores_in_use":{"0":{"neuroncore_utilization":90}}}}}]}` + "\n"), nil
		})
		a := newTestAgent(t, nil)
		if pct, measured, _ := a.gpuUtilization(context.Background()); !measured || pct != 90 {
			t.Errorf("= (%v, %v), want measured 90", pct, measured)
		}
	})
	t.Run("idle devices are a measured idle", func(t *testing.T) {
		stubAccelerators(t, []string{accelGaudi, accelNVIDIA}, func(name string) ([]byte, error) {
			return []byte("0\n1\n"), nil
		})
		a := newTestAgent(t, nil)
		if pct, measured, _ := a.gpuUtilization(context.Background()); !measured || pct != 1 {
			t.Errorf("= (%v, %v), want measured 1", pct, measured)
		}
	})
}

// Through the idle decision: an unmeasurable accelerator keeps the instance
// active, whatever the host counters say.
func TestIdleDecision_UnmeasurableAcceleratorIsNotIdle(t *testing.T) {
	noSessions(t)
	c := installCounters(t)
	stubAccelerators(t, []string{accelNVIDIA}, func(string) ([]byte, error) {
		return []byte("No devices were found\n"), errors.New("exit status 6")
	})
	a := newTestAgent(t, nil)
	a.evaluateIdle(context.Background())
	c.idleCPUMinute()
	s := a.evaluateIdle(context.Background())
	if s.idle {
		t.Fatalf("idle with nvidia-smi failing (%s); a failing tool must not read as 0%% (#954)", s.reason)
	}
	if !strings.Contains(s.reason, "accelerator utilization could not be measured") {
		t.Errorf("reason = %q", s.reason)
	}
}

// neuron-monitor streams a report per period and never exits; the runner must
// return the first report and kill it, not wait for it.
func TestRunAccelTool_FirstLineOfAStreamingTool(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only: runs a #!/bin/sh fake neuron-monitor from PATH, which Windows does not execute (#1002)")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\necho '{\"neuron_runtime_data\":[]}'\nwhile true; do sleep 1; done\n"
	if err := os.WriteFile(filepath.Join(dir, "fake-neuron-monitor"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := runAccelTool(context.Background(), true, "fake-neuron-monitor")
	if err != nil {
		t.Fatal(err)
	}
	if r := parseNeuronMonitor(out); !r.measured || r.pct != 0 {
		t.Errorf("first report = %q -> %+v", out, r)
	}
}
