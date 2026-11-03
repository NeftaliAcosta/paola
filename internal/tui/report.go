package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type reportModel struct {
	viewport      viewport.Model
	title         string
	width         int
	height        int
	action        string
	allowFileView bool
}

// ShowReport renders long history output inside a full-screen, scrollable
// viewer so it never leaks into the terminal scrollback.
func ShowReport(title, content string) (string, error) {
	return showReport(title, content, true)
}

// ShowDiffReport renders a single file diff without the report-level file navigation shortcut.
func ShowDiffReport(title, content string) (string, error) {
	return showReport(title, content, false)
}

func showReport(title, content string, allowFileView bool) (string, error) {
	m := reportModel{title: title, width: 100, height: 30, action: "back", allowFileView: allowFileView, viewport: viewport.New(90, 22)}
	m.viewport.SetContent(colorizeReport(content))
	finalModel, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	if err != nil {
		return m.action, err
	}
	if result, ok := finalModel.(reportModel); ok {
		return result.action, nil
	}
	return m.action, nil
}

func colorizeReport(content string) string {
	section := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	good := lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	bad := lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	active := lipgloss.NewStyle().Foreground(lipgloss.Color("212"))
	info := lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	var out []string
	for _, line := range strings.Split(content, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "), strings.HasPrefix(line, "@@ "), strings.HasPrefix(line, "+++ "), strings.HasPrefix(line, "--- "):
			out = append(out, info.Render(line))
		case strings.HasPrefix(line, "+"):
			out = append(out, good.Render(line))
		case strings.HasPrefix(line, "-"):
			out = append(out, bad.Render(line))
		case strings.HasSuffix(line, "SUMMARY"), line == "TIMELINE", line == "PARAMETERS":
			out = append(out, section.Render(line))
		case strings.HasPrefix(line, "✓") || strings.Contains(line, "completed"):
			out = append(out, good.Render(line))
		case strings.HasPrefix(line, "✗") || strings.Contains(line, "FAILED") || strings.Contains(line, "Error:"):
			out = append(out, bad.Render(line))
		case strings.HasPrefix(line, "●"):
			out = append(out, active.Render(line))
		case strings.HasPrefix(line, "○") || strings.Contains(line, "not recorded"):
			out = append(out, muted.Render(line))
		case strings.HasPrefix(line, "     Files:") || strings.HasPrefix(line, "     Attempts"):
			out = append(out, info.Render(line))
		default:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func (m reportModel) Init() tea.Cmd { return nil }
func (m reportModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = v.Width, v.Height
		m.viewport.Width = max(20, v.Width-6)
		m.viewport.Height = max(6, v.Height-6)
	case tea.KeyMsg:
		switch v.String() {
		case "v", "V":
			if !m.allowFileView {
				break
			}
			m.action = "view_files"
			return m, tea.Quit
		case "q", "esc", "enter", "ctrl+c":
			m.action = "back"
			return m, tea.Quit
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	default:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}
	return m, nil
}
func (m reportModel) View() string {
	header := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Render("PAOLA · HISTORY · " + m.title)
	body := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1).Width(max(20, m.width-4)).Render(m.viewport.View())
	footerText := "↑/↓ scroll · PageUp/PageDown · mouse wheel · [q/Esc] back"
	if m.allowFileView {
		footerText = "↑/↓ scroll · PageUp/PageDown · mouse wheel · [v] view files · [q/Esc] back"
	}
	footer := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render(footerText)
	return fmt.Sprintf("%s\n\n%s\n\n%s", header, body, footer)
}
