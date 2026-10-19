package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Accelerator utilisation for the idle check (#954).
//
// The signal follows the counters' rules (cpuUsage #734, networkBytes #919,
// diskIOBytes #937): a reading is (value, measured, why), "could not tell" is
// treated as active and warned about once, and only a successful parse of real
// numbers can read as idle. Before #954 nvidia-smi failing (a driver/kernel
// mismatch after an update, "No devices were found", a GPU fallen off the bus)
// and nvidia-smi printing only [N/A] all returned 0, a measured "GPU idle", and
// Trainium/Inferentia, Gaudi and AMD GPUs were never looked at, so a job
// running on the device with little host CPU could be idle-stopped.
//
// Which accelerators exist is decided by structure, not by which tools are on
// PATH: a Deep Learning AMI ships nvidia-smi on instances with no GPU, where it
// can only fail, and keying on the tool would make idle-stop impossible there.
// This file is not build-tagged, so the parsing is tested on every platform.

// idleGPUPercent is the utilisation above which an accelerator counts as busy.
const idleGPUPercent = 5.0

// Accelerator families.
const (
	accelNVIDIA = "nvidia"
	accelAMD    = "amd"
	accelGaudi  = "gaudi"
	accelNeuron = "neuron"
)

// PCI vendor IDs, from /sys/bus/pci/devices/*/vendor. Neuron devices are not
// found this way: Annapurna Labs (0x1d0f) is also the vendor of ENA and of EBS
// NVMe controllers, so Neuron is detected by its driver's /dev/neuronN nodes.
const (
	pciVendorNVIDIA = "0x10de"
	pciVendorAMDATI = "0x1002" // AMD GPUs; AMD CPUs' host bridges are 0x1022
	pciVendorHabana = "0x1da3"
)

// detectAccelerators returns the accelerator families present, sorted. A GPU is
// a PCI device of a GPU vendor in a display class (0x03xxxx); a Gaudi is any
// Habana device; a Neuron device is a /dev/neuronN node.
func detectAccelerators(pciDevices, devDir string) []string {
	found := map[string]bool{}
	entries, _ := os.ReadDir(pciDevices)
	for _, e := range entries {
		read := func(f string) string {
			b, err := os.ReadFile(filepath.Join(pciDevices, e.Name(), f))
			if err != nil {
				return ""
			}
			return strings.ToLower(strings.TrimSpace(string(b)))
		}
		vendor := read("vendor")
		display := strings.HasPrefix(read("class"), "0x03")
		switch {
		case vendor == pciVendorNVIDIA && display:
			found[accelNVIDIA] = true
		case vendor == pciVendorAMDATI && display:
			found[accelAMD] = true
		case vendor == pciVendorHabana:
			found[accelGaudi] = true
		}
	}
	devs, _ := os.ReadDir(devDir)
	for _, e := range devs {
		if n, ok := strings.CutPrefix(e.Name(), "neuron"); ok {
			if _, err := strconv.Atoi(n); err == nil {
				found[accelNeuron] = true
			}
		}
	}
	families := make([]string, 0, len(found))
	for f := range found {
		families = append(families, f)
	}
	sort.Strings(families)
	return families
}

// accelReading is one family's utilisation: the busiest device, in percent.
type accelReading struct {
	pct      float64
	measured bool
	why      string
}

func unknownAccel(format string, args ...any) accelReading {
	return accelReading{pct: 100, why: fmt.Sprintf(format, args...)}
}

// parsePercentLines parses one utilisation per line (nvidia-smi and hl-smi CSV
// with noheader). A line that is not a number ([N/A], [Not Supported]) is a
// device that could not be read, and makes the reading unknown unless another
// device already shows the accelerator busy.
func parsePercentLines(tool string, out []byte) accelReading {
	var max float64
	var n, bad int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), "%"))
		if line == "" {
			continue
		}
		v, err := strconv.ParseFloat(line, 64)
		if err != nil || v < 0 {
			bad++
			continue
		}
		n++
		if v > max {
			max = v
		}
	}
	switch {
	case max > idleGPUPercent:
		return accelReading{pct: max, measured: true}
	case n == 0:
		return unknownAccel("%s reported no utilisation", tool)
	case bad > 0:
		return unknownAccel("%s could not read utilisation for %d device(s)", tool, bad)
	}
	return accelReading{pct: max, measured: true}
}

// parseRocmSmiJSON parses `rocm-smi --showuse --json`:
// {"card0": {"GPU use (%)": "12"}, ...}.
func parseRocmSmiJSON(out []byte) accelReading {
	var cards map[string]map[string]any
	if err := json.Unmarshal(out, &cards); err != nil {
		return unknownAccel("rocm-smi output did not parse: %v", err)
	}
	var lines []string
	for name, fields := range cards {
		if !strings.HasPrefix(name, "card") {
			continue
		}
		v, ok := fields["GPU use (%)"]
		if !ok {
			lines = append(lines, "?")
			continue
		}
		lines = append(lines, fmt.Sprint(v))
	}
	return parsePercentLines("rocm-smi", []byte(strings.Join(lines, "\n")))
}

// parseNeuronMonitor parses one neuron-monitor report (a JSON object per
// line). Utilisation is per NeuronCore, under each Neuron runtime process:
// neuron_runtime_data[].report.neuroncore_counters.neuroncores_in_use.<core>.neuroncore_utilization.
// An empty runtime list is a measured idle: a NeuronCore can only be used
// through a runtime, and neuron-monitor reports every runtime on the host. A
// report without the list, or with an error, is unknown.
func parseNeuronMonitor(line []byte) accelReading {
	var report struct {
		Runtimes *[]struct {
			Error  string `json:"error"`
			Report struct {
				Counters *struct {
					Error string `json:"error"`
					Cores map[string]struct {
						Utilization *float64 `json:"neuroncore_utilization"`
					} `json:"neuroncores_in_use"`
				} `json:"neuroncore_counters"`
			} `json:"report"`
		} `json:"neuron_runtime_data"`
	}
	if err := json.Unmarshal(line, &report); err != nil {
		return unknownAccel("neuron-monitor output did not parse: %v", err)
	}
	if report.Runtimes == nil {
		return unknownAccel("neuron-monitor report has no neuron_runtime_data")
	}
	var lines []string
	for _, rt := range *report.Runtimes {
		c := rt.Report.Counters
		if rt.Error != "" || c == nil || c.Error != "" {
			lines = append(lines, "?")
			continue
		}
		for _, core := range c.Cores {
			if core.Utilization == nil {
				lines = append(lines, "?")
				continue
			}
			lines = append(lines, strconv.FormatFloat(*core.Utilization, 'f', -1, 64))
		}
	}
	if len(*report.Runtimes) == 0 {
		return accelReading{measured: true}
	}
	return parsePercentLines("neuron-monitor", []byte(strings.Join(lines, "\n")))
}

// accelToolTimeout bounds one tool run. neuron-monitor emits its first report
// after one period (5 s by default).
const accelToolTimeout = 15 * time.Second

// errFirstLineTimeout is a streaming tool that produced no line in time.
var errFirstLineTimeout = errors.New("no output before the timeout")

// runAccelTool runs a utilisation tool and returns its output: all of it, or
// for a tool that streams reports (neuron-monitor), the first line, after which
// the tool is killed. A var so tests can replace the tools.
var runAccelTool = func(ctx context.Context, firstLine bool, name string, args ...string) ([]byte, error) {
	if _, err := exec.LookPath(name); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, accelToolTimeout)
	defer cancel()
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command -- name and args come only from readAccelerator's switch: four literal tool names (nvidia-smi, hl-smi, rocm-smi, neuron-monitor) with literal flags, chosen by the detected hardware family; no user or config input reaches here, and exec runs no shell.
	cmd := exec.CommandContext(ctx, name, args...)
	if !firstLine {
		return cmd.Output()
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, err := bufio.NewReaderSize(stdout, 1<<20).ReadBytes('\n')
	if len(bytes.TrimSpace(line)) == 0 {
		if ctx.Err() != nil {
			return nil, errFirstLineTimeout
		}
		return nil, err
	}
	return line, nil
}

// readAccelerator measures one family with its vendor tool.
func readAccelerator(ctx context.Context, family string) accelReading {
	var tool string
	var firstLine bool
	var args []string
	var parse func([]byte) accelReading
	switch family {
	case accelNVIDIA:
		tool, args = "nvidia-smi", []string{"--query-gpu=utilization.gpu", "--format=csv,noheader,nounits"}
		parse = func(b []byte) accelReading { return parsePercentLines(tool, b) }
	case accelGaudi:
		tool, args = "hl-smi", []string{"-Q", "utilization.aip", "-f", "csv,noheader,nounits"}
		parse = func(b []byte) accelReading { return parsePercentLines(tool, b) }
	case accelAMD:
		tool, args, parse = "rocm-smi", []string{"--showuse", "--json"}, parseRocmSmiJSON
	case accelNeuron:
		tool, firstLine, parse = "neuron-monitor", true, parseNeuronMonitor
	default:
		return unknownAccel("no way to measure %s", family)
	}
	out, err := runAccelTool(ctx, firstLine, tool, args...)
	if err != nil {
		var notFound *exec.Error
		if errors.As(err, &notFound) {
			return unknownAccel("%s device present but %s is not installed", family, tool)
		}
		return unknownAccel("%s failed: %v", tool, err)
	}
	return parse(out)
}

// acceleratorFamilies lists the accelerators on this host; per OS
// (sysAcceleratorFamilies), and a var so tests can stub it.
var acceleratorFamilies = func() []string { return sysAcceleratorFamilies() }

// gpuUtilization returns the busiest accelerator's utilisation, whether it is a
// real measurement, and if not, why. No accelerator is a measured 0. An
// accelerator that is busy decides "active" even if another cannot be read;
// otherwise any unreadable one makes the whole reading unknown.
func (a *Agent) gpuUtilization(ctx context.Context) (pct float64, measured bool, why string) {
	families := acceleratorFamilies()
	measured = true
	var whys []string
	for _, f := range families {
		r := readAccelerator(ctx, f)
		if r.measured && r.pct > idleGPUPercent {
			return r.pct, true, ""
		}
		if !r.measured {
			measured = false
			whys = append(whys, r.why)
			if a.accelUnknownWarned == nil {
				a.accelUnknownWarned = map[string]bool{}
			}
			if !a.accelUnknownWarned[f] {
				a.accelUnknownWarned[f] = true
				log.Printf("WARNING: %s; accelerator activity is treated as active, so idle-stop "+
					"will not trigger while this persists (#954)", r.why)
			}
			continue
		}
		if r.pct > pct {
			pct = r.pct
		}
	}
	if !measured {
		return 100, false, strings.Join(whys, "; ")
	}
	return pct, true, ""
}
