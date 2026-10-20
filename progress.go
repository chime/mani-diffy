package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

// progressDisplay shows how far a walk is with each of its roots.
type progressDisplay interface {
	// update shows the progress of the root at index i of the roots.
	update(i int, root Root, progress RootProgress)
	// wait finishes showing the progress, once the walk is done.
	wait()
}

// newProgressDisplay returns a live display of the roots still running when f
// is a terminal, or else a line for each root when it finishes. With verbose
// output, the lines of the progress of every application would garble the
// live display, so it isn't used.
func newProgressDisplay(f *os.File, roots []Root, verbose bool) progressDisplay {
	if !verbose && term.IsTerminal(int(f.Fd())) {
		return newLiveDisplay(f, roots)
	}
	return newFinishedLines(roots)
}

var (
	faintStyle   = lipgloss.NewStyle().Faint(true)
	nameStyle    = lipgloss.NewStyle().Bold(true)
	spinnerStyle = lipgloss.NewStyle().Foreground(lipgloss.Cyan)
	finishedMark = lipgloss.NewStyle().Foreground(lipgloss.Green).Render("✓")
	failedMark   = lipgloss.NewStyle().Foreground(lipgloss.Red).Render("✗")
	canceledMark = lipgloss.NewStyle().Faint(true).Render("-")
)

// rootLine describes a root's progress on a line, with the root's name padded
// to the width of the longest name. running marks a root that is still
// running. Once the walk is done, the applications of the roots still running
// were canceled.
func rootLine(running, name string, nameWidth int, progress RootProgress, walkDone bool) string {
	mark := running
	switch {
	case progress.Failed:
		mark = failedMark
	case progress.Finished:
		mark = finishedMark
	case walkDone:
		mark = canceledMark
	}
	return fmt.Sprintf("%s %s  %s", mark, nameStyle.Render(fmt.Sprintf("%-*s", nameWidth, name)), describeProgress(progress, walkDone))
}

// describeProgress describes how far a walk is with the applications of a
// root.
func describeProgress(progress RootProgress, walkDone bool) string {
	switch {
	case progress.Failed:
		return describe(progress.Stats) + ", failed"
	case progress.Finished:
		return describe(progress.Stats)
	case walkDone:
		return fmt.Sprintf("%s, %d canceled", describe(progress.Stats), progress.Outstanding)
	default:
		return fmt.Sprintf("%s, %d to go", describe(progress.Stats), progress.Outstanding)
	}
}

func longestName(roots []Root) int {
	width := 0
	for _, root := range roots {
		width = max(width, len(root.Name))
	}
	return width
}

// liveDisplay shows the progress of every root below the output, updating in
// place, like `docker build` does for its steps. Once the walk is done, it
// clears the display, and prints what was done with every root instead.
type liveDisplay struct {
	program *tea.Program
	latest  *latestProgress
	roots   []Root
	out     io.Writer
	done    chan struct{}
}

// clearingTime is long enough for the display to draw a frame.
const clearingTime = 50 * time.Millisecond

// latestProgress is the latest progress of every root. The walk records it
// without waiting for the display, which shows a copy of it on every frame.
type latestProgress struct {
	mu       sync.Mutex
	progress []RootProgress
}

func (l *latestProgress) set(i int, progress RootProgress) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.progress[i] = progress
}

func (l *latestProgress) copy() []RootProgress {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.progress)
}

func newLiveDisplay(w io.Writer, roots []Root, options ...tea.ProgramOption) *liveDisplay {
	model := newProgressModel(roots)
	d := &liveDisplay{
		// Without input, interrupting stops mani-diffy as it does without the
		// display, after the display restores the terminal.
		program: tea.NewProgram(model, append([]tea.ProgramOption{tea.WithOutput(w), tea.WithInput(nil)}, options...)...),
		latest:  model.latest,
		roots:   roots,
		out:     w,
		done:    make(chan struct{}),
	}
	go func() {
		defer close(d.done)
		if _, err := d.program.Run(); errors.Is(err, tea.ErrInterrupted) {
			os.Exit(130)
		}
	}()
	return d
}

func (d *liveDisplay) update(i int, _ Root, progress RootProgress) {
	d.latest.set(i, progress)
}

func (d *liveDisplay) wait() {
	d.program.Send(walkDoneMsg{})
	<-d.done
	nameWidth := longestName(d.roots)
	for i, progress := range d.latest.copy() {
		fmt.Fprintln(d.out, rootLine("", d.roots[i].Name, nameWidth, progress, true))
	}
}

// walkDoneMsg tells the display that the walk is done.
type walkDoneMsg struct{}

type progressModel struct {
	roots     []Root
	nameWidth int
	progress  []RootProgress
	// latest is where the progress is copied from on every frame.
	latest  *latestProgress
	spinner spinner.Model
	start   time.Time
	// width and height are the size of the terminal, once known.
	width, height int
	done          bool
}

// dot is the dot spinner without the space after each frame, which the lines
// that show it have already.
var dot = spinner.Spinner{Frames: trimFrames(spinner.Dot.Frames), FPS: spinner.Dot.FPS}

func trimFrames(frames []string) []string {
	trimmed := make([]string, len(frames))
	for i, frame := range frames {
		trimmed[i] = strings.TrimSpace(frame)
	}
	return trimmed
}

func newProgressModel(roots []Root) progressModel {
	return progressModel{
		roots:     roots,
		nameWidth: longestName(roots),
		progress:  make([]RootProgress, len(roots)),
		latest:    &latestProgress{progress: make([]RootProgress, len(roots))},
		spinner:   spinner.New(spinner.WithSpinner(dot), spinner.WithStyle(spinnerStyle)),
		start:     time.Now(),
	}
}

func (m progressModel) Init() tea.Cmd {
	return m.spinner.Tick
}

func (m progressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case walkDoneMsg:
		// The display is cleared on the next frame, after which the program
		// quits, and the roots are printed once it did. Quitting right away
		// would leave the last frame on the screen.
		m.done = true
		return m, tea.Tick(clearingTime, func(time.Time) tea.Msg { return tea.Quit() })
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case spinner.TickMsg:
		m.copyLatest()
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *progressModel) copyLatest() {
	m.progress = m.latest.copy()
}

func (m progressModel) View() tea.View {
	if m.done {
		return tea.NewView("")
	}

	var finished, running []int
	for i, progress := range m.progress {
		if progress.Finished {
			finished = append(finished, i)
		} else {
			running = append(running, i)
		}
	}
	var lines []string

	// The roots are shown in order when they fit above the line about how
	// far the walk is. Otherwise, the finished ones are folded into a line,
	// and then the running ones that don't fit either.
	fits := m.height == 0 || len(m.roots) <= m.height-1
	if fits {
		for i := range m.roots {
			lines = append(lines, m.rootLine(i))
		}
	} else {
		room := m.height - 1
		if len(finished) > 0 {
			lines = append(lines, finishedMark+fmt.Sprintf(" %d finished", len(finished)))
			room--
		}
		shown := running
		if len(running) > room {
			shown = running[:max(room-1, 0)]
		}
		for _, i := range shown {
			lines = append(lines, m.rootLine(i))
		}
		if len(shown) < len(running) {
			lines = append(lines, faintStyle.Render(fmt.Sprintf("… and %d more", len(running)-len(shown))))
		}
	}
	lines = append(lines, faintStyle.Render(fmt.Sprintf("Rendering %d roots: %d done, %v",
		len(m.roots), len(finished), time.Since(m.start).Round(100*time.Millisecond))))

	if m.width > 0 {
		for i, line := range lines {
			lines[i] = ansi.Truncate(line, m.width, "…")
		}
	}
	return tea.NewView(strings.Join(lines, "\n"))
}

// rootLine describes the progress of the root at index i, with a spinner while
// it runs.
func (m progressModel) rootLine(i int) string {
	return rootLine(m.spinner.View(), m.roots[i].Name, m.nameWidth, m.progress[i], false)
}

// finishedLines print a line for each root when it finishes or fails, when
// there are several roots. With one root, the summary describes it.
type finishedLines struct {
	several   bool
	nameWidth int
	printed   []bool
}

func newFinishedLines(roots []Root) *finishedLines {
	return &finishedLines{several: len(roots) > 1, nameWidth: longestName(roots), printed: make([]bool, len(roots))}
}

func (l *finishedLines) update(i int, root Root, progress RootProgress) {
	if l.several && !l.printed[i] && (progress.Finished || progress.Failed) {
		l.printed[i] = true
		output.Println(rootLine("", root.Name, l.nameWidth, progress, false))
	}
}

func (*finishedLines) wait() {}
