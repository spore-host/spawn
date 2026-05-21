package wizard

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/platform"
)

// Wizard guides users through spawning an instance
type Wizard struct {
	scanner  *bufio.Scanner
	platform *platform.Platform
	config   *aws.LaunchConfig
}

// NewWizard creates a new interactive wizard
func NewWizard(plat *platform.Platform) *Wizard {
	return &Wizard{
		scanner:  bufio.NewScanner(os.Stdin),
		platform: plat,
		config: &aws.LaunchConfig{
			Tags: make(map[string]string),
		},
	}
}

// Run executes the wizard and returns the configuration
func (w *Wizard) Run(ctx context.Context) (*aws.LaunchConfig, error) {
	fmt.Println()
	fmt.Println("╔════════════════════════════════════════════════════════╗")
	fmt.Printf("║  %s %-48s║\n", i18n.Emoji("wizard"), i18n.T("spawn.wizard.title"))
	fmt.Println("╚════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println(i18n.T("spawn.wizard.intro.help"))
	fmt.Println(i18n.T("spawn.wizard.intro.default_hint"))
	fmt.Println()

	// Step 1: Instance Type
	if err := w.askInstanceType(); err != nil {
		return nil, err
	}

	// Step 2: Region
	if err := w.askRegion(); err != nil {
		return nil, err
	}

	// Step 3: Spot or On-Demand
	if err := w.askSpot(); err != nil {
		return nil, err
	}

	// Step 4: Auto-termination
	if err := w.askAutoTerminate(); err != nil {
		return nil, err
	}

	// Step 5: SSH Key
	if err := w.askSSHKey(); err != nil {
		return nil, err
	}

	// Step 6: Name (optional)
	if err := w.askName(); err != nil {
		return nil, err
	}

	// Step 7: Summary and confirm
	return w.confirm()
}

func (w *Wizard) askInstanceType() error {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("%s %s\n", i18n.Emoji("package"), i18n.T("spawn.wizard.step1.title"))
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println(i18n.T("spawn.wizard.step1.common_choices"))
	fmt.Println()
	fmt.Printf("  %s %s\n", i18n.Emoji("computer"), i18n.T("spawn.wizard.step1.category.dev"))
	fmt.Println(i18n.T("spawn.wizard.step1.instance.t3_medium"))
	fmt.Println(i18n.T("spawn.wizard.step1.instance.t3_large"))
	fmt.Println()
	fmt.Printf("  %s %s\n", i18n.Emoji("gear"), i18n.T("spawn.wizard.step1.category.general"))
	fmt.Println(i18n.T("spawn.wizard.step1.instance.m7i_large"))
	fmt.Println(i18n.T("spawn.wizard.step1.instance.m7i_xlarge"))
	fmt.Println()
	fmt.Printf("  %s %s\n", i18n.Emoji("rocket"), i18n.T("spawn.wizard.step1.category.compute"))
	fmt.Println(i18n.T("spawn.wizard.step1.instance.c7i_xlarge"))
	fmt.Println(i18n.T("spawn.wizard.step1.instance.c7i_2xlarge"))
	fmt.Println()
	fmt.Printf("  %s %s\n", i18n.Emoji("gpu"), i18n.T("spawn.wizard.step1.category.gpu"))
	fmt.Println(i18n.T("spawn.wizard.step1.instance.g6_xlarge"))
	fmt.Println(i18n.T("spawn.wizard.step1.instance.p5_48xlarge"))
	fmt.Println()
	fmt.Print(i18n.T("spawn.wizard.step1.prompt"))

	instanceType := w.readLine()
	if instanceType == "" {
		instanceType = "t3.medium"
	}

	w.config.InstanceType = instanceType

	// Detect characteristics
	arch := aws.DetectArchitecture(instanceType)
	gpu := aws.DetectGPUInstance(instanceType)

	fmt.Println()
	if gpu {
		fmt.Printf("  %s %s\n", i18n.Symbol("success"), i18n.Tf("spawn.wizard.step1.detected_gpu", map[string]interface{}{
			"Architecture": arch,
		}))
	} else {
		fmt.Printf("  %s %s\n", i18n.Symbol("success"), i18n.Tf("spawn.wizard.step1.detected", map[string]interface{}{
			"Architecture": arch,
		}))
	}
	fmt.Println()

	return nil
}

func (w *Wizard) askRegion() error {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("%s %s\n", i18n.Emoji("globe"), i18n.T("spawn.wizard.step2.title"))
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println(i18n.T("spawn.wizard.step2.common_regions"))
	fmt.Println()
	fmt.Printf("  %s %s\n", i18n.Emoji("flag_us"), i18n.T("spawn.wizard.step2.region.us"))
	fmt.Println(i18n.T("spawn.wizard.step2.region.us_east_1"))
	fmt.Println(i18n.T("spawn.wizard.step2.region.us_west_2"))
	fmt.Println()
	fmt.Printf("  %s %s\n", i18n.Emoji("flag_eu"), i18n.T("spawn.wizard.step2.region.eu"))
	fmt.Println(i18n.T("spawn.wizard.step2.region.eu_west_1"))
	fmt.Println(i18n.T("spawn.wizard.step2.region.eu_central_1"))
	fmt.Println()
	fmt.Printf("  %s %s\n", i18n.Emoji("flag_asia"), i18n.T("spawn.wizard.step2.region.asia"))
	fmt.Println(i18n.T("spawn.wizard.step2.region.ap_northeast_1"))
	fmt.Println(i18n.T("spawn.wizard.step2.region.ap_southeast_1"))
	fmt.Println()

	defaultRegion := "us-east-1"
	fmt.Print(i18n.Tf("spawn.wizard.step2.prompt", map[string]interface{}{
		"Default": defaultRegion,
	}))

	region := w.readLine()
	if region == "" {
		region = defaultRegion
	}

	w.config.Region = region
	fmt.Println()

	return nil
}

func (w *Wizard) askSpot() error {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("%s %s\n", i18n.Emoji("money"), i18n.T("spawn.wizard.step3.title"))
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Printf("%s %s\n", i18n.Emoji("lightbulb"), i18n.T("spawn.wizard.step3.description"))
	fmt.Println()
	fmt.Printf("   %s %s\n", i18n.Symbol("success"), i18n.T("spawn.wizard.step3.good_for"))
	fmt.Printf("   %s %s\n", i18n.Symbol("warning"), i18n.T("spawn.wizard.step3.not_for"))
	fmt.Println()
	fmt.Print(i18n.T("spawn.wizard.step3.prompt"))

	response := strings.ToLower(w.readLine())
	if response == "y" || response == "yes" {
		w.config.Spot = true
		fmt.Println()
		fmt.Printf("  %s %s\n", i18n.Symbol("success"), i18n.T("spawn.wizard.step3.using_spot"))
	} else {
		fmt.Println()
		fmt.Printf("  %s %s\n", i18n.Symbol("success"), i18n.T("spawn.wizard.step3.using_ondemand"))
	}
	fmt.Println()

	return nil
}

func (w *Wizard) askAutoTerminate() error {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("%s %s\n", i18n.Emoji("clock"), i18n.T("spawn.wizard.step4.title"))
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println(i18n.T("spawn.wizard.step4.description"))
	fmt.Println()
	fmt.Printf("  %s %s\n", i18n.Emoji("one"), i18n.T("spawn.wizard.step4.option.ttl"))
	fmt.Printf("  %s %s\n", i18n.Emoji("two"), i18n.T("spawn.wizard.step4.option.idle"))
	fmt.Printf("  %s %s\n", i18n.Emoji("three"), i18n.T("spawn.wizard.step4.option.both"))
	fmt.Printf("  %s %s\n", i18n.Emoji("four"), i18n.T("spawn.wizard.step4.option.manual"))
	fmt.Println()
	fmt.Print(i18n.T("spawn.wizard.step4.prompt"))

	choice := w.readLine()
	if choice == "" {
		choice = "3"
	}

	fmt.Println()

	switch choice {
	case "1":
		fmt.Print(i18n.T("spawn.wizard.step4.ttl_prompt"))
		ttl := w.readLine()
		if ttl == "" {
			ttl = "8h"
		}
		w.config.TTL = ttl
		fmt.Printf("  %s %s\n", i18n.Symbol("success"), i18n.Tf("spawn.wizard.step4.ttl_set", map[string]interface{}{
			"TTL": ttl,
		}))

	case "2":
		fmt.Print(i18n.T("spawn.wizard.step4.idle_prompt"))
		idle := w.readLine()
		if idle == "" {
			idle = "1h"
		}
		w.config.IdleTimeout = idle
		fmt.Printf("  %s %s\n", i18n.Symbol("success"), i18n.Tf("spawn.wizard.step4.idle_set", map[string]interface{}{
			"Idle": idle,
		}))

	case "3":
		fmt.Print(i18n.T("spawn.wizard.step4.ttl_prompt_short"))
		ttl := w.readLine()
		if ttl == "" {
			ttl = "8h"
		}
		w.config.TTL = ttl

		fmt.Print(i18n.T("spawn.wizard.step4.idle_prompt_short"))
		idle := w.readLine()
		if idle == "" {
			idle = "1h"
		}
		w.config.IdleTimeout = idle
		fmt.Printf("  %s %s\n", i18n.Symbol("success"), i18n.Tf("spawn.wizard.step4.both_set", map[string]interface{}{
			"TTL":  ttl,
			"Idle": idle,
		}))

	case "4":
		fmt.Printf("  %s %s\n", i18n.Symbol("warning"), i18n.T("spawn.wizard.step4.manual_warning"))
		fmt.Println(i18n.T("spawn.wizard.step4.manual_command"))
	}

	fmt.Println()
	return nil
}

func (w *Wizard) askSSHKey() error {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("%s %s\n", i18n.Emoji("key"), i18n.T("spawn.wizard.step5.title"))
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// Check for existing key
	if w.platform.HasSSHKey() {
		fmt.Printf("%s %s\n", i18n.Symbol("success"), i18n.Tf("spawn.wizard.step5.found_key", map[string]interface{}{
			"Path": w.platform.SSHKeyPath,
		}))
		fmt.Println(i18n.T("spawn.wizard.step5.will_use_key"))
		w.config.KeyName = "default-ssh-key"
	} else {
		fmt.Printf("%s %s\n", i18n.Symbol("warning"), i18n.Tf("spawn.wizard.step5.no_key_found", map[string]interface{}{
			"Path": w.platform.SSHKeyPath,
		}))
		fmt.Println()
		fmt.Println(i18n.T("spawn.wizard.step5.key_required"))
		fmt.Println()
		fmt.Print(i18n.T("spawn.wizard.step5.create_prompt"))

		response := strings.ToLower(w.readLine())
		if response == "" || response == "y" || response == "yes" {
			fmt.Println()
			fmt.Printf("  %s %s\n", i18n.Emoji("wrench"), i18n.T("spawn.wizard.step5.creating_key"))

			if err := w.platform.CreateSSHKey(); err != nil {
				return i18n.Te("error.ssh_key_create_failed", err)
			}

			fmt.Printf("  %s %s\n", i18n.Symbol("success"), i18n.Tf("spawn.wizard.step5.key_created", map[string]interface{}{
				"Path": w.platform.SSHKeyPath,
			}))
			w.config.KeyName = "default-ssh-key"
		} else {
			return i18n.Te("error.ssh_key_required", nil)
		}
	}

	fmt.Println()
	return nil
}

func (w *Wizard) askName() error {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("%s %s\n", i18n.Emoji("tag"), i18n.T("spawn.wizard.step6.title"))
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Print(i18n.T("spawn.wizard.step6.prompt"))

	name := w.readLine()
	if name != "" {
		w.config.Name = name
		fmt.Printf("  %s %s\n", i18n.Symbol("success"), i18n.Tf("spawn.wizard.step6.name_set", map[string]interface{}{
			"Name": name,
		}))
	}

	fmt.Println()
	return nil
}

func (w *Wizard) confirm() (*aws.LaunchConfig, error) {
	fmt.Println()
	fmt.Println("╔════════════════════════════════════════════════════════╗")
	fmt.Printf("║  %s %-48s║\n", i18n.Emoji("clipboard"), i18n.T("spawn.wizard.summary.title"))
	fmt.Println("╚════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Display configuration
	fmt.Println(i18n.T("spawn.wizard.summary.about_to_launch"))
	fmt.Println()
	fmt.Printf("  %s %s\n", i18n.T("spawn.wizard.summary.label.instance_type"), w.config.InstanceType)
	fmt.Printf("  %s         %s\n", i18n.T("spawn.wizard.summary.label.region"), w.config.Region)

	if w.config.Name != "" {
		fmt.Printf("  %s           %s\n", i18n.T("spawn.wizard.summary.label.name"), w.config.Name)
	}

	if w.config.Spot {
		fmt.Printf("  %s           %s\n", i18n.T("spawn.wizard.summary.label.type"), i18n.T("spawn.wizard.summary.type_spot"))
	} else {
		fmt.Printf("  %s           %s\n", i18n.T("spawn.wizard.summary.label.type"), i18n.T("spawn.wizard.summary.type_ondemand"))
	}

	if w.config.TTL != "" {
		fmt.Printf("  %s     %s\n", i18n.T("spawn.wizard.summary.label.time_limit"), w.config.TTL)
	}

	if w.config.IdleTimeout != "" {
		fmt.Printf("  %s   %s\n", i18n.T("spawn.wizard.summary.label.idle_timeout"), w.config.IdleTimeout)
	}

	// Estimate cost
	fmt.Println()
	cost := estimateCost(w.config.InstanceType, w.config.Spot)
	fmt.Printf("%s %s\n", i18n.Emoji("money"), i18n.Tf("spawn.wizard.summary.estimated_cost", map[string]interface{}{
		"Cost": fmt.Sprintf("%.2f", cost),
	}))

	if w.config.Spot {
		onDemandCost := estimateCost(w.config.InstanceType, false)
		savings := ((onDemandCost - cost) / onDemandCost) * 100
		fmt.Printf(" (%s)\n", i18n.Tf("spawn.wizard.summary.savings", map[string]interface{}{
			"Percent": fmt.Sprintf("%.0f", savings),
		}))
	} else {
		fmt.Println()
	}

	if w.config.TTL != "" {
		duration, _ := time.ParseDuration(w.config.TTL)
		hours := duration.Hours()
		totalCost := cost * hours
		fmt.Printf("   %s\n", i18n.Tf("spawn.wizard.summary.total_cost", map[string]interface{}{
			"Duration": w.config.TTL,
			"Cost":     fmt.Sprintf("%.2f", totalCost),
		}))
	}

	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Printf("%s %s", i18n.Emoji("rocket"), i18n.T("spawn.wizard.summary.confirm"))

	response := strings.ToLower(w.readLine())
	if response == "" || response == "y" || response == "yes" {
		fmt.Println()
		return w.config, nil
	}

	return nil, i18n.Te("error.cancelled_by_user", nil)
}

func (w *Wizard) readLine() string {
	w.scanner.Scan()
	return strings.TrimSpace(w.scanner.Text())
}

// estimateCost estimates hourly cost for an instance type
func estimateCost(instanceType string, spot bool) float64 {
	// Rough estimates - in production, query AWS Pricing API
	costs := map[string]float64{
		"t3.micro":    0.01,
		"t3.small":    0.02,
		"t3.medium":   0.04,
		"t3.large":    0.08,
		"m7i.large":   0.10,
		"m7i.xlarge":  0.20,
		"c7i.xlarge":  0.17,
		"c7i.2xlarge": 0.34,
		"g6.xlarge":   1.21,
		"p5.48xlarge": 98.0,
	}

	cost, ok := costs[instanceType]
	if !ok {
		// Default estimate if not found
		cost = 0.10
	}

	if spot {
		// Spot is typically 60-70% cheaper
		cost = cost * 0.35
	}

	return cost
}
