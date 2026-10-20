package main

import (
	"bytes"
	"fmt"
	"log"
	"regexp"
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

var displayRoots = []Root{{Name: "use1-stable"}, {Name: "empty"}}

// withProgress records the progress of the root at index i, and shows it on
// the next frame, as the walk and the display do.
func withProgress(t *testing.T, model tea.Model, i int, progress RootProgress) tea.Model {
	t.Helper()
	model.(progressModel).latest.set(i, progress)
	updated, _ := model.Update(spinner.TickMsg{})
	return updated
}

func TestLiveViewShowsEveryRootInOrder(t *testing.T) {
	var model tea.Model = newProgressModel(displayRoots)

	model = withProgress(t, model, 0, RootProgress{Stats: Stats{Rendered: 3, Unchanged: 1}, Outstanding: 2})
	model = withProgress(t, model, 1, RootProgress{Finished: true})
	lines := strings.Split(ansi.Strip(model.View().Content), "\n")

	if len(lines) != 3 ||
		!regexp.MustCompile(`^\S use1-stable  3 rendered, 1 unchanged, 2 to go$`).MatchString(lines[0]) ||
		lines[1] != "✓ empty        0 rendered, 0 unchanged" ||
		!strings.HasPrefix(lines[2], "Rendering 2 roots: 1 done") {
		t.Errorf("Expected each root in order, finished or not, then how far the walk is, got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestLiveViewSummarizesRootsThatDoNotFit(t *testing.T) {
	roots := []Root{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}}
	model := newProgressModel(roots)

	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 4})
	view := ansi.Strip(updated.View().Content)

	// Two roots, a line about the rest, and how far the walk is fit in four
	// lines.
	if lines := strings.Split(view, "\n"); len(lines) != 4 || lines[2] != "… and 2 more" || !strings.HasPrefix(lines[3], "Rendering 4 roots") {
		t.Errorf("Expected four lines, with the number of roots that don't fit, got:\n%s", view)
	}
}

func TestLiveViewFoldsFinishedRootsThatDoNotFit(t *testing.T) {
	roots := []Root{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}}
	model := newProgressModel(roots)

	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 4})
	updated = withProgress(t, updated, 0, RootProgress{Finished: true})
	updated = withProgress(t, updated, 2, RootProgress{Finished: true})
	lines := strings.Split(ansi.Strip(updated.View().Content), "\n")

	if len(lines) != 4 || lines[0] != "✓ 2 finished" || !strings.Contains(lines[1], " b  ") || !strings.Contains(lines[2], " d  ") {
		t.Errorf("Expected the finished roots folded into a line, and the running ones, got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestLiveViewFitsTheWindowWidth(t *testing.T) {
	model := newProgressModel(displayRoots)

	updated, _ := model.Update(tea.WindowSizeMsg{Width: 20, Height: 10})

	for _, line := range strings.Split(updated.View().Content, "\n") {
		if width := ansi.StringWidth(line); width > 20 {
			t.Errorf("Expected lines to fit in 20 columns, got %d: %q", width, ansi.Strip(line))
		}
	}
}

func TestLiveDisplayPrintsEveryRootInOrderOnceDone(t *testing.T) {
	var out bytes.Buffer
	display := newLiveDisplay(&out, displayRoots, tea.WithoutSignals(), tea.WithWindowSize(120, 40))

	display.update(0, displayRoots[0], RootProgress{Outstanding: 2})
	display.update(1, displayRoots[1], RootProgress{Finished: true})
	display.update(0, displayRoots[0], RootProgress{Stats: Stats{Rendered: 2}, Finished: true})
	display.wait()

	want := "✓ use1-stable  2 rendered, 0 unchanged\n" +
		"✓ empty        0 rendered, 0 unchanged"
	if printed := ansi.Strip(out.String()); !strings.Contains(strings.ReplaceAll(printed, "\r", ""), want) {
		t.Errorf("Expected %q to be printed, got:\n%s", want, printed)
	}
}

func TestLiveDisplayPrintsRootsThatDidNotFinish(t *testing.T) {
	var out bytes.Buffer
	display := newLiveDisplay(&out, displayRoots, tea.WithoutSignals(), tea.WithWindowSize(120, 40))

	display.update(0, displayRoots[0], RootProgress{Stats: Stats{Rendered: 1}, Outstanding: 2})
	display.wait()

	if want := "✗ use1-stable  1 rendered, 0 unchanged, 2 to go"; !strings.Contains(ansi.Strip(out.String()), want) {
		t.Errorf("Expected %q to be printed, got:\n%s", want, ansi.Strip(out.String()))
	}
}

// captureOutput makes output write into the returned buffer during the test.
func captureOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := output
	output = log.New(&buf, "", 0)
	t.Cleanup(func() { output = previous })
	return &buf
}

func TestFinishedLinesPrintEachRootWhenItFinishes(t *testing.T) {
	out := captureOutput(t)
	lines := newFinishedLines(displayRoots)

	lines.update(0, displayRoots[0], RootProgress{Outstanding: 2})
	lines.update(1, displayRoots[1], RootProgress{Stats: Stats{Unchanged: 3}, Finished: true})
	lines.update(0, displayRoots[0], RootProgress{Stats: Stats{Rendered: 2}, Finished: true})
	lines.wait()

	want := fmt.Sprintf("%s\n%s\n",
		"✓ empty        0 rendered, 3 unchanged",
		"✓ use1-stable  2 rendered, 0 unchanged")
	if got := ansi.Strip(out.String()); got != want {
		t.Errorf("Expected %q, got %q", want, got)
	}
}

func TestFinishedLinesPrintNothingForOneRoot(t *testing.T) {
	out := captureOutput(t)
	lines := newFinishedLines([]Root{{Name: "only"}})

	lines.update(0, Root{Name: "only"}, RootProgress{Stats: Stats{Rendered: 1}, Finished: true})

	if out.Len() != 0 {
		t.Errorf("Expected nothing, since the summary describes the only root, got %q", out.String())
	}
}
