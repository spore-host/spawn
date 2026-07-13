package progress

import (
	"fmt"
	"os"
	"time"
	"unicode"

	"github.com/spore-host/libs/i18n"
)

// Progress tracks and displays spawn progress
type Progress struct {
	steps       []Step
	currentStep int
	quiet       bool            // when true, suppress all TUI output (e.g. for -o json)
	tty         bool            // stdout is an interactive terminal (redraw in place)
	printed     map[string]bool // non-TTY: step transitions already logged
}

// Box drawing. The interior between the ║ borders is boxWidth columns wide.
const boxWidth = 56

var (
	boxTop    = "╔" + repeat("═", boxWidth) + "╗"
	boxBottom = "╚" + repeat("═", boxWidth) + "╝"
)

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// boxLine renders "║  <emoji> <text>...║" padded so the right border lands at
// exactly boxWidth display columns. Emoji render two columns wide but count as
// one rune, so plain %-Ns padding misaligns the border — displayWidth accounts
// for that.
func boxLine(emoji, text string) string {
	content := "  " + emoji + " " + text
	pad := boxWidth - displayWidth(content)
	if pad < 0 {
		pad = 0
	}
	return "║" + content + repeat(" ", pad) + "║"
}

// displayWidth returns the number of terminal columns a string occupies,
// counting wide runes (CJK, most emoji, and variation-selector sequences) as 2.
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r == '️': // variation selector: renders the previous rune wide
			w++
		case unicode.IsControl(r):
			// no width
		case isWideRune(r):
			w += 2
		default:
			w++
		}
	}
	return w
}

// isWideRune reports whether r is rendered as a double-width glyph. Covers the
// common CJK ranges and the emoji blocks spawn uses (🚀 🎉 🔌 💡 ⏰ 💤 etc.).
func isWideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0x303E, // CJK radicals, Kangxi
		r >= 0x3041 && r <= 0x33FF, // Hiragana … CJK symbols
		r >= 0x3400 && r <= 0x4DBF, // CJK Ext A
		r >= 0x4E00 && r <= 0x9FFF, // CJK Unified
		r >= 0xA000 && r <= 0xA4CF, // Yi
		r >= 0xAC00 && r <= 0xD7A3, // Hangul syllables
		r >= 0xF900 && r <= 0xFAFF, // CJK compat
		r >= 0xFE30 && r <= 0xFE4F, // CJK compat forms
		r >= 0xFF00 && r <= 0xFF60, // fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F300 && r <= 0x1FAFF, // emoji & pictographs
		r >= 0x2600 && r <= 0x27BF:   // misc symbols & dingbats
		return true
	}
	return false
}

// stdoutIsTTY reports whether stdout is an interactive terminal. When it isn't
// (piped, captured, CI), the in-place redraw (ANSI clear-screen) does nothing
// and every display() call would re-print the whole box, so we fall back to a
// plain line-per-step log instead.
func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
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
	p.tty = stdoutIsTTY()
	p.printed = make(map[string]bool)
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

// display shows the current progress.
func (p *Progress) display() {
	if p.quiet {
		return
	}
	// When stdout isn't a terminal (piped/captured/CI), a full-box redraw can't
	// clear the previous frame, so it would stack. Log one line per transition.
	if !p.tty {
		p.displayPlain()
		return
	}

	// Clear screen and move cursor to top (skip in accessibility mode).
	if i18n.Global == nil || !i18n.Global.AccessibilityMode() {
		fmt.Print("\033[2J\033[H")
	} else {
		fmt.Println()
	}

	fmt.Println()
	fmt.Println(boxTop)
	fmt.Println(boxLine(i18n.Emoji("rocket"), i18n.T("spawn.progress.title")))
	fmt.Println(boxBottom)
	fmt.Println()

	for i, step := range p.steps {
		symbol := getSymbol(step.Status)
		duration := stepDuration(step)

		// Highlight current step
		if i == p.currentStep && step.Status == "running" {
			fmt.Printf("  %s %s...%s\n", symbol, step.Name, duration)
		} else {
			fmt.Printf("  %s %s%s\n", symbol, step.Name, duration)
		}
	}

	fmt.Println()
}

// displayPlain logs step transitions one line at a time for non-TTY output,
// emitting each step's terminal state (complete/error) exactly once.
func (p *Progress) displayPlain() {
	for _, step := range p.steps {
		if step.Status != "complete" && step.Status != "error" {
			continue
		}
		if p.printed[step.Name] {
			continue
		}
		p.printed[step.Name] = true
		fmt.Printf("  %s %s%s\n", getSymbol(step.Status), step.Name, stepDuration(step))
	}
}

// stepDuration renders a step's elapsed-time suffix (e.g. " (628ms)").
func stepDuration(step Step) string {
	switch {
	case step.Status == "complete" && !step.StartTime.IsZero() && !step.EndTime.IsZero():
		elapsed := step.EndTime.Sub(step.StartTime)
		if elapsed < time.Second {
			return fmt.Sprintf(" (%.0fms)", elapsed.Seconds()*1000)
		}
		return fmt.Sprintf(" (%.1fs)", elapsed.Seconds())
	case step.Status == "running" && !step.StartTime.IsZero():
		return fmt.Sprintf(" (%.1fs)", time.Since(step.StartTime).Seconds())
	}
	return ""
}

// DisplaySuccess shows the final success message
func (p *Progress) DisplaySuccess(instanceID, publicIP, sshCommand string, config interface{}) {
	if p.quiet {
		return
	}
	fmt.Println()
	fmt.Println(boxTop)
	fmt.Println(boxLine(i18n.Emoji("party"), i18n.T("spawn.progress.success.title")))
	fmt.Println(boxBottom)
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
