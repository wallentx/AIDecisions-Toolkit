package installer

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"

	"aid/internal/catalog"
	"golang.org/x/term"
)

type key int

const (
	keyOther key = iota
	keyUp
	keyDown
	keyLeft
	keyRight
	keySpace
	keyEnter
	keyEscape
	keyUpdate
	keyHome
	keyEnd
	keyCancel
	keyYes
	keyNo
)

func readKeys(ctx context.Context, input io.Reader) <-chan byte {
	keys := make(chan byte)
	go func() {
		defer close(keys)
		buffer := make([]byte, 1)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if _, err := io.ReadFull(input, buffer); err != nil {
				return
			}
			select {
			case keys <- buffer[0]:
			case <-ctx.Done():
				return
			}
		}
	}()
	return keys
}

func nextKey(ctx context.Context, keys <-chan byte) key {
	var b byte
	select {
	case <-ctx.Done():
		return keyCancel
	case value, open := <-keys:
		if !open {
			return keyCancel
		}
		b = value
	}
	switch b {
	case 3, 4, 'q':
		return keyCancel
	case 'y', 'Y':
		return keyYes
	case 'n', 'N':
		return keyNo
	case 'k':
		return keyUp
	case 'j':
		return keyDown
	case 'h':
		return keyLeft
	case 'l', '\t':
		return keyRight
	case ' ':
		return keySpace
	case '\n', '\r':
		return keyEnter
	case 'u', 'U':
		return keyUpdate
	case 27:
		// Escape is also the prefix of arrow sequences; a lone Escape cancels.
		timer := time.NewTimer(60 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return keyEscape
		case <-timer.C:
			return keyEscape
		case next := <-keys:
			if next != '[' && next != 'O' {
				return keyEscape
			}
		}
		select {
		case <-ctx.Done():
			return keyEscape
		case <-timer.C:
			return keyEscape
		case final := <-keys:
			switch final {
			case 'A':
				return keyUp
			case 'B':
				return keyDown
			case 'C':
				return keyRight
			case 'D':
				return keyLeft
			case 'H':
				return keyHome
			case 'F':
				return keyEnd
			}
		}
	}
	return keyOther
}

type screen struct {
	Providers []provider
	Selected  map[string]bool
	Cursor    int
	Review    bool
	Apply     bool
	Updating  bool
	Message   string
	Plan      plan
}

func (s *screen) desired() map[string]bool {
	result := make(map[string]bool, len(s.Selected))
	for _, p := range s.Providers {
		checked := s.Selected[p.ID]
		if !s.Updating || (checked && p.Installed) {
			result[p.ID] = checked
		}
	}
	return result
}

func (s *screen) render(out io.Writer, width, height int) bool {
	if width < 4 {
		width = 4
	}
	if height < 1 {
		height = 1
	}
	if width > 100 {
		width = 100
	}
	var lines []string
	line := func(text string) {
		lines = append(lines, text)
	}
	line("AID  /  Agent installation")
	line(strings.Repeat("-", width-1))
	fits := true
	if !s.Review {
		line("Select clients that should have AID skills and MCP.")
		line("Unchecked installed clients: remove.")
		line("")
		visible := max(1, height-12)
		start := max(0, min(s.Cursor-visible+1, len(s.Providers)-visible))
		for i := start; i < min(len(s.Providers), start+visible); i++ {
			p := s.Providers[i]
			cursor, check, status := " ", " ", "not detected"
			if i == s.Cursor {
				cursor = ">"
			}
			if s.Selected[p.ID] {
				check = "x"
			}
			if p.Detected {
				status = "detected"
			}
			if p.Installed {
				status = "installed"
			}
			text := fmt.Sprintf("%s [%s] %-15s %s", cursor, check, p.Name, status)
			line(text)
		}
		line("")
		line(shorten(s.Providers[s.Cursor].Root, width-1))
		line("")
		line("Up/Down or j/k: move  Space: toggle")
		line("Enter: review  u: update checked")
		line("Esc / Ctrl-C: cancel")
	} else {
		line("Review installation changes")
		line("")
		counts := map[string]int{}
		for _, op := range s.Plan.Operations {
			counts[op.Action]++
		}
		line(fmt.Sprintf("Install: %d  Update: %d  Remove: %d", counts["install"], counts["update"], counts["remove"]))
		for _, op := range s.Plan.Operations {
			for _, part := range wrap("  "+op.Action+": "+op.Label, width-1) {
				line(part)
			}
		}
		if len(s.Plan.Operations) == 0 {
			line("Skills and MCP already match the selection.")
		}
		line("")
		if s.Apply {
			line("  [ Cancel ]    >[ Apply ]<")
		} else {
			line(">[ Cancel ]<    [ Apply ]")
		}
		line("")
		line("Left/Right or Tab: choose  Enter: confirm")
		line("y: apply  n/Esc: back  q: cancel")
		fits = width >= 32 && len(lines) <= height
		if !fits {
			lines = []string{"AID / Review changes", "Resize to show all changes.", "Then press any key.", "Esc: back  q: cancel"}
		}
	}
	if s.Message != "" {
		line("")
		line(shorten(s.Message, width-1))
	}
	fmt.Fprint(out, "\x1b[H\x1b[2J")
	color := os.Getenv("NO_COLOR") == ""
	for i, text := range lines[:min(len(lines), height)] {
		text = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, text)
		runes := []rune(text)
		if len(runes) > width-1 {
			text = string(runes[:width-1])
		}
		if i != 0 {
			fmt.Fprint(out, "\r\n")
		}
		if color && (i == 0 || strings.HasPrefix(text, ">")) {
			text = "\x1b[1;36m" + text + "\x1b[0m"
		}
		fmt.Fprint(out, text)
	}
	return fits
}

func wrap(value string, width int) []string {
	var lines []string
	var current string
	for _, word := range strings.Fields(value) {
		if current != "" && len([]rune(current+" "+word)) > width {
			lines = append(lines, current)
			current = ""
		}
		if current != "" {
			current += " "
		}
		current += word
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

func shorten(value string, width int) string {
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	return "..." + string(runes[len(runes)-(width-3):])
}

func runTUI(stdin, stdout *os.File, ps []provider, tools []catalog.Tool, stateFile, executableDir string) (plan, bool, error) {
	previous, err := term.MakeRaw(int(stdin.Fd()))
	if err != nil {
		return plan{}, false, fmt.Errorf("enter terminal raw mode: %w", err)
	}
	defer term.Restore(int(stdin.Fd()), previous)
	restoreOutput, err := enableANSI(stdout)
	if err != nil {
		return plan{}, false, err
	}
	defer restoreOutput()
	fmt.Fprint(stdout, "\x1b[?1049h\x1b[?25l")
	defer fmt.Fprint(stdout, "\x1b[0m\x1b[?25h\x1b[?1049l")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	keys := readKeys(ctx, stdin)
	s := screen{Providers: ps, Selected: map[string]bool{}}
	for _, p := range ps {
		s.Selected[p.ID] = p.Installed
	}
	for {
		width, height, err := term.GetSize(int(stdout.Fd()))
		if err != nil {
			width, height = 80, 24
		}
		fits := s.render(stdout, width, height)
		k := nextKey(ctx, keys)
		if ctx.Err() != nil || k == keyCancel {
			return plan{}, false, nil
		}
		if s.Review {
			switch k {
			case keyLeft, keyRight, keySpace:
				if fits {
					s.Apply = !s.Apply
				}
			case keyEscape, keyNo:
				s.Review = false
			case keyEnter:
				if fits {
					if s.Apply {
						return s.Plan, true, nil
					}
					s.Review = false
				}
			case keyYes:
				if fits {
					return s.Plan, true, nil
				}
			}
			continue
		}
		switch k {
		case keyEscape:
			return plan{}, false, nil
		case keyUp:
			s.Cursor = (s.Cursor + len(ps) - 1) % len(ps)
		case keyDown:
			s.Cursor = (s.Cursor + 1) % len(ps)
		case keyHome:
			s.Cursor = 0
		case keyEnd:
			s.Cursor = len(ps) - 1
		case keySpace:
			id := ps[s.Cursor].ID
			s.Selected[id] = !s.Selected[id]
		case keyUpdate, keyEnter:
			s.Updating = k == keyUpdate
			p, err := makePlan(ps, tools, s.desired(), stateFile, executableDir)
			if err != nil {
				s.Message = err.Error()
				continue
			}
			s.Plan, s.Message, s.Review, s.Apply = p, "", true, false
		}
	}
}
