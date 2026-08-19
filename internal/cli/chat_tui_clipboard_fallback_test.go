package cli

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"reasonix/internal/control"
)

func stubEmptyImageClipboard(t *testing.T, text string) {
	t.Helper()
	prevImage := readClipboardImage
	prevText := readNativeClipboardText
	t.Cleanup(func() {
		readClipboardImage = prevImage
		readNativeClipboardText = prevText
	})
	readClipboardImage = func() (string, error) { return "", control.ErrNoClipboardImage }
	readNativeClipboardText = func() (string, error) { return text, nil }
}

func imagePasteKey() tea.KeyPressMsg {
	if runtime.GOOS == "windows" {
		return tea.KeyPressMsg{Code: 'v', Mod: tea.ModAlt}
	}
	return tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl}
}

func TestCtrlVPastesTextWhenClipboardHasNoImage(t *testing.T) {
	setLocalClipboardSession(t)
	stubEmptyImageClipboard(t, "https://example.com/x")

	m := newComposerMouseTestTUI(t, 60, 16)
	m.input.SetValue("before ")

	next, cmd := m.Update(imagePasteKey())
	m = next.(chatTUI)
	if cmd == nil {
		t.Fatal("image paste shortcut produced no clipboard command")
	}
	next, cmd = m.Update(cmd())
	m = next.(chatTUI)

	if got := strings.Join(m.transcript, "\n"); strings.Contains(got, "wl-paste") {
		t.Fatalf("empty image clipboard surfaced a tooling notice:\n%s", got)
	}
	result := clipboardTextPasteResultFromCmd(t, cmd)
	next, _ = m.Update(result)
	m = next.(chatTUI)

	if got, want := m.input.Value(), "before https://example.com/x"; got != want {
		t.Fatalf("clipboard text fallback produced %q, want %q", got, want)
	}
}

func TestCtrlVDoesNotPasteTwiceWhenTerminalAlreadyPasted(t *testing.T) {
	setLocalClipboardSession(t)
	stubEmptyImageClipboard(t, "term text")

	m := newComposerMouseTestTUI(t, 60, 16)
	m.input.SetValue("before ")

	next, cmd := m.Update(imagePasteKey())
	m = next.(chatTUI)
	if cmd == nil {
		t.Fatal("image paste shortcut produced no clipboard command")
	}
	next, _ = m.Update(tea.PasteMsg{Content: "term text"})
	m = next.(chatTUI)
	if got, want := m.input.Value(), "before term text"; got != want {
		t.Fatalf("bracketed paste produced %q, want %q", got, want)
	}

	next, cmd = m.Update(cmd())
	m = next.(chatTUI)
	if cmd != nil {
		t.Fatal("fallback ran even though the terminal already pasted")
	}
	if got, want := m.input.Value(), "before term text"; got != want {
		t.Fatalf("text was pasted twice: %q, want %q", got, want)
	}
}

func TestRapidCtrlVDoesNotDropSecondTextFallback(t *testing.T) {
	setLocalClipboardSession(t)
	stubEmptyImageClipboard(t, "text")

	m := newComposerMouseTestTUI(t, 60, 16)
	next, firstImage := m.Update(imagePasteKey())
	m = next.(chatTUI)
	next, firstText := m.Update(firstImage())
	m = next.(chatTUI)

	next, secondImage := m.Update(imagePasteKey())
	m = next.(chatTUI)
	if secondImage == nil {
		t.Fatal("second image paste shortcut did not start a probe")
	}

	firstResult := clipboardTextPasteResultFromCmd(t, firstText)
	next, _ = m.Update(firstResult)
	m = next.(chatTUI)

	next, secondText := m.Update(secondImage())
	m = next.(chatTUI)
	if secondText == nil {
		t.Fatal("second text fallback was mistaken for a terminal-owned paste")
	}
	secondResult := clipboardTextPasteResultFromCmd(t, secondText)
	next, _ = m.Update(secondResult)
	m = next.(chatTUI)

	if got, want := m.input.Value(), "texttext"; got != want {
		t.Fatalf("rapid clipboard fallbacks produced %q, want %q", got, want)
	}
}

func TestClipboardImagePasteKeepsNoticeForRealFailures(t *testing.T) {
	setLocalClipboardSession(t)
	m := newComposerMouseTestTUI(t, 60, 16)
	m.input.SetValue("before ")

	next, cmd := m.Update(clipboardImageMsg{err: errors.New("clipboard image paste needs wl-paste (Wayland) or xclip (X11)")})
	m = next.(chatTUI)

	if cmd != nil {
		t.Fatal("a real clipboard failure must not trigger a text paste")
	}
	if got := strings.Join(m.transcript, "\n"); !strings.Contains(got, "wl-paste") {
		t.Fatalf("a real clipboard failure lost its notice:\n%s", got)
	}
	if got := m.input.Value(); got != "before " {
		t.Fatalf("a real clipboard failure changed the composer: %q", got)
	}
}
