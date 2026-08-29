package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// loginModel is the login view as initialModel builds it, without a server.
func loginModel() model {
	email := textinput.New()
	email.Focus()
	password := textinput.New()
	password.EchoMode = textinput.EchoPassword

	// Built with textinput.New even though setup mode is off by default: a
	// zero-value textinput.Model has a nil cursor and panics on Focus, so a
	// model that could ever reach the third field has to carry a real one.
	setupToken := textinput.New()

	return model{
		view:            viewLogin,
		emailInput:      email,
		passwordInput:   password,
		setupTokenInput: setupToken,
		serverURL:       "http://localhost:8082",
		width:           80,
		height:          24,
	}
}

// namedKey covers the keys the shared key() helper does not: it builds
// tea.KeyMsgs from Type rather than from runes.
func namedKey(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

func pressKeys(t *testing.T, m model, keys ...tea.KeyMsg) model {
	t.Helper()
	for _, k := range keys {
		next, _ := m.Update(k)
		updated, ok := next.(model)
		if !ok {
			t.Fatalf("Update returned %T, want tui.model", next)
		}
		m = updated
	}
	return m
}

// Tab moves forward through the login fields and wraps.
func TestTabCyclesLoginFieldsForwards(t *testing.T) {
	m := loginModel()
	if m.loginFocus != 0 {
		t.Fatalf("focus starts at %d, want 0", m.loginFocus)
	}

	m = pressKeys(t, m, namedKey(tea.KeyTab))
	if m.loginFocus != 1 {
		t.Errorf("after tab: focus %d, want 1 (password)", m.loginFocus)
	}

	m = pressKeys(t, m, namedKey(tea.KeyTab))
	if m.loginFocus != 0 {
		t.Errorf("after two tabs: focus %d, want 0 (wrapped to email)", m.loginFocus)
	}
}

// Shift-tab moves backwards -- which, with two fields, is the same field tab
// would have reached. This passes against a cycle that only goes forward, and
// is here as the baseline rather than as the guard; the guard is the
// three-field case below.
func TestShiftTabCyclesLoginFieldsBackwards(t *testing.T) {
	m := loginModel()

	m = pressKeys(t, m, namedKey(tea.KeyShiftTab))
	if m.loginFocus != 1 {
		t.Errorf("shift+tab from email: focus %d, want 1 (wrapped back to password)", m.loginFocus)
	}

	m = pressKeys(t, m, namedKey(tea.KeyShiftTab))
	if m.loginFocus != 0 {
		t.Errorf("shift+tab from password: focus %d, want 0 (email)", m.loginFocus)
	}
}

// Setup mode adds a third field, and that is where the direction of the cycle
// stops being a matter of taste.
//
// With two fields forward and backward land on the same field, which is why the
// two tests above pass against a cycle that only ever goes forward. With three
// they are different answers, and an operator pressing shift+tab to correct the
// address they just typed lands on the setup token instead.
func TestShiftTabGoesBackwardsThroughThreeSetupFields(t *testing.T) {
	m := loginModel()
	m.setupMode = true

	m = pressKeys(t, m, namedKey(tea.KeyShiftTab))
	if m.loginFocus != 2 {
		t.Errorf("shift+tab from email: focus %d, want 2 (wrapped back to the token)", m.loginFocus)
	}

	m = pressKeys(t, m, namedKey(tea.KeyShiftTab))
	if m.loginFocus != 1 {
		t.Errorf("shift+tab from the token: focus %d, want 1 (password)", m.loginFocus)
	}
}

// And forwards still reaches all three and wraps.
func TestTabCyclesThreeSetupFields(t *testing.T) {
	m := loginModel()
	m.setupMode = true

	for i, want := range []int{1, 2, 0} {
		m = pressKeys(t, m, namedKey(tea.KeyTab))
		if m.loginFocus != want {
			t.Errorf("tab %d: focus %d, want %d", i+1, m.loginFocus, want)
		}
	}
}

// "up" is shift-tab's alias and must agree with it, or the two ways to go back
// disagree about which way back is.
func TestUpAgreesWithShiftTab(t *testing.T) {
	withUp := pressKeys(t, loginModel(), namedKey(tea.KeyUp))
	withShiftTab := pressKeys(t, loginModel(), namedKey(tea.KeyShiftTab))

	if withUp.loginFocus != withShiftTab.loginFocus {
		t.Errorf("up left focus at %d, shift+tab at %d; they must agree",
			withUp.loginFocus, withShiftTab.loginFocus)
	}
}

// The server's answer is what decides which screen this is.
func TestServerInfoPutsTheLoginScreenIntoSetupMode(t *testing.T) {
	m := pressKeys(t, loginModel())

	next, _ := m.Update(serverInfoMsg{version: "v0.5.0", setupRequired: true})
	m = next.(model)

	if !m.setupMode {
		t.Fatal("a server reporting setup_required left the screen in login mode")
	}

	screen := m.View()
	if !strings.Contains(screen, "Silo Setup") {
		t.Error("the setup screen is still titled Silo Login")
	}
	if !strings.Contains(screen, "Setup token:") {
		t.Error("the setup screen has no token field")
	}
	// The sentence that stops an operator typing a guess at credentials they
	// think already exist.
	if !strings.Contains(screen, "not signing in to one") {
		t.Errorf("the setup screen does not say the account is being created:\n%s", screen)
	}
}

// A claimed server leaves the screen alone, and shows no token field to type
// into.
func TestServerInfoLeavesAClaimedServerOnTheLoginScreen(t *testing.T) {
	next, _ := loginModel().Update(serverInfoMsg{version: "v0.5.0"})
	m := next.(model)

	if m.setupMode {
		t.Fatal("a server that is set up put the screen into setup mode")
	}
	if screen := m.View(); strings.Contains(screen, "Setup token:") {
		t.Error("the login screen is showing a setup token field")
	}
}

// The post-login server-info answer must not be able to flip a live session
// back to a setup screen, however slow it was in coming.
func TestServerInfoDoesNotFlipAScreenPastLogin(t *testing.T) {
	m := loginModel()
	m.view = viewLibraries

	next, _ := m.Update(serverInfoMsg{version: "v0.5.0", setupRequired: true})
	m = next.(model)

	if m.setupMode {
		t.Error("a late setup_required flipped a logged-in session into setup mode")
	}
	if m.view != viewLibraries {
		t.Errorf("the view moved to %q", m.view)
	}
}

// All three fields, and the complaint names the one that is new -- an operator
// who left the token out should not be told to check their password.
func TestSetupRequiresTheTokenAsWellAsTheCredentials(t *testing.T) {
	m := loginModel()
	m.setupMode = true
	m.emailInput.SetValue("me@example.com")
	m.passwordInput.SetValue("a password")

	next, cmd := m.Update(namedKey(tea.KeyEnter))
	m = next.(model)

	if cmd != nil {
		t.Error("a setup with no token still sent a request")
	}
	if !strings.Contains(m.message, "setup token") {
		t.Errorf("the message does not mention the token: %q", m.message)
	}
}

// An auto-login fires from Init before server-info can have answered, and on an
// unclaimed server it fails. The error is about an account that does not exist
// yet, so it must not still be on screen once the setup form appears.
func TestSetupModeClearsAStaleAutoLoginError(t *testing.T) {
	m := loginModel()
	next, _ := m.Update(loginDoneMsg{err: errors.New("401 Unauthorized: Invalid email or password")})
	m = next.(model)
	if m.message == "" {
		t.Fatal("the failed auto-login left no message to clear")
	}

	next, _ = m.Update(serverInfoMsg{version: "v0.5.0", setupRequired: true})
	m = next.(model)

	if m.message != "" {
		t.Errorf("the stale login error survived into setup mode: %q", m.message)
	}
}
