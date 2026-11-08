package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"golang.org/x/term"
)

// ── Terminal detection ──

func isTTY() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// ── ANSI colours ──

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
	colorBold   = "\033[1m"
	colorDim    = "\033[2m"
)

func green(s string) string  { return colorGreen + s + colorReset }
func red(s string) string    { return colorRed + s + colorReset }
func yellow(s string) string { return colorYellow + s + colorReset }
func cyan(s string) string   { return colorCyan + s + colorReset }
func bold(s string) string   { return colorBold + s + colorReset }
func dim(s string) string    { return colorDim + s + colorReset }

func plainIcon(op string, failed bool, preview bool) string {
	if failed {
		return "[FAIL]"
	}
	if preview {
		switch op {
		case "delete", "delete-replaced":
			return "[-]"
		case "update", "replace", "create-replacement":
			return "[~]"
		default:
			return "[+]"
		}
	}
	switch op {
	case "update", "replace", "create-replacement":
		return "[~]"
	default:
		return "[ok]"
	}
}

// ── Spinner ──

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type spinner struct {
	stop    chan struct{}
	stopped chan struct{}
	line    string
}

func newSpinner(line string) *spinner {
	s := &spinner{
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
		line:    line,
	}
	go s.run()
	return s
}

func (s *spinner) run() {
	defer close(s.stopped)
	i := 0
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.stop:
			fmt.Printf("\r\033[K")
			return
		case <-ticker.C:
			frame := cyan(spinnerFrames[i%len(spinnerFrames)])
			fmt.Printf("\r  %s %s", frame, s.line)
			i++
		}
	}
}

func (s *spinner) finish() {
	close(s.stop)
	<-s.stopped
}

// ── Banner ──

func printBanner() {
	if isTTY() {
		fmt.Println()
		fmt.Println(green("      ┌────────────────────┐"))
		fmt.Println(green("      │   A  N  V  I  L    │"))
		fmt.Println(green("      └──┬──────────────┬──┘"))
		fmt.Println(green("         │██████████████│"))
		fmt.Println(green("      ┌──┴──────────────┴──┐"))
		fmt.Println(green("      │ secure by default  │"))
		fmt.Println(green("     ─┴────────────────────┴─"))
	} else {
		fmt.Println()
		fmt.Println("  ANVIL — secure by default")
	}
	fmt.Println()
	if isTTY() {
		time.Sleep(150 * time.Millisecond)
	}
}

// ── Duration formatting ──

func formatDuration(d time.Duration) string {
	if d < 10*time.Second {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	mins := int(d.Minutes())
	secs := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm %ds", mins, secs)
}

// ── Resource tracking ──

type trackedResource struct {
	urn       string
	typeName  string
	name      string
	op        string
	startTime time.Time
	endTime   time.Time
	failed    bool
	// printed is set once the resource's line is shown. Pulumi reports a
	// component's outputs twice (registration and registerOutputs), and the
	// line must appear once.
	printed bool
}

// ── URN parsing ──

func parseURN(urn string) (typeName string, resourceName string) {
	parts := strings.Split(urn, "::")
	if len(parts) < 4 {
		return urn, urn
	}
	typeName = parts[2]
	resourceName = parts[3]
	return
}

func isAnvilComponent(typeToken string) bool {
	// Child resources have compound tokens: "anvil:aws:Bucket$aws:s3/bucket:Bucket"
	// Check the leaf type (after last $), not the parent prefix.
	if idx := strings.LastIndex(typeToken, "$"); idx != -1 {
		typeToken = typeToken[idx+1:]
	}
	return strings.HasPrefix(typeToken, "anvil:")
}

func isInternalResource(typeToken string, verbose bool) bool {
	if strings.HasPrefix(typeToken, "pulumi:pulumi:") {
		return true
	}
	if strings.HasPrefix(typeToken, "pulumi:providers:") {
		return true
	}
	if !verbose && !isAnvilComponent(typeToken) {
		return true
	}
	return false
}

func shortTypeName(typeToken string) string {
	// For compound tokens, use the leaf type
	if idx := strings.LastIndex(typeToken, "$"); idx != -1 {
		typeToken = typeToken[idx+1:]
	}
	parts := strings.Split(typeToken, ":")
	if len(parts) >= 3 {
		return parts[len(parts)-1]
	}
	return typeToken
}

// ── Op helpers ──

func opVerb(op string, preview bool) string {
	if preview {
		switch op {
		case "create", "create-replacement":
			return "will create"
		case "update":
			return "will update"
		case "delete", "delete-replaced":
			return "will delete"
		case "replace":
			return "will replace"
		case "same", "read":
			return ""
		default:
			return op
		}
	}
	switch op {
	case "create", "create-replacement":
		return "created"
	case "update":
		return "updated"
	case "delete", "delete-replaced":
		return "deleted"
	case "replace":
		return "replaced"
	case "same", "read":
		return ""
	default:
		return op
	}
}

func opSpinnerVerb(op string, preview bool) string {
	if preview {
		return "checking..."
	}
	switch op {
	case "create", "create-replacement":
		return "creating..."
	case "update":
		return "updating..."
	case "delete", "delete-replaced":
		return "deleting..."
	case "replace":
		return "replacing..."
	default:
		return "processing..."
	}
}

func opIcon(op string, failed bool, preview bool) string {
	if failed {
		return red("✘")
	}
	if preview {
		switch op {
		case "delete", "delete-replaced":
			return red("-")
		case "update", "replace", "create-replacement":
			return yellow("~")
		default:
			return green("+")
		}
	}
	switch op {
	case "update", "replace", "create-replacement":
		return yellow("~")
	default:
		return green("✔")
	}
}

// ── Event handler ──

type EventHandler struct {
	verbose   bool
	command   string
	resources map[string]*trackedResource
	errors    []diagError
	spinner   *spinner
	startTime time.Time

	// showNames adds each component's name to its line (anvil org: OUs and
	// accounts are only distinguishable by name).
	showNames bool

	// logFile is a full, unfiltered run log written to .anvil/<command>.log.
	// Unlike stdout (which is summarised and TTY-gated), it captures every
	// resource op and the raw diagnostic messages — including program
	// tracebacks — so failures are debuggable after the fact.
	logFile *os.File
}

type diagError struct {
	urn     string
	message string
}

func NewEventHandler(verbose bool, command string) *EventHandler {
	h := &EventHandler{
		verbose:   verbose,
		command:   command,
		resources: make(map[string]*trackedResource),
		startTime: time.Now(),
	}
	h.openLog()
	return h
}

// openLog creates .anvil/<command>.log for this run. Best-effort: if the path
// can't be resolved or created, the command proceeds without a log file.
func (h *EventHandler) openLog() {
	root, err := findProjectRoot()
	if err != nil {
		return
	}
	anvilDir := filepath.Join(root, ".anvil")
	if err := os.MkdirAll(anvilDir, 0755); err != nil {
		return
	}
	f, err := os.Create(filepath.Join(anvilDir, h.command+".log"))
	if err != nil {
		return
	}
	h.logFile = f
	fmt.Fprintf(f, "# anvil %s — %s\n\n", h.command, h.startTime.Format(time.RFC3339))
}

// logf writes a single timestamped, ANSI-free line to the run log (best-effort).
func (h *EventHandler) logf(format string, args ...interface{}) {
	if h.logFile == nil {
		return
	}
	msg := stripAnsi(fmt.Sprintf(format, args...))
	fmt.Fprintf(h.logFile, "%s  %s\n", time.Now().Format("15:04:05"), strings.TrimRight(msg, "\n"))
}

// logRaw writes a message verbatim (ANSI stripped) to the run log, preserving
// multi-line content such as program tracebacks.
func (h *EventHandler) logRaw(s string) {
	if h.logFile == nil {
		return
	}
	out := stripAnsi(s)
	fmt.Fprint(h.logFile, out)
	if !strings.HasSuffix(out, "\n") {
		fmt.Fprintln(h.logFile)
	}
}

// Close flushes and closes the run log. Safe to call multiple times.
func (h *EventHandler) Close() {
	if h.logFile != nil {
		fmt.Fprintf(h.logFile, "\n# finished — %s\n", time.Now().Format(time.RFC3339))
		h.logFile.Close()
		h.logFile = nil
	}
}

func (h *EventHandler) isPreview() bool {
	return h.command == "preview"
}

func (h *EventHandler) commandVerb() string {
	switch h.command {
	case "preview":
		return "Preview"
	case "destroy":
		return "Destroy"
	default:
		return "Deploy"
	}
}

func (h *EventHandler) rerunCmd() string {
	switch h.command {
	case "preview":
		return "anvil preview"
	case "destroy":
		return "anvil destroy --yes"
	default:
		return "anvil deploy"
	}
}

// HandleEvent processes a single engine event.
// events.EngineEvent embeds apitype.EngineEvent, so sub-event fields
// (ResourcePreEvent, ResOutputsEvent, etc.) are promoted from apitype.
func (h *EventHandler) HandleEvent(event events.EngineEvent) {
	switch {
	case event.ResourcePreEvent != nil:
		h.handleResourcePre(event.ResourcePreEvent)
	case event.ResOutputsEvent != nil:
		h.handleResOutputs(event.ResOutputsEvent)
	case event.ResOpFailedEvent != nil:
		h.handleResOpFailed(event.ResOpFailedEvent)
	case event.DiagnosticEvent != nil:
		h.handleDiagnostic(event.DiagnosticEvent)
	case event.SummaryEvent != nil:
		// We build our own summary, ignore Pulumi's.
	}
}

func (h *EventHandler) handleResourcePre(e *apitype.ResourcePreEvent) {
	meta := e.Metadata
	typeName, resourceName := parseURN(meta.URN)

	tr := &trackedResource{
		urn:       meta.URN,
		typeName:  typeName,
		name:      resourceName,
		op:        string(meta.Op),
		startTime: time.Now(),
	}
	h.resources[meta.URN] = tr

	if isInternalResource(typeName, h.verbose) {
		return
	}
	if string(meta.Op) == "same" || string(meta.Op) == "read" {
		return
	}

	displayName := h.label(typeName, resourceName)
	indent := "  "
	if h.verbose && !isAnvilComponent(typeName) {
		indent = "    "
	}

	line := fmt.Sprintf("%-14s %s", bold(displayName), dim(opSpinnerVerb(string(meta.Op), h.isPreview())))

	if isTTY() {
		if h.spinner != nil {
			h.spinner.finish()
		}
		h.spinner = newSpinner(indent + line)
	}
}

func (h *EventHandler) handleResOutputs(e *apitype.ResOutputsEvent) {
	meta := e.Metadata
	tr, ok := h.resources[meta.URN]
	if !ok {
		return
	}
	tr.endTime = time.Now()

	// Full log captures every resource (incl. internal/child), unlike stdout.
	// The raw op is included so replacements aren't mistaken for plain creates.
	if tr.op != "same" && tr.op != "read" {
		h.logf("%-10s %s :: %s [%s] (%s)", opVerb(tr.op, h.isPreview()),
			tr.typeName, tr.name, tr.op, formatDuration(tr.endTime.Sub(tr.startTime)))
	}

	if isInternalResource(tr.typeName, h.verbose) {
		return
	}
	if tr.op == "same" || tr.op == "read" || tr.printed {
		return
	}

	if isTTY() && h.spinner != nil {
		h.spinner.finish()
		h.spinner = nil
	}

	h.printResourceLine(tr)
}

func (h *EventHandler) handleResOpFailed(e *apitype.ResOpFailedEvent) {
	meta := e.Metadata
	tr, ok := h.resources[meta.URN]
	if !ok {
		return
	}
	tr.failed = true
	tr.endTime = time.Now()

	h.logf("FAILED     %s :: %s", tr.typeName, tr.name)

	if isInternalResource(tr.typeName, h.verbose) {
		return
	}

	if isTTY() && h.spinner != nil {
		h.spinner.finish()
		h.spinner = nil
	}

	h.printResourceLine(tr)
}

func (h *EventHandler) handleDiagnostic(e *apitype.DiagnosticEvent) {
	// Capture the raw, unfiltered diagnostic to the run log first. This is the
	// full message — program tracebacks, provider errors, plugin warnings — that
	// the stdout UI summarises away via mapError() or drops as noise.
	if raw := strings.TrimRight(e.Message, "\n"); strings.TrimSpace(stripAnsi(raw)) != "" {
		where := ""
		if e.URN != "" {
			_, name := parseURN(e.URN)
			where = name + ": "
		}
		h.logRaw(fmt.Sprintf("[%s] %s%s", e.Severity, where, raw))
	}

	switch e.Severity {
	case "error":
		h.errors = append(h.errors, diagError{
			urn:     e.URN,
			message: mapError(strings.TrimSpace(e.Message)),
		})
	case "warning":
		msg := strings.TrimSpace(stripAnsi(e.Message))
		if msg == "" {
			return
		}
		// Filter known Pulumi internal noise
		if strings.Contains(msg, "using pulumi-resource-") ||
			strings.Contains(msg, "from $PATH") ||
			strings.Contains(msg, "resource plugin anvil is expected") {
			return
		}
		if h.spinner != nil {
			h.spinner.finish()
			h.spinner = nil
		}
		fmt.Printf("\n  %s %s\n\n", red("⚠"), msg)
	}
}

func stripAnsi(s string) string {
	var result strings.Builder
	inEscape := false
	for _, r := range s {
		if r == '\033' {
			inEscape = true
			continue
		}
		if inEscape {
			if r == 'm' {
				inEscape = false
			}
			continue
		}
		result.WriteRune(r)
	}
	return result.String()
}

// label is a resource's display name: its type, plus its name when showNames
// is set.
func (h *EventHandler) label(typeName, name string) string {
	if h.showNames {
		return shortTypeName(typeName) + "  " + name
	}
	return shortTypeName(typeName)
}

func (h *EventHandler) printResourceLine(tr *trackedResource) {
	tr.printed = true
	displayName := h.label(tr.typeName, tr.name)
	duration := tr.endTime.Sub(tr.startTime)

	indent := "  "
	if h.verbose && !isAnvilComponent(tr.typeName) {
		indent = "    "
	}

	if tr.failed {
		if isTTY() {
			fmt.Printf("%s%s %-14s %s\n", indent, red("✘"), bold(displayName), red("failed"))
		} else {
			fmt.Printf("%s[FAIL] %-14s failed\n", indent, displayName)
		}
		return
	}

	verb := opVerb(tr.op, h.isPreview())
	timing := fmt.Sprintf("(%s)", formatDuration(duration))

	if isTTY() {
		icon := opIcon(tr.op, false, h.isPreview())
		fmt.Printf("%s%s %-14s %-10s %s\n", indent, icon, bold(displayName), verb, dim(timing))
	} else {
		icon := plainIcon(tr.op, false, h.isPreview())
		fmt.Printf("%s%s %-14s %-10s %s\n", indent, icon, displayName, verb, timing)
	}
}

func (h *EventHandler) PrintSummary(stage string) {
	// The run log is complete once we reach the summary — flush and close it,
	// whichever branch below returns.
	defer h.Close()

	totalDuration := time.Since(h.startTime)

	if h.spinner != nil {
		h.spinner.finish()
		h.spinner = nil
	}

	if isTTY() {
		time.Sleep(100 * time.Millisecond)
	}

	counts := map[string]int{}
	// Changes to resources inside a component (e.g. a tag on a DSQL cluster)
	// leave the component itself "same", so they're tallied separately.
	underlying := 0
	failed := 0
	for _, tr := range h.resources {
		if tr.op == "same" || tr.op == "read" || tr.op == "" {
			continue
		}
		if isInternalResource(tr.typeName, false) {
			if !strings.HasPrefix(tr.typeName, "pulumi:") && !tr.failed {
				underlying++
			}
			continue
		}
		if tr.failed {
			failed++
			continue
		}
		switch tr.op {
		case "create", "create-replacement":
			counts["created"]++
		case "update":
			counts["updated"]++
		case "delete", "delete-replaced":
			counts["deleted"]++
		case "replace":
			counts["replaced"]++
		}
	}

	total := 0
	for _, c := range counts {
		total += c
	}

	fmt.Println()

	if failed > 0 || len(h.errors) > 0 {
		h.printFailureSummary(stage, counts, total, failed)
		return
	}

	if total == 0 && underlying == 0 {
		fmt.Println("  No changes. Infrastructure is up to date.")
		return
	}

	summaryParts := []string{}
	if h.isPreview() {
		if c, ok := counts["created"]; ok {
			summaryParts = append(summaryParts, fmt.Sprintf("%d to create", c))
		}
		if c, ok := counts["updated"]; ok {
			summaryParts = append(summaryParts, fmt.Sprintf("%d to update", c))
		}
		if c, ok := counts["replaced"]; ok {
			summaryParts = append(summaryParts, fmt.Sprintf("%d to replace", c))
		}
		if c, ok := counts["deleted"]; ok {
			summaryParts = append(summaryParts, fmt.Sprintf("%d to delete", c))
		}
	} else {
		if c, ok := counts["created"]; ok {
			summaryParts = append(summaryParts, fmt.Sprintf("%d created", c))
		}
		if c, ok := counts["updated"]; ok {
			summaryParts = append(summaryParts, fmt.Sprintf("%d updated", c))
		}
		if c, ok := counts["replaced"]; ok {
			summaryParts = append(summaryParts, fmt.Sprintf("%d replaced", c))
		}
		if c, ok := counts["deleted"]; ok {
			summaryParts = append(summaryParts, fmt.Sprintf("%d deleted", c))
		}
	}

	if total == 0 {
		noun := "resources"
		if underlying == 1 {
			noun = "resource"
		}
		if h.isPreview() {
			summaryParts = append(summaryParts, fmt.Sprintf("%d underlying %s to change", underlying, noun))
		} else {
			summaryParts = append(summaryParts, fmt.Sprintf("%d underlying %s changed", underlying, noun))
		}
	}

	summary := strings.Join(summaryParts, ", ")

	if isTTY() {
		fmt.Printf("  %s %s complete — %s in %s\n",
			green("✔"), h.commandVerb(), summary, formatDuration(totalDuration))
	} else {
		fmt.Printf("  %s complete — %s in %s\n",
			h.commandVerb(), summary, formatDuration(totalDuration))
	}

	fmt.Println()
	switch h.command {
	case "deploy":
		if isTTY() {
			fmt.Println(dim("  Run `anvil preview` to check for drift"))
		} else {
			fmt.Println("  Run `anvil preview` to check for drift")
		}
	case "preview":
		if isTTY() {
			fmt.Println(dim("  Run `anvil deploy` to apply these changes"))
		} else {
			fmt.Println("  Run `anvil deploy` to apply these changes")
		}
	case "destroy":
		if isTTY() {
			fmt.Printf(dim("  Stage \"%s\" is now empty\n"), stage)
		} else {
			fmt.Printf("  Stage \"%s\" is now empty\n", stage)
		}
	}
}

func (h *EventHandler) printFailureSummary(stage string, counts map[string]int, total, failed int) {
	parts := []string{}
	if total > 0 {
		for verb, c := range counts {
			parts = append(parts, fmt.Sprintf("%d %s", c, verb))
		}
	}
	parts = append(parts, fmt.Sprintf("%d failed", failed))
	summary := strings.Join(parts, ", ")

	if isTTY() {
		fmt.Printf("  %s %s failed — %s\n", red("✘"), h.commandVerb(), summary)
	} else {
		fmt.Printf("  %s failed — %s\n", h.commandVerb(), summary)
	}

	if len(h.errors) > 0 {
		fmt.Println()
		if isTTY() {
			fmt.Println(red("  Errors:"))
		} else {
			fmt.Println("  Errors:")
		}
		fmt.Println()

		for _, e := range h.errors {
			displayName := ""
			if tr, ok := h.resources[e.urn]; ok && isAnvilComponent(tr.typeName) {
				displayName = fmt.Sprintf("    %s  %s", shortTypeName(tr.typeName), tr.name)
			} else if e.urn != "" {
				_, name := parseURN(e.urn)
				displayName = fmt.Sprintf("    %s", name)
			}

			if displayName != "" {
				if isTTY() {
					fmt.Println(bold(displayName))
				} else {
					fmt.Println(displayName)
				}
			}
			fmt.Printf("    %s\n\n", strings.TrimSpace(e.message))
		}

		rerun := h.rerunCmd()
		if isTTY() {
			fmt.Printf(dim("  Fix the errors above, then run `%s` again.\n"), rerun)
			if h.command == "deploy" {
				fmt.Println(dim("  Successfully created resources will not be recreated."))
			} else if h.command == "destroy" {
				fmt.Println(dim("  Successfully deleted resources will not be re-deleted."))
			}
		} else {
			fmt.Printf("  Fix the errors above, then run `%s` again.\n", rerun)
			if h.command == "deploy" {
				fmt.Println("  Successfully created resources will not be recreated.")
			} else if h.command == "destroy" {
				fmt.Println("  Successfully deleted resources will not be re-deleted.")
			}
		}
	}
}

func (h *EventHandler) HasErrors() bool {
	return len(h.errors) > 0
}
