package progress

import (
	"fmt"
	"runtime"
	"time"

	"github.com/spore-host/libs/i18n"
)

// Progress tracks and displays spawn progress
type Progress struct {
	steps       []Step
	currentStep int
	quiet       bool // when true, suppress all TUI output (e.g. for -o json)
}

// Step represents a single step in the spawn process
type Step struct {
	Name      string
	Status    string // pending, running, complete, error
	StartTime time.Time
	EndTime   time.Time
}

// NewProgress creates a new progress tracker
func NewProgress() *Progress {
	return newProgress(false)
}

// NewQuietProgress creates a progress tracker that suppresses all TUI output.
// Use when stdout must stay clean for machine-readable output (e.g. -o json).
func NewQuietProgress() *Progress {
	return newProgress(true)
}

func newProgress(quiet bool) *Progress {
	p := defaultProgress()
	p.quiet = quiet
	return p
}

func defaultProgress() *Progress {
	return &Progress{
		steps: []Step{
			{Name: i18n.T("spawn.progress.detecting_ami"), Status: "pending"},
			{Name: i18n.T("spawn.progress.setup_ssh_key"), Status: "pending"},
			{Name: i18n.T("spawn.progress.setup_iam_role"), Status: "pending"},
			{Name: i18n.T("spawn.progress.create_security_group"), Status: "pending"},
			{Name: i18n.T("spawn.progress.launching_instance"), Status: "pending"},
			{Name: i18n.T("spawn.progress.install_agent"), Status: "pending"},
			{Name: i18n.T("spawn.progress.waiting_instance"), Status: "pending"},
			{Name: i18n.T("spawn.progress.get_public_ip"), Status: "pending"},
			{Name: i18n.T("spawn.progress.waiting_ssh"), Status: "pending"},
		},
		currentStep: 0,
	}
}

// Start marks a step as started
func (p *Progress) Start(stepName string) {
	for i := range p.steps {
		if p.steps[i].Name == stepName {
			p.steps[i].Status = "running"
			p.steps[i].StartTime = time.Now()
			p.currentStep = i
			p.display()
			return
		}
	}
}

// Complete marks a step as complete
func (p *Progress) Complete(stepName string) {
	for i := range p.steps {
		if p.steps[i].Name == stepName {
			p.steps[i].Status = "complete"
			p.steps[i].EndTime = time.Now()
			p.display()
			return
		}
	}
}

// Error marks a step as errored
func (p *Progress) Error(stepName string, err error) {
	for i := range p.steps {
		if p.steps[i].Name == stepName {
			p.steps[i].Status = "error"
			p.steps[i].EndTime = time.Now()
			p.display()
			if !p.quiet {
				fmt.Println()
				fmt.Printf("%s %s: %v\n", i18n.Symbol("error"), i18n.T("spawn.progress.error"), err)
			}
			return
		}
	}
}

// Skip marks a step as skipped
func (p *Progress) Skip(stepName string) {
	for i := range p.steps {
		if p.steps[i].Name == stepName {
			p.steps[i].Status = "skipped"
			p.display()
			return
		}
	}
}

// display shows the current progress
func (p *Progress) display() {
	if p.quiet {
		return
	}
	// Clear screen and move cursor to top (skip in accessibility mode or when not a TTY)
	if i18n.Global == nil || (!i18n.Global.AccessibilityMode() && runtime.GOOS != "windows") {
		fmt.Print("\033[2J\033[H")
	} else {
		fmt.Println()
	}

	fmt.Println()
	fmt.Println("╔════════════════════════════════════════════════════════╗")
	fmt.Printf("║  %s %-48s║\n", i18n.Emoji("rocket"), i18n.T("spawn.progress.title"))
	fmt.Println("╚════════════════════════════════════════════════════════╝")
	fmt.Println()

	for i, step := range p.steps {
		symbol := getSymbol(step.Status)
		duration := ""

		if step.Status == "complete" && !step.StartTime.IsZero() && !step.EndTime.IsZero() {
			elapsed := step.EndTime.Sub(step.StartTime)
			if elapsed < time.Second {
				duration = fmt.Sprintf(" (%.0fms)", elapsed.Seconds()*1000)
			} else {
				duration = fmt.Sprintf(" (%.1fs)", elapsed.Seconds())
			}
		} else if step.Status == "running" && !step.StartTime.IsZero() {
			elapsed := time.Since(step.StartTime)
			duration = fmt.Sprintf(" (%.1fs)", elapsed.Seconds())
		}

		// Highlight current step
		if i == p.currentStep && step.Status == "running" {
			fmt.Printf("  %s %s...%s\n", symbol, step.Name, duration)
		} else {
			fmt.Printf("  %s %s%s\n", symbol, step.Name, duration)
		}
	}

	fmt.Println()
}

// DisplaySuccess shows the final success message
func (p *Progress) DisplaySuccess(instanceID, publicIP, sshCommand string, config interface{}) {
	if p.quiet {
		return
	}
	fmt.Println()
	fmt.Println("╔════════════════════════════════════════════════════════╗")
	fmt.Printf("║  %s %-48s║\n", i18n.Emoji("party"), i18n.T("spawn.progress.success.title"))
	fmt.Println("╚════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println(i18n.T("spawn.progress.success.details"))
	fmt.Println()
	fmt.Printf("  %s  %s\n", i18n.T("spawn.progress.success.label.instance_id"), instanceID)
	fmt.Printf("  %s    %s\n", i18n.T("spawn.progress.success.label.public_ip"), publicIP)
	fmt.Printf("  %s       %s\n", i18n.T("spawn.progress.success.label.status"), i18n.T("spawn.progress.success.status_running"))
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Printf("%s %s\n", i18n.Emoji("plug"), i18n.T("spawn.progress.success.connect_now"))
	fmt.Println()
	fmt.Printf("  %s\n", sshCommand)
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// Show monitoring info if applicable
	if launchConfig, ok := config.(LaunchConfigInterface); ok {
		if launchConfig.GetTTL() != "" || launchConfig.GetIdleTimeout() != "" {
			fmt.Printf("%s %s\n", i18n.Emoji("lightbulb"), i18n.T("spawn.progress.success.monitoring.title"))
			fmt.Println()
			if ttl := launchConfig.GetTTL(); ttl != "" {
				fmt.Printf("   %s %s\n", i18n.Emoji("clock"), i18n.Tf("spawn.progress.success.monitoring.ttl", map[string]interface{}{
					"TTL": ttl,
				}))
			}
			if idle := launchConfig.GetIdleTimeout(); idle != "" {
				fmt.Printf("   %s %s\n", i18n.Emoji("zzz"), i18n.Tf("spawn.progress.success.monitoring.idle", map[string]interface{}{
					"Idle": idle,
				}))
			}
			fmt.Println()
			fmt.Println(i18n.T("spawn.progress.success.monitoring.agent_active"))
			fmt.Println(i18n.T("spawn.progress.success.monitoring.close_laptop"))
			// Reassure the user the deadline is guaranteed even if the in-instance
			// agent fails: a server-side reaper backstops it (spore-host/spawn#70).
			// Literal (not i18n) to avoid a cross-repo libs release for one line;
			// promote to an i18n key on the next libs bump.
			fmt.Printf("   %s A server-side reaper enforces the deadline as a backstop, even if the agent fails.\n", i18n.Emoji("check"))
			fmt.Println()
		}
	}

	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
}

// LaunchConfigInterface defines methods for getting launch config details
type LaunchConfigInterface interface {
	GetTTL() string
	GetIdleTimeout() string
}

func getSymbol(status string) string {
	switch status {
	case "pending":
		return i18n.Symbol("pending") + " "
	case "running":
		return i18n.Symbol("running")
	case "complete":
		return i18n.Symbol("success")
	case "error":
		return i18n.Symbol("error")
	case "skipped":
		return i18n.Symbol("skipped") + " "
	default:
		return "  "
	}
}

// Spinner shows a simple spinner for long operations
type Spinner struct {
	message string
	frames  []string
	stop    chan bool
}

// NewSpinner creates a new spinner
func NewSpinner(message string) *Spinner {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	if i18n.Global != nil && i18n.Global.AccessibilityMode() {
		frames = []string{"|", "/", "-", "\\"}
	}
	return &Spinner{
		message: message,
		frames:  frames,
		stop:    make(chan bool),
	}
}

// Start starts the spinner
func (s *Spinner) Start() {
	go func() {
		i := 0
		for {
			select {
			case <-s.stop:
				return
			default:
				fmt.Printf("\r  %s %s", s.frames[i], s.message)
				i = (i + 1) % len(s.frames)
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()
}

// Stop stops the spinner
func (s *Spinner) Stop(success bool) {
	s.stop <- true
	if success {
		fmt.Printf("\r  %s %s\n", i18n.Symbol("success"), s.message)
	} else {
		fmt.Printf("\r  %s %s\n", i18n.Symbol("error"), s.message)
	}
}
