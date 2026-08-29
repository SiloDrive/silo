package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
)

// signedIn is the library list as it stands a moment after a login, built by
// initialModel so that a change to how a field is configured reaches these
// tests rather than leaving them passing against a shape nothing ships.
func signedIn(t *testing.T) model {
	t.Helper()
	m := initialModel("http://localhost:8082", "someone@example.com", "")
	m.view = viewLibraries
	m.width, m.height = 80, 24
	return m
}

// The route in: Libraries is still the landing screen, and Account is a
// keystroke off it rather than a menu in front of it.
func TestAccountIsReachedFromTheLibraryList(t *testing.T) {
	m := press(t, signedIn(t), "a")
	if m.view != viewAccount {
		t.Fatalf("after 'a': view %q, want %q", m.view, viewAccount)
	}

	m = press(t, m, "esc")
	if m.view != viewLibraries {
		t.Errorf("after esc: view %q, want %q", m.view, viewLibraries)
	}
}

func TestAccountOpensTheChangePasswordForm(t *testing.T) {
	m := press(t, signedIn(t), "a", "enter")
	if m.view != viewPassword {
		t.Fatalf("after enter on %q: view %q, want %q",
			accountItems[0].label, m.view, viewPassword)
	}
	if m.passwordFocus != 0 {
		t.Errorf("focus starts at %d, want 0 (the current password)", m.passwordFocus)
	}

	m = press(t, m, "esc")
	if m.view != viewAccount {
		t.Errorf("after esc: view %q, want %q -- cancelling a form goes back one step, not to the top", m.view, viewAccount)
	}
}

// All three are masked. The setup token beside them on the login screen is
// deliberately not, and the difference is worth a test: a token is copied off a
// log line with nothing to check it against but the eye, while a password is
// typed from memory and checked against the confirmation field.
func TestThePasswordFieldsAreMasked(t *testing.T) {
	m := signedIn(t)
	for _, f := range []struct {
		name  string
		input textinput.Model
	}{
		{"current", m.currentPasswordInput},
		{"new", m.newPasswordInput},
		{"confirm", m.confirmPasswordInput},
	} {
		if f.input.EchoMode != textinput.EchoPassword {
			t.Errorf("the %s password field echoes %v, want EchoPassword", f.name, f.input.EchoMode)
		}
	}
}

// The confirmation field has to be able to refuse. Typed once and accepted, a
// mistyped new password is a password nobody knows: the change succeeds, the
// server signs every session out, and the address it belongs to now needs an
// operator with shell access to get back into.
func TestAMistypedConfirmationIsRefusedBeforeTheRequest(t *testing.T) {
	m := press(t, signedIn(t), "a", "enter")
	m.currentPasswordInput.SetValue("old-secret")
	m.newPasswordInput.SetValue("new-secret")
	m.confirmPasswordInput.SetValue("new-sceret")

	m = press(t, m, "enter")

	if m.view != viewPassword {
		t.Fatalf("view %q, want %q -- the form should stay up", m.view, viewPassword)
	}
	if !strings.Contains(m.message, "match") {
		t.Errorf("message %q, want it to say the two new passwords do not match", m.message)
	}
}

func TestBothPasswordsAreRequired(t *testing.T) {
	m := press(t, signedIn(t), "a", "enter")
	m.newPasswordInput.SetValue("new-secret")
	m.confirmPasswordInput.SetValue("new-secret")

	m = press(t, m, "enter")

	if !strings.Contains(m.message, "required") {
		t.Errorf("message %q, want it to say the current password is required", m.message)
	}
}

// Tab reaches all three, and shift+tab is the way back -- which with three
// fields is a different answer from tab, the case two fields cannot catch.
func TestTabCyclesTheThreePasswordFields(t *testing.T) {
	m := press(t, signedIn(t), "a", "enter")

	for i, want := range []int{1, 2, 0} {
		m = press(t, m, "tab")
		if m.passwordFocus != want {
			t.Fatalf("tab %d: focus %d, want %d", i+1, m.passwordFocus, want)
		}
	}

	m = press(t, m, "shift+tab")
	if m.passwordFocus != 2 {
		t.Errorf("shift+tab from the current password: focus %d, want 2 (the confirmation)", m.passwordFocus)
	}
}

// Re-entering the form finds it empty. Whatever ended it last time -- escape,
// a success, a failure -- left three passwords in the model, and the next
// person to press enter on the menu must not be looking at them.
func TestReopeningTheFormClearsWhatWasTyped(t *testing.T) {
	m := press(t, signedIn(t), "a", "enter")
	m.currentPasswordInput.SetValue("old-secret")
	m.newPasswordInput.SetValue("new-secret")
	m.confirmPasswordInput.SetValue("new-secret")

	m = press(t, m, "esc", "enter")

	for _, f := range []struct {
		name, value string
	}{
		{"current", m.currentPasswordInput.Value()},
		{"new", m.newPasswordInput.Value()},
		{"confirm", m.confirmPasswordInput.Value()},
	} {
		if f.value != "" {
			t.Errorf("the %s password field still holds %q", f.name, f.value)
		}
	}
}

// The server's count includes the session that asked, because it revokes every
// session credential and grants no exemptions. Repeating that number at
// somebody still looking at a working library list would be telling them they
// are signed out of it.
func TestPasswordChangedSummaryCountsThisSessionOut(t *testing.T) {
	cases := []struct {
		revoked int
		want    string
	}{
		{0, "Password changed"},
		{1, "Password changed"},
		{2, "Password changed; 1 other session signed out"},
		{4, "Password changed; 3 other sessions signed out"},
	}
	for _, c := range cases {
		if got := passwordChangedSummary(c.revoked); got != c.want {
			t.Errorf("passwordChangedSummary(%d) = %q, want %q", c.revoked, got, c.want)
		}
	}
}
