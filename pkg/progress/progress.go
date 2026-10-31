package progress

import (
	"fmt"
	"io"
	"os"
	"sync"
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
	printed     map[string]bool // non-TTY: terminal state already logged, per step
	started     map[string]bool // non-TTY: start already logged, per step
	errOut      io.Writer       // where Error prints; nil is os.Stderr
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
	p.started = make(map[string]bool)
	return p
}

// defaultProgress pre-registers the steps a single launch always runs, in
// display order. Every one must be started by the launch, or it shows as
// pending for good: "Installing spawn agent" sat here with an hourglass after
// the agent was verified, because spored installs from user-data and no code
// ever started that step; "Verifying spored agent" reports it (#1125).
// TestEveryPreregisteredStepIsStarted keeps the list honest.
func defaultProgress() *Progress {
	return &Progress{
		steps: []Step{
			{Name: i18n.T("spawn.progress.detecting_ami"), Status: "pending"},
			{Name: i18n.T("spawn.progress.setup_ssh_key"), Status: "pending"},
			{Name: i18n.T("spawn.progress.setup_iam_role"), Status: "pending"},
			{Name: i18n.T("spawn.progress.create_security_group"), Status: "pending"},
			{Name: i18n.T("spawn.progress.launching_instance"), Status: "pending"},
			{Name: i18n.T("spawn.progress.waiting_instance"), Status: "pending"},
			{Name: i18n.T("spawn.progress.get_public_ip"), Status: "pending"},
			{Name: i18n.T("spawn.progress.waiting_ssh"), Status: "pending"},
		},
		currentStep: 0,
	}
}

// stepIndex returns the index of stepName, APPENDING it if the fixed list does
// not already contain it.
//
// Appending rather than ignoring is the whole of #739. Start/Complete/Error/Skip
// used to scan the nine-step list and `return` silently on no match — and an
// audit found 32 distinct names passed from cmd/, of which **24 matched nothing**.
// Every one of those calls was a no-op, and the 24 were not a random sample: they
// were spawn's LONGEST operations — `Verifying spored agent` (5 min),
// `Waiting for Windows (password available)` (12 min), every FSx step, the whole
// Windows ISO->AMI import. The eight that matched all belonged to the fast
// pre-launch phase, which is exactly why nobody noticed: everything that printed
// finished in seconds and everything slow was invisible.
//
// A fixed list was always the wrong shape for this. Several steps are
// conditional — FSx, MPI, Windows, cohort reconciliation only happen on some
// launches — and some labels are computed at runtime (cmd/image.go builds them in
// variables), so no static registry can enumerate them. Find-or-append makes the
// step list describe what this launch actually did.
func (p *Progress) stepIndex(stepName string) int {
	for i := range p.steps {
		if p.steps[i].Name == stepName {
			return i
		}
	}
	p.steps = append(p.steps, Step{Name: stepName, Status: "pending"})
	return len(p.steps) - 1
}

// Start marks a step as started
func (p *Progress) Start(stepName string) {
	i := p.stepIndex(stepName)
	p.steps[i].Status = "running"
	p.steps[i].StartTime = time.Now()
	p.currentStep = i
	p.display()
}

// Complete marks a step as complete
func (p *Progress) Complete(stepName string) {
	i := p.stepIndex(stepName)
	p.steps[i].Status = "complete"
	p.steps[i].EndTime = time.Now()
	p.display()
}

// Error marks a step as errored and prints err, on stderr.
//
// A caller usually returns err as well, and the root command prints what is
// returned, so a launch failure printed twice: once here, then again plain
// (#1470). Error records the text it printed and [Reported] tells the root
// printer it has been shown. A quiet tracker (-o json) prints nothing and
// records nothing, so the root printer still shows the error there.
func (p *Progress) Error(stepName string, err error) {
	i := p.stepIndex(stepName)
	p.steps[i].Status = "error"
	p.steps[i].EndTime = time.Now()
	p.display()
	// Outside the lookup on purpose: this used to sit INSIDE the matched-step
	// branch, so an unregistered name swallowed the error text too — a failure
	// reported nowhere at all unless the caller also returned the error.
	if !p.quiet && err != nil {
		w := p.errOut
		if w == nil {
			w = os.Stderr
		}
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintf(w, "%s %s: %v\n", i18n.Symbol("error"), i18n.T("spawn.progress.error"), err)
		reported.add(err.Error())
	}
}

// SetErrorOutput sends the error lines [Progress.Error] prints to w instead
// of stderr.
func (p *Progress) SetErrorOutput(w io.Writer) { p.errOut = w }

// reported is the text of every error a Progress has printed in this process.
var reported = reportedErrors{texts: map[string]bool{}}

type reportedErrors struct {
	mu    sync.Mutex
	texts map[string]bool
}

func (r *reportedErrors) add(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.texts[text] = true
}

// Reported reports whether a Progress has already printed err's text, so a
// caller about to print the error a command returned need not repeat it
// (#1470). An error that wraps the printed one in more text is not reported:
// the added text has not been shown.
func Reported(err error) bool {
	if err == nil {
		return false
	}
	reported.mu.Lock()
	defer reported.mu.Unlock()
	return reported.texts[err.Error()]
}

// Skip marks a step as skipped
func (p *Progress) Skip(stepName string) {
	i := p.stepIndex(stepName)
	p.steps[i].Status = "skipped"
	p.display()
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
		// A skipped step is one this launch doesn't do (no FSx, no security
		// group needed). Listing it read as something the user had asked for,
		// or as a failure (#1124), so it isn't shown.
		if step.Status == "skipped" {
			continue
		}
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
// emitting each step's START and then its terminal state (complete/error/
// skipped) exactly once each.
//
// It used to skip any step that was not already complete or error, so a RUNNING
// step printed nothing — measured at literally 0 bytes (#739). On a TTY the box
// at least shows the step marked running; on a pipe, a capture, or CI, the step
// announced itself only once it had finished. That is backwards: the whole value
// of progress output on a non-interactive stream is knowing what is in flight,
// and it meant a five-minute wait and a wedged process looked identical.
func (p *Progress) displayPlain() {
	for _, step := range p.steps {
		switch step.Status {
		case "running":
			if p.started[step.Name] {
				continue
			}
			p.started[step.Name] = true
			fmt.Printf("  %s %s...\n", getSymbol(step.Status), step.Name)
		case "complete", "error": // not "skipped": see display (#1124)
			if p.printed[step.Name] {
				continue
			}
			p.printed[step.Name] = true
			fmt.Printf("  %s %s%s\n", getSymbol(step.Status), step.Name, stepDuration(step))
		}
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

// SuccessSummary is what the launch summary shows. Empty optional fields are
// omitted.
type SuccessSummary struct {
	InstanceID string
	// PublicIP is the instance's public address; empty means it has none and
	// is shown as NoPublicIP (#1443).
	PublicIP        string
	InstanceProfile string // the resolved IAM instance profile (#550)
	ConnectCommand  string
	DNSRecord       string // the FQDN spored registered, if any
	DNSConnect      string // how to connect by that name
}

// NoPublicIP is the launch summary's Public IP for an instance that has none
// (--no-public-ip, or a subnet that assigns none), as the dry run shows it.
const NoPublicIP = "none (private only)"

// sectionRule closes each section of the launch summary.
const sectionRule = "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

// DisplaySuccess shows the final success message
func (p *Progress) DisplaySuccess(s SuccessSummary, config interface{}) {
	if p.quiet {
		return
	}
	renderSuccess(os.Stdout, s, config)
}

// renderSuccess writes the launch summary: a title box, then sections, each
// closed by a rule. Every section owns its closing rule, so the summary always
// ends on one and two rules are never adjacent. Before #1125 the rules were
// printed between sections unconditionally: an absent section left two rules
// back to back, and the IAM profile line, printed by the caller after the
// final rule, opened a section nothing closed.
func renderSuccess(w io.Writer, s SuccessSummary, config interface{}) {
	section := func(lines ...string) {
		for _, l := range lines {
			_, _ = fmt.Fprintln(w, l)
		}
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, sectionRule)
		_, _ = fmt.Fprintln(w)
	}

	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, boxTop)
	_, _ = fmt.Fprintln(w, boxLine(i18n.Emoji("party"), i18n.T("spawn.progress.success.title")))
	_, _ = fmt.Fprintln(w, boxBottom)
	_, _ = fmt.Fprintln(w)

	publicIP := s.PublicIP
	if publicIP == "" {
		publicIP = NoPublicIP
	}
	details := []string{
		i18n.T("spawn.progress.success.details"),
		"",
		fmt.Sprintf("  %s  %s", i18n.T("spawn.progress.success.label.instance_id"), s.InstanceID),
		fmt.Sprintf("  %s    %s", i18n.T("spawn.progress.success.label.public_ip"), publicIP),
		fmt.Sprintf("  %s       %s", i18n.T("spawn.progress.success.label.status"), i18n.T("spawn.progress.success.status_running")),
	}
	if s.InstanceProfile != "" {
		details = append(details, fmt.Sprintf("  IAM instance profile: %s", s.InstanceProfile))
	}
	section(details...)

	connect := []string{
		fmt.Sprintf("%s %s", i18n.Emoji("plug"), i18n.T("spawn.progress.success.connect_now")),
		"",
		"  " + s.ConnectCommand,
	}
	if s.DNSRecord != "" {
		connect = append(connect, "", fmt.Sprintf("%s DNS: %s", i18n.Emoji("globe"), s.DNSRecord))
		if s.DNSConnect != "" {
			connect = append(connect, "   Connect: "+s.DNSConnect)
		}
	}
	section(connect...)

	// Show monitoring info if applicable
	if launchConfig, ok := config.(LaunchConfigInterface); ok {
		if launchConfig.GetTTL() != "" || launchConfig.GetIdleTimeout() != "" {
			monitoring := []string{
				fmt.Sprintf("%s %s", i18n.Emoji("lightbulb"), i18n.T("spawn.progress.success.monitoring.title")),
				"",
			}
			if ttl := launchConfig.GetTTL(); ttl != "" {
				monitoring = append(monitoring, fmt.Sprintf("   %s %s", i18n.Emoji("clock"), i18n.Tf("spawn.progress.success.monitoring.ttl", map[string]interface{}{
					"TTL": ttl,
				})))
			}
			if idle := launchConfig.GetIdleTimeout(); idle != "" {
				monitoring = append(monitoring, fmt.Sprintf("   %s %s", i18n.Emoji("zzz"), i18n.Tf("spawn.progress.success.monitoring.idle", map[string]interface{}{
					"Idle": idle,
				})))
			}
			monitoring = append(monitoring, "",
				i18n.T("spawn.progress.success.monitoring.agent_active"),
				i18n.T("spawn.progress.success.monitoring.close_laptop"),
				// Reassure the user the deadline is guaranteed even if the in-instance
				// agent fails: a server-side reaper backstops it (spore-host/spawn#70).
				// Literal (not i18n) to avoid a cross-repo libs release for one line;
				// promote to an i18n key on the next libs bump.
				fmt.Sprintf("   %s A server-side reaper enforces the deadline as a backstop, even if the agent fails.", i18n.Emoji("check")),
			)
			section(monitoring...)
		}
	}
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
		// The catalog's key is "progress"; "running" isn't one, so every running
		// step printed the "?" fallback (#1114 demo, #1124).
		return i18n.Symbol("progress")
	case "complete":
		return i18n.Symbol("success")
	case "error":
		return i18n.Symbol("error")
	case "skipped":
		return i18n.Symbol("skip") + " "
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
