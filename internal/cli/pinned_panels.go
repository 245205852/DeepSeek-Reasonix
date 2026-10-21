package cli

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type pinnedPanelScroll struct {
	owner, focusText        string
	offset, width, capacity int
}

type pinnedPanelView struct {
	lines                          []string
	owner, focusText               string
	start, focus, offset, capacity int
}

func (p pinnedPanelView) height() int {
	if p.capacity == 0 {
		return 0
	}
	if len(p.lines) <= p.capacity {
		return len(p.lines)
	}
	return p.capacity + 1 // the last row describes the scrollable window
}

func (p pinnedPanelView) render(width int) string {
	if p.height() == 0 {
		return ""
	}
	end := min(p.offset+p.capacity, len(p.lines))
	content := strings.Join(p.lines[p.offset:end], "\n")
	if len(p.lines) > p.capacity {
		hint := fmt.Sprintf("↑ %d–%d/%d ↓ · Alt+PgUp/PgDn", p.offset+1, end, len(p.lines))
		content += "\n" + dim(padRight(ansi.Truncate(hint, width, ""), width))
	}
	return content
}

// All optional bottom panels share one row budget. The composer, status and
// progress rows stay outside this viewport, so a wrapped item cannot displace
// the input or footer. Retain the complete content for wheel/keyboard scrolling.
func (m chatTUI) pinnedPanelView() pinnedPanelView {
	p := m.pinnedPanelContent()
	limit := len(p.lines)
	if m.height > 0 {
		limit = max(0, m.height-m.protectedBottomRows()-minTranscriptRows)
	}
	p.capacity = min(len(p.lines), limit)
	if len(p.lines) > limit {
		p.capacity = max(0, limit-1)
	}
	maximum := max(0, len(p.lines)-p.capacity)
	p.offset = min(m.pinnedScroll.offset, maximum)
	if m.pinnedScroll.owner != p.owner || m.pinnedScroll.width != m.width {
		p.offset = min(p.start, maximum)
	}
	if m.pinnedScroll.owner != p.owner || m.pinnedScroll.focusText != p.focusText || m.pinnedScroll.width != m.width || m.pinnedScroll.capacity != p.capacity {
		if p.focus < p.offset {
			p.offset = p.focus
		} else if p.focus >= p.offset+p.capacity {
			p.offset = max(0, p.focus-p.capacity+1)
		}
	}
	p.offset = min(max(0, p.offset), maximum)
	return p
}

func (m chatTUI) pinnedPanelContent() pinnedPanelView {
	var p pinnedPanelView
	add := func(owner, content string) {
		if content == "" {
			return
		}
		rows := strings.Split(wrapTerminalText(content, max(m.width, 10)), "\n")
		focus := 0
		// The shared rowLine helper and the elicitation/todo renderers mark
		// keyboard selection or the active task at the start of a visual row.
		for i, row := range rows {
			plain := ansi.Strip(row)
			trimmed := strings.TrimLeft(plain, " ")
			indent := len(plain) - len(trimmed)
			if (indent <= 1 && (strings.HasPrefix(trimmed, "❯ ") || strings.HasPrefix(trimmed, "› "))) || (indent <= 3 && strings.HasPrefix(trimmed, "▶ ")) {
				focus = i
				break
			}
		}
		p.owner, p.start, p.focus = owner, len(p.lines), len(p.lines)+focus
		p.focusText = fmt.Sprintf("%d:%s", p.focus, ansi.Strip(rows[focus]))
		p.lines = append(p.lines, rows...)
	}
	add("todos", m.renderTodoPanel())
	add(fmt.Sprintf("approval:%p", m.pendingApproval), m.renderApprovalBanner())
	chooserKey := fmt.Sprintf("chooser:%p", m.chooser)
	if m.chooser != nil {
		chooserKey += fmt.Sprintf(":%d", m.chooser.tab)
	}
	add(chooserKey, m.renderChooser())
	add(fmt.Sprintf("elicit:%p", m.elicit), m.renderElicit())
	add(fmt.Sprintf("rewind:%p", m.rewind), m.renderRewind())
	add(fmt.Sprintf("import:%p", m.mcpImport), m.renderMCPImport())
	add(fmt.Sprintf("resume:%p", m.resumePick), m.renderResumePicker())
	add(fmt.Sprintf("quick:%p", m.quickPick), m.renderQuickPicker())
	add(fmt.Sprintf("setup:%p", m.setup), m.renderConnectionSetup())
	add(fmt.Sprintf("copy:%p", m.copyPick), m.renderCopyPicker())
	add("completion", m.renderCompletion())
	if m.nativeScrollback {
		add("manager", m.renderMainManager())
	}
	return p
}

func (m *chatTUI) syncPinnedPanelScroll() {
	p := m.pinnedPanelView()
	m.pinnedScroll = pinnedPanelScroll{owner: p.owner, focusText: p.focusText, offset: p.offset, width: m.width, capacity: p.capacity}
}

func (m *chatTUI) scrollPinnedPanel(delta int) bool {
	p := m.pinnedPanelView()
	if p.capacity == 0 || len(p.lines) <= p.capacity {
		return false
	}
	m.pinnedScroll = pinnedPanelScroll{
		owner: p.owner, focusText: p.focusText, width: m.width, capacity: p.capacity,
		offset: min(max(0, p.offset+delta), len(p.lines)-p.capacity),
	}
	return true
}

func (m *chatTUI) scrollPinnedPanelMouse(msg tea.MouseWheelMsg) bool {
	if m.nativeScrollback || msg.X < 0 || msg.X >= m.width || msg.Y < m.viewport.Height() || msg.Y >= m.viewport.Height()+m.pinnedPanelView().height() {
		return false
	}
	switch msg.Button {
	case tea.MouseWheelUp:
		return m.scrollPinnedPanel(-3)
	case tea.MouseWheelDown:
		return m.scrollPinnedPanel(3)
	}
	return false
}

func (m *chatTUI) scrollPinnedPanelKey(msg tea.KeyPressMsg) bool {
	switch msg.String() {
	case "alt+pgup":
		return m.scrollPinnedPanel(-max(1, m.pinnedPanelView().capacity-1))
	case "alt+pgdown":
		return m.scrollPinnedPanel(max(1, m.pinnedPanelView().capacity-1))
	}
	return false
}
