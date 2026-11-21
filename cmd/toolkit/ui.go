package main

// Everything the toolkit shows: styles, the progress spinner, prompts, tables
// and summaries. Results go to stdout; progress, warnings and errors go to
// stderr, so stdout stays clean when piped.

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/muesli/termenv"

	"toolkit/internal/runpod"
	"toolkit/internal/workshop"
)

// terminal is true when a person is at the keyboard: only then does the
// toolkit animate progress or ask questions.
var terminal = isTerminal(os.Stdin) && isTerminal(os.Stdout)

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// Styles

// The styles only use the 16 ANSI colors, which every terminal theme tunes to
// its own background, so they read well in light and dark terminals. lipgloss
// drops colors when NO_COLOR is set or the output is not a terminal.
var (
	boldStyle    = lipgloss.NewStyle().Bold(true)
	faintStyle   = lipgloss.NewStyle().Faint(true)
	nameStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	costStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	successStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	warningStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	errorStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
)

func formatPrice(perHour float64) string {
	return costStyle.Render(fmt.Sprintf("$%.2f/h", perHour))
}

// formatCost is for estimates, hence the "~".
func formatCost(amount float64) string {
	return costStyle.Render(fmt.Sprintf("~$%.2f", amount))
}

func printWarning(format string, args ...any) {
	fmt.Fprintln(os.Stderr, warningStyle.Render("! "+fmt.Sprintf(format, args...)))
}

func printError(err error) {
	fmt.Fprintln(os.Stderr, errorStyle.Render("✗ ")+err.Error())
}

// printFields prints aligned "label  value" lines, the layout of every summary.
func printFields(fields [][2]string) {
	label := faintStyle.Width(11)
	for _, field := range fields {
		fmt.Println("  " + label.Render(field[0]) + field[1])
	}
}

// Progress

// progress shows what a long operation is doing: on a terminal, a spinner with
// the current phase and the elapsed time; otherwise one plain line per phase.
// Notices and debug lines are printed above the spinner.
type progress struct {
	debug     bool
	program   *tea.Program // nil when not on a terminal
	done      chan struct{}
	lastPhase string
}

func startProgress(phase string, debug bool) *progress {
	p := &progress{debug: debug}
	if !terminal {
		p.Phase(phase)
		return p
	}
	p.program = tea.NewProgram(
		spinnerModel{spinner: spinner.New(spinner.WithSpinner(spinner.Dot)), phase: phase, started: time.Now()},
		tea.WithOutput(os.Stderr),
		// No input: the spinner reads no keys, and Ctrl-C stays a signal that
		// cancels the operation's context.
		tea.WithInput(nil),
	)
	p.done = make(chan struct{})
	go func() {
		p.program.Run()
		close(p.done)
	}()
	return p
}

func (p *progress) Phase(phase string) {
	if p.program != nil {
		p.program.Send(phaseMsg(phase))
		return
	}
	if phase != p.lastPhase {
		fmt.Fprintln(os.Stderr, phase+"...")
		p.lastPhase = phase
	}
}

func (p *progress) Notice(text string) {
	p.println(warningStyle.Render("! " + text))
}

func (p *progress) Debug(text string) {
	if !p.debug {
		return
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = faintStyle.Render("[debug] " + line)
	}
	p.println(strings.Join(lines, "\n"))
}

// Report shows an event from the workshop package.
func (p *progress) Report(event workshop.Event) {
	switch {
	case event.Phase != "":
		p.Phase(event.Phase)
	case event.Notice != "":
		p.Notice(event.Notice)
	case event.Debug != "":
		p.Debug(event.Debug)
	}
}

func (p *progress) println(text string) {
	if p.program != nil {
		p.program.Println(text)
		return
	}
	fmt.Fprintln(os.Stderr, text)
}

// Stop removes the spinner. It must be called before printing anything else.
func (p *progress) Stop() {
	if p.program == nil {
		return
	}
	p.program.Send(stopMsg{})
	<-p.done
}

type phaseMsg string
type stopMsg struct{}

type spinnerModel struct {
	spinner spinner.Model
	phase   string
	started time.Time
	stopped bool
}

func (m spinnerModel) Init() tea.Cmd {
	return m.spinner.Tick
}

func (m spinnerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case phaseMsg:
		m.phase = string(msg)
		return m, nil
	case stopMsg:
		m.stopped = true
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.spinner, cmd = m.spinner.Update(msg)
	return m, cmd
}

func (m spinnerModel) View() string {
	if m.stopped {
		return "" // leave no spinner line behind
	}
	elapsed := time.Since(m.started).Truncate(time.Second)
	// The spinner frames already end with a space.
	return m.spinner.View() + m.phase + faintStyle.Render(" · "+elapsed.String())
}

// Prompts

var errCancelled = errors.New("cancelled")

// Esc cancels a prompt as well as Ctrl-C.
var promptKeys = func() *huh.KeyMap {
	keys := huh.NewDefaultKeyMap()
	keys.Quit = key.NewBinding(key.WithKeys("ctrl+c", "esc"))
	return keys
}()

func runPrompt(field huh.Field) error {
	err := huh.NewForm(huh.NewGroup(field)).WithTheme(promptTheme()).WithKeyMap(promptKeys).Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return errCancelled
	}
	return err
}

// promptTheme is huh's 16-color theme with the parts that depend on the
// palette fixed: ThemeBase16 draws the focused button in color 7 on color 5,
// which some palettes make unreadable, and options in color 7, which is too
// pale on a light background. Those use the terminal's own colors instead.
func promptTheme() *huh.Theme {
	theme := huh.ThemeBase16()
	// The key help line otherwise uses fixed true-color grays.
	theme.Help.ShortKey = faintStyle
	theme.Help.ShortDesc = faintStyle
	theme.Help.ShortSeparator = faintStyle
	theme.Help.FullKey = faintStyle
	theme.Help.FullDesc = faintStyle
	theme.Help.FullSeparator = faintStyle
	theme.Help.Ellipsis = faintStyle
	for _, styles := range []*huh.FieldStyles{&theme.Focused, &theme.Blurred} {
		styles.Option = styles.Option.UnsetForeground()
		styles.UnselectedOption = styles.UnselectedOption.UnsetForeground()
		styles.FocusedButton = styles.FocusedButton.UnsetForeground().UnsetBackground().Reverse(true).Bold(true)
		styles.BlurredButton = styles.BlurredButton.UnsetForeground().UnsetBackground()

		// Without colors (NO_COLOR), lipgloss drops reverse video too, and the
		// focused button would look like the other one: frame it instead.
		if lipgloss.ColorProfile() == termenv.Ascii {
			styles.FocusedButton = styles.FocusedButton.Border(lipgloss.NormalBorder())
			styles.BlurredButton = styles.BlurredButton.Border(lipgloss.HiddenBorder())
		}
	}
	return theme
}

// selectGPU shows the GPUs as aligned rows. GPUs above maxPrice get an extra
// column saying by how much, so the higher price can't be missed.
func selectGPU(title string, gpus []runpod.GPUType, cloud string, maxPrice float64) (runpod.GPUType, error) {
	rows := gpuRows(gpus, cloud)
	for i, gpu := range gpus {
		if over := workshop.Price(gpu, cloud) - maxPrice; over > 0 {
			rows[i] = append(rows[i], fmt.Sprintf("+$%.2f/h over your max", over))
		}
	}
	var widths []int // per column; rows above the max price have one more
	for _, row := range rows {
		for i, cell := range row {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], len(cell))
		}
	}
	options := make([]huh.Option[int], len(gpus))
	for i, row := range rows {
		cells := make([]string, len(row))
		for j, cell := range row {
			cells[j] = fmt.Sprintf("%-*s", widths[j], cell)
		}
		options[i] = huh.NewOption(strings.Join(cells, "   "), i)
	}

	var chosen int
	if err := runPrompt(huh.NewSelect[int]().Title(title).Options(options...).Value(&chosen)); err != nil {
		return runpod.GPUType{}, err
	}
	return gpus[chosen], nil
}

func confirmDelete(info workshop.Info) (bool, error) {
	confirmed := false // the default answer is "no"
	err := runPrompt(huh.NewConfirm().
		Title(fmt.Sprintf("Delete workshop %q?", info.Name)).
		Description(fmt.Sprintf("Everything on the machine will be lost.\nRunning for %s, est. ~$%.2f so far.",
			workshop.FormatDuration(info.Elapsed()), info.EstimatedCost())).
		Affirmative("Delete").
		Negative("Keep").
		Value(&confirmed))
	return confirmed, err
}

// Tables

// newTable is the toolkit's table look: no frame, a rule under the header.
func newTable(headers ...string) *table.Table {
	return table.New().
		Border(lipgloss.NormalBorder()).
		BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).
		BorderColumn(false).
		BorderStyle(faintStyle).
		Headers(headers...).
		StyleFunc(func(row, col int) lipgloss.Style {
			style := lipgloss.NewStyle().PaddingRight(3)
			if row == table.HeaderRow {
				return style.Inherit(boldStyle)
			}
			return style
		})
}

// gpuRows are the cells describing each GPU, shared by the picker and tables.
func gpuRows(gpus []runpod.GPUType, cloud string) [][]string {
	rows := make([][]string, len(gpus))
	for i, gpu := range gpus {
		rows[i] = []string{
			gpu.Name,
			fmt.Sprintf("%d GB", gpu.Memory),
			fmt.Sprintf("$%.2f/h", workshop.Price(gpu, cloud)),
			strings.ToLower(gpu.Availability) + " availability",
		}
	}
	return rows
}

// printGPUSuggestions is the non-interactive version of the picker: a table
// with the exact --gpu value for each GPU.
func printGPUSuggestions(gpus []runpod.GPUType, cloud string, limit int) {
	gpus = gpus[:min(limit, len(gpus))]
	t := newTable("GPU", "VRAM", "PRICE", "AVAILABILITY", "USE")
	for i, row := range gpuRows(gpus, cloud) {
		t.Row(append(row, fmt.Sprintf("--gpu %q", gpus[i].Name))...)
	}
	fmt.Fprintln(os.Stderr, t.String())
}

func printWorkshops(workshops []workshop.Info, gpuNames map[string]string) {
	t := newTable("NAME", "STATUS", "GPU", "ELAPSED", "EST. COST", "BUDGET")
	for _, w := range workshops {
		gpu := "-"
		if w.Pod.GPU != nil {
			gpu = w.Pod.GPU.ID
			if name, found := gpuNames[gpu]; found {
				gpu = name
			}
		}
		t.Row(
			nameStyle.Render(w.Name),
			statusStyle(w.Pod.Status).Render(w.Pod.Status),
			gpu,
			workshop.FormatDuration(w.Elapsed()),
			formatCost(w.EstimatedCost()),
			fmt.Sprintf("%s · deleted at %s", w.Budget, w.Deadline.Format("15:04")),
		)
	}
	fmt.Println(t.String())
}

func statusStyle(status string) lipgloss.Style {
	switch status {
	case "RUNNING":
		return successStyle
	case "PROVISIONING", "STARTING":
		return warningStyle
	default:
		return errorStyle
	}
}

// Summaries

func printOpenSummary(info workshop.Info, gpu runpod.GPUType, cloud string) {
	budgetTime := info.Deadline.Sub(info.Created)
	fmt.Println()
	printFields([][2]string{
		{"Workshop", nameStyle.Render(info.Name)},
		{"GPU", fmt.Sprintf("%s · %d GB · %s cloud", gpu.Name, gpu.Memory, strings.ToLower(cloud))},
		{"Price", formatPrice(info.Pod.Cost)},
		{"Budget", fmt.Sprintf("%s · deleted at %s (est. %s)", info.Budget, info.Deadline.Format("15:04"), formatCost(info.Pod.Cost*budgetTime.Hours()))},
		{"Idle", fmt.Sprintf("deleted after %s with no SSH session and GPU at rest", workshop.FormatDuration(info.IdleTimeout))},
	})
	fmt.Println()
}

// printDeletedSummary is shown once a workshop is gone, whoever deleted it.
func printDeletedSummary(headline string, info workshop.Info) {
	duration := info.Elapsed()
	fmt.Println(successStyle.Render("✓ ") + headline)
	printFields([][2]string{
		{"Duration", workshop.FormatDuration(duration)},
		{"Est. cost", fmt.Sprintf("%s (%s)", formatCost(info.Pod.Cost*duration.Hours()), formatPrice(info.Pod.Cost))},
	})
}

func printDisconnectedSummary(info workshop.Info, checkErr error) {
	if checkErr != nil {
		printWarning("Disconnected from workshop %q, but could not check whether it still exists: %v", info.Name, checkErr)
	} else {
		fmt.Printf("Disconnected from workshop %s. It keeps running.\n", nameStyle.Render(info.Name))
	}
	printFields([][2]string{
		{"Reconnect", "toolkit workshop connect " + info.Name},
		{"Close", "toolkit workshop close " + info.Name},
		{"Idle", fmt.Sprintf("deleted after %s if nothing runs on the GPU", workshop.FormatDuration(info.IdleTimeout))},
	})
}
