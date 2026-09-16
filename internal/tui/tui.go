package tui

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/dkam/silo/client"
	"github.com/dkam/silo/internal/format"
)

// Views
const (
	viewLogin            = "login"
	viewLibraries        = "libraries"
	viewNewLibrary       = "new_library"
	viewConfirm          = "confirm_delete"
	viewBrowse           = "browse"
	viewUpload           = "upload"
	viewMkdir            = "mkdir"
	viewConfirmDelete    = "confirm_delete_file"
	viewConfirmOverwrite = "confirm_overwrite"
	viewRename           = "rename"
	viewMove             = "move"
	viewAccount          = "account"
	viewPassword         = "change_password"
)

// accountItem is one line of the Account menu and the view it opens.
type accountItem struct {
	label string
	view  string
}

// accountItems is the Account menu.
//
// A list rather than a switch on the cursor, because what belongs here is not
// finished -- signing out, the sessions an account has open, what it is using
// of its quota -- and each of those should cost one line here and a view of its
// own, rather than a re-shuffle of this one.
var accountItems = []accountItem{
	{label: "Change password", view: viewPassword},
}

// passwordFields is how many inputs the change-password form has.
const passwordFields = 3

// Styles
var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63"))
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	successStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))
	helpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
)

// Messages
type loginDoneMsg struct{ err error }
type librariesLoadedMsg struct {
	libraries []client.Library
	err       error
}
type libraryCreatedMsg struct{ err error }
type libraryDeletedMsg struct{ err error }

// libraryCountedMsg carries the answer to "how much is in here?", asked when
// the delete confirmation opens. It names the library it counted: the walk
// takes as many round trips as the library has directories, and an answer that
// arrives after the user has backed out and picked a different library must
// not be shown against that one.
type libraryCountedMsg struct {
	libraryID string
	contents  libraryContents
}
type dirLoadedMsg struct {
	entries []client.DirEntry
	err     error
}
type uploadDoneMsg struct {
	err error
	// summary is what to say on success. A directory upload has a count worth
	// reporting; a single file has "File uploaded" and nothing more.
	summary string
}
type mkdirDoneMsg struct{ err error }
type deleteFileDoneMsg struct{ err error }
type downloadDoneMsg struct{ err error }
type renameDoneMsg struct{ err error }
type moveDoneMsg struct{ err error }

// passwordChangedMsg carries the count the server gave back: how many session
// credentials it signed out, this one included.
type passwordChangedMsg struct {
	revoked int
	err     error
}
type movePickerLoadedMsg struct {
	dirs []client.DirEntry
	err  error
}
type serverInfoMsg struct {
	version       string
	setupRequired bool
}

// libraryUpdatedMsg is a push from the notification socket: a library's head
// has moved, whoever moved it. It is what keeps two TUIs open on one library in
// step, rather than each waiting for its own user to press 'r'.
type libraryUpdatedMsg struct{ client.LibraryUpdate }

// watchClosedMsg says the socket is finished for good, so nothing re-arms the
// wait for the next event.
type watchClosedMsg struct{}

type model struct {
	api *client.APIClient
	// watcher is the notification socket. Nil until login, and nil again if it
	// closes for good; both cases fall back to refreshing by hand.
	watcher *client.Watcher
	view    string

	// Login
	emailInput      textinput.Model
	passwordInput   textinput.Model
	setupTokenInput textinput.Model
	loginFocus      int // 0=email, 1=password, 2=setup token (setup mode only)
	// setupMode says the server told us it has no accounts. The same screen
	// then collects a third field and creates the account rather than signing
	// in to one. A mode rather than a second view: everything after the
	// request -- the transition to the library list, the watcher, the error
	// row -- is shared, and the mode arrives asynchronously from server-info,
	// so a view switch would race the first frame.
	setupMode bool

	// Libraries
	libraries       []client.Library
	cursor          int
	librariesOffset int    // index of the first library drawn
	message         string // status message
	// result is what to say once the reload a write kicks off has finished.
	// Put in message directly it would be gone before it was read: the reload
	// lands a moment later and clears the row it was written to.
	result string

	// New library
	newLibraryInput textinput.Model

	// Delete library. The id and name are copied out of the list on the way
	// in rather than read back through the cursor, so that the screen keeps
	// naming the library the count was asked about even if the list moves
	// underneath it.
	deleteLibraryID   string
	deleteLibraryName string
	// deleteContents is nil until the walk answers. A confirmation that does
	// not yet know what it is about accepts nothing -- neither the y nor the
	// typed name -- because the two shapes of confirmation are not
	// interchangeable and guessing which one is on screen is the user's job
	// only once the screen has settled.
	deleteContents     *libraryContents
	deleteConfirmInput textinput.Model

	// Browse
	browseLibraryID   string
	browseLibraryName string
	browsePath        string
	dirEntries        []client.DirEntry
	browseCursor      int
	browseOffset      int // index of the first entry drawn

	// Upload
	uploadInput textinput.Model

	// Mkdir
	mkdirInput textinput.Model

	// Rename
	renameInput textinput.Model

	// Account
	accountCursor int

	// Change password
	currentPasswordInput textinput.Model
	newPasswordInput     textinput.Model
	confirmPasswordInput textinput.Model
	passwordFocus        int // 0=current, 1=new, 2=confirm

	// Move (remote directory picker)
	moveSrcPath      string
	movePickerPath   string
	movePickerDirs   []client.DirEntry
	movePickerCursor int
	movePickerOffset int // index of the first directory drawn

	// Pending download (for overwrite confirmation)
	pendingDownloadLibraryPath string
	pendingDownloadLocalPath   string

	// Auto-login from env
	autoEmail    string
	autoPassword string

	// Server info
	serverURL     string
	serverVersion string

	width  int
	height int
}

// fetchServerInfo asks what this server is and whether anyone has claimed it.
//
// The error is still discarded, and that is still right: an unreachable server
// leaves setupRequired false, the login screen stays a login screen, and the
// attempt the operator makes next produces a message they can act on. A second
// error row saying the same thing in different words would not help them.
func (m model) fetchServerInfo() tea.Msg {
	info, _ := m.api.GetServerInfo()
	return serverInfoMsg{version: info.Version, setupRequired: info.SetupRequired}
}

// awaitUpdate waits for the next push. Bubble Tea runs commands off the update
// loop, so one parked here blocks nothing — but a command fires once, which is
// why every handler of a libraryUpdatedMsg arms another.
func (m model) awaitUpdate() tea.Cmd {
	if m.watcher == nil {
		return nil
	}
	events := m.watcher.Events()
	return func() tea.Msg {
		ev, ok := <-events
		if !ok {
			return watchClosedMsg{}
		}
		return libraryUpdatedMsg{LibraryUpdate: ev}
	}
}

// refreshFor is the reload an update calls for, or nil when the update is about
// something this screen is not showing. The form views reload nothing on
// purpose: they are holding half-typed input, and re-fetching underneath would
// move the ground it was typed against.
func (m model) refreshFor(libraryID string) tea.Cmd {
	switch m.view {
	case viewLibraries:
		return m.loadLibraries
	case viewBrowse:
		if libraryID == m.browseLibraryID {
			return m.loadDir
		}
	}
	return nil
}

// entryPath builds a full library path from the current browse path and an entry name.
func (m model) entryPath(name string) string {
	return path.Join(m.browsePath, name)
}

func initialModel(serverURL, autoEmail, autoPassword string) model {
	email := textinput.New()
	email.Placeholder = "email@example.com"
	email.Focus()
	email.CharLimit = 255

	password := textinput.New()
	password.Placeholder = "password"
	password.EchoMode = textinput.EchoPassword
	password.CharLimit = 255

	// Not masked. It is meant to be checked against a log line by eye, and a
	// masked field makes a mistyped token undiagnosable.
	setupToken := textinput.New()
	setupToken.Placeholder = "SILO-XXXX-XXXX-XXXX-XXXX"
	setupToken.CharLimit = 64

	newLibrary := textinput.New()
	newLibrary.Placeholder = "Library name"
	newLibrary.CharLimit = 255

	upload := textinput.New()
	upload.Placeholder = "/path/to/local/file-or-directory"
	upload.CharLimit = 1024

	mkdirIn := textinput.New()
	mkdirIn.Placeholder = "directory name"
	mkdirIn.CharLimit = 255

	renameIn := textinput.New()
	renameIn.Placeholder = "new name"
	renameIn.CharLimit = 255

	deleteConfirm := textinput.New()
	deleteConfirm.Placeholder = "library name"
	deleteConfirm.CharLimit = 255

	// Masked, unlike the setup token beside them. A password is typed from
	// memory and confirmed against a second field; a token is copied off a log
	// line and has nothing to check it against but the operator's own eyes.
	currentPassword := textinput.New()
	currentPassword.Placeholder = "current password"
	currentPassword.EchoMode = textinput.EchoPassword
	currentPassword.CharLimit = 255

	newPassword := textinput.New()
	newPassword.Placeholder = "new password"
	newPassword.EchoMode = textinput.EchoPassword
	newPassword.CharLimit = 255

	confirmPassword := textinput.New()
	confirmPassword.Placeholder = "new password again"
	confirmPassword.EchoMode = textinput.EchoPassword
	confirmPassword.CharLimit = 255

	m := model{
		api:             client.NewClient(serverURL),
		view:            viewLogin,
		emailInput:      email,
		passwordInput:   password,
		setupTokenInput: setupToken,
		newLibraryInput: newLibrary,
		uploadInput:     upload,
		mkdirInput:      mkdirIn,
		renameInput:     renameIn,

		deleteConfirmInput: deleteConfirm,

		currentPasswordInput: currentPassword,
		newPasswordInput:     newPassword,
		confirmPasswordInput: confirmPassword,

		autoEmail:    autoEmail,
		autoPassword: autoPassword,
		serverURL:    serverURL,
		// A usable size until the first WindowSizeMsg lands, so the opening
		// frame is not laid out against a zero-sized terminal.
		width:  80,
		height: 24,
	}

	if autoEmail != "" {
		m.emailInput.SetValue(autoEmail)
	}

	return m
}

func (m model) Init() tea.Cmd {
	if m.autoEmail != "" && m.autoPassword != "" {
		m.message = "Logging in..."
		return tea.Batch(m.fetchServerInfo, func() tea.Msg {
			err := m.api.Login(m.autoEmail, m.autoPassword)
			return loginDoneMsg{err: err}
		})
	}
	return tea.Batch(textinput.Blink, m.fetchServerInfo)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "q":
			// Not on the login view: "q" is a legal character in an email
			// address, and the form has no other way to type one.
			if m.view == viewLibraries || m.view == viewBrowse || m.view == viewAccount {
				return m, tea.Quit
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	// Asked from Init, so the login screen knows whether it is really a setup
	// screen, and asked again after a login only if that first answer never
	// arrived. It stays here rather than in the login view's update because a
	// retry's answer lands once the library list is already on screen.
	case serverInfoMsg:
		m.serverVersion = msg.version
		// Only while the login screen is still up. The post-login answer must
		// not be able to flip a live session into setup mode, however slow it
		// was in coming.
		if m.view == viewLogin {
			m.setupMode = msg.setupRequired
			if m.loginFocus >= m.loginFields() {
				m.loginFocus = 0
			}
			m.focusLoginField()
			if m.setupMode {
				// An auto-login fired from Init before this answer arrived, and
				// on an unclaimed server it failed. Clear it: a 401 about an
				// account that does not exist yet only reads as something the
				// operator did wrong.
				m.message = ""
			}
		}

	// A library moved. Handled here rather than in a view's update because it
	// arrives whatever is on screen, and because the wait for the next one has
	// to be re-armed either way.
	case libraryUpdatedMsg:
		return m, tea.Batch(m.refreshFor(msg.LibraryID), m.awaitUpdate())

	case watchClosedMsg:
		m.watcher = nil
		return m, nil

	// The password change lands here rather than only in the password view,
	// for the same reason libraryUpdatedMsg does: it arrives whatever is on
	// screen. Escaping the form does not cancel the request — the command is
	// already in flight — so routing it by view meant that pressing esc while
	// it ran dropped the result on the floor. The password had changed, every
	// credential had been revoked, and the user saw neither a confirmation nor
	// an error.
	case passwordChangedMsg:
		if msg.err != nil {
			m.message = errorStyle.Render(msg.err.Error())
		} else {
			m.message = successStyle.Render(passwordChangedSummary(msg.revoked))
		}
		if m.view == viewPassword {
			m.view = viewAccount
		}
		return m, nil
	}

	var (
		next tea.Model
		cmd  tea.Cmd
	)
	switch m.view {
	case viewLogin:
		next, cmd = m.updateLogin(msg)
	case viewLibraries:
		next, cmd = m.updateLibraries(msg)
	case viewNewLibrary:
		next, cmd = m.updateNewLibrary(msg)
	case viewConfirm:
		next, cmd = m.updateConfirm(msg)
	case viewBrowse:
		next, cmd = m.updateBrowse(msg)
	case viewUpload:
		next, cmd = m.updateUpload(msg)
	case viewMkdir:
		next, cmd = m.updateMkdir(msg)
	case viewConfirmDelete:
		next, cmd = m.updateConfirmDeleteFile(msg)
	case viewConfirmOverwrite:
		next, cmd = m.updateConfirmOverwrite(msg)
	case viewRename:
		next, cmd = m.updateRename(msg)
	case viewMove:
		next, cmd = m.updateMove(msg)
	case viewAccount:
		next, cmd = m.updateAccount(msg)
	case viewPassword:
		next, cmd = m.updatePassword(msg)
	default:
		return m, nil
	}

	// Every path back out of a view lands here, so a cursor move, a reload
	// and a resize all get their scroll offsets fixed up the same way.
	if updated, ok := next.(model); ok {
		return updated.syncScroll(), cmd
	}
	return next, cmd
}

// --- Login View ---

// loginFields is how many inputs the login screen is showing. Setup mode adds
// the token, and the cycle is sized from this rather than from a constant so
// that a fourth field would be one line rather than four.
func (m model) loginFields() int {
	if m.setupMode {
		return 3
	}
	return 2
}

// focusLoginField moves the cursor to whichever field loginFocus names, and
// blurs the rest. Blur everything first: a field left focused behind the cursor
// still takes keystrokes, and two focused inputs would each get every rune.
func (m *model) focusLoginField() {
	m.emailInput.Blur()
	m.passwordInput.Blur()
	m.setupTokenInput.Blur()

	switch m.loginFocus {
	case 0:
		m.emailInput.Focus()
	case 1:
		m.passwordInput.Focus()
	case 2:
		m.setupTokenInput.Focus()
	}
}

func (m model) updateLogin(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "tab", "down":
			m.loginFocus = (m.loginFocus + 1) % m.loginFields()
			m.focusLoginField()
			return m, nil

		case "shift+tab", "up":
			n := m.loginFields()
			m.loginFocus = (m.loginFocus + n - 1) % n
			m.focusLoginField()
			return m, nil

		case "enter":
			email := m.emailInput.Value()
			password := m.passwordInput.Value()

			if m.setupMode {
				token := strings.TrimSpace(m.setupTokenInput.Value())
				if email == "" || password == "" || token == "" {
					m.message = errorStyle.Render("Email, password and setup token required")
					return m, nil
				}
				m.message = "Creating the first account..."
				return m, func() tea.Msg {
					return loginDoneMsg{err: m.api.Setup(email, password, token)}
				}
			}

			if email == "" || password == "" {
				m.message = "Email and password required"
				return m, nil
			}
			m.message = "Logging in..."
			return m, func() tea.Msg {
				err := m.api.Login(email, password)
				return loginDoneMsg{err: err}
			}
		}

	case loginDoneMsg:
		if msg.err != nil {
			m.message = errorStyle.Render(msg.err.Error())
			return m, nil
		}
		m.view = viewLibraries
		m.message = ""
		// The socket opens here and lasts the session. A server built without
		// a notification endpoint is not an error: the watcher asks, finds
		// none, and stops, leaving the views to load when they are entered.
		m.watcher = m.api.Watch()
		cmds := []tea.Cmd{m.loadLibraries, m.awaitUpdate()}
		// Only if Init's fetch did not answer. It asks for the same two facts,
		// neither of which a login can have changed; asking again is a round
		// trip for a version string already on screen.
		if m.serverVersion == "" {
			cmds = append(cmds, m.fetchServerInfo)
		}
		return m, tea.Batch(cmds...)
	}

	var cmds []tea.Cmd
	var cmd tea.Cmd
	m.emailInput, cmd = m.emailInput.Update(msg)
	cmds = append(cmds, cmd)
	m.passwordInput, cmd = m.passwordInput.Update(msg)
	cmds = append(cmds, cmd)
	// Only in setup mode, so keystrokes are not buffered into a field that is
	// not on screen and cannot be reached.
	if m.setupMode {
		m.setupTokenInput, cmd = m.setupTokenInput.Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

func (m model) renderLogin() string {
	if m.setupMode {
		return m.renderSetup()
	}

	header := []string{titleStyle.Render("Silo Login"), ""}
	body := []string{
		"Email:",
		m.emailInput.View(),
		"",
		"Password:",
		m.passwordInput.View(),
	}
	return m.frame(header, body, m.footer(headerRows, loginHelp))
}

// renderSetup is the login screen on a server nobody has claimed.
//
// The sentence about choosing rather than signing in is the load-bearing one.
// The entire risk of this screen is an operator reading three fields, assuming
// the first two name an account that already exists, and typing a guess at
// credentials rather than deciding on them.
func (m model) renderSetup() string {
	header := []string{titleStyle.Render("Silo Setup"), ""}

	body := []string{}
	body = append(body, wrapText(
		"This server has no accounts yet. Choose the email and password you want — "+
			"you are creating the first account, not signing in to one.", m.width)...)
	body = append(body, "")
	body = append(body, wrapText(
		"Paste the setup token from the server's log, or run: silo setup-token", m.width)...)

	body = append(body,
		"",
		"Email:",
		m.emailInput.View(),
		"",
		"Password:",
		m.passwordInput.View(),
		"",
		"Setup token:",
		m.setupTokenInput.View(),
	)
	return m.frame(header, body, m.footer(headerRows, setupHelp))
}

// --- Libraries View ---

func (m model) loadLibraries() tea.Msg {
	libraries, err := m.api.ListLibraries()
	return librariesLoadedMsg{libraries: libraries, err: err}
}

func (m model) updateLibraries(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if c, o, ok := moveCursor(msg.String(), m.cursor, m.librariesOffset, len(m.libraries), m.librariesRows()); ok {
			m.cursor, m.librariesOffset = c, o
			return m, nil
		}
		switch msg.String() {
		case "n":
			m.view = viewNewLibrary
			m.newLibraryInput.SetValue("")
			m.newLibraryInput.Focus()
			m.message = ""
			return m, textinput.Blink
		case "d":
			if len(m.libraries) > 0 {
				return m.enterDeleteLibrary(m.libraries[m.cursor])
			}
		case "a":
			m.view = viewAccount
			m.message = ""
			return m, nil
		case "r":
			m.message = "Refreshing..."
			return m, m.loadLibraries
		case "enter":
			if len(m.libraries) > 0 {
				library := m.libraries[m.cursor]
				m.browseLibraryID = library.ID
				m.browseLibraryName = library.Name
				m.browsePath = "/"
				m.browseCursor = 0
				m.view = viewBrowse
				m.message = ""
				// Subscribed on the way in, and left subscribed on the way
				// out: the set is one library per library visited this
				// session, and an event for a library not on screen is
				// discarded by refreshFor for the price of one comparison.
				if m.watcher != nil {
					m.watcher.Subscribe(library.ID)
				}
				return m, m.loadDir
			}
		}

	case librariesLoadedMsg:
		if msg.err != nil {
			m.message = errorStyle.Render(msg.err.Error())
			return m, nil
		}
		m.libraries = msg.libraries
		m.message, m.result = m.result, ""
		if m.cursor >= len(m.libraries) {
			m.cursor = max(0, len(m.libraries)-1)
		}
	}

	return m, nil
}

func (m model) renderLibraries() string {
	from, to, counter := window(m.librariesOffset, len(m.libraries), m.librariesRows())
	header := []string{titleStyle.Render("Libraries") + counter, ""}

	var body []string
	if len(m.libraries) == 0 {
		body = append(body, dimStyle.Render("  No libraries yet. Press 'n' to create one."))
	}
	for i := from; i < to; i++ {
		library := m.libraries[i]
		cursor := "  "
		name := library.Name
		if library.Name == "" {
			name = "(unnamed)"
		}
		if i == m.cursor {
			cursor = "> "
			name = selectedStyle.Render(name)
		}
		ts := ""
		if library.UpdateTime > 0 {
			ts = dimStyle.Render(" " + time.Unix(library.UpdateTime, 0).Format("2006-01-02 15:04"))
		}
		encrypted := ""
		if library.Encrypted {
			encrypted = dimStyle.Render(" [encrypted]")
		}
		body = append(body, cursor+name+ts+encrypted, dimStyle.Render("    "+library.ID))
	}

	return m.frame(header, body, m.footer(headerRows, librariesHelp))
}

// --- New Library View ---

func (m model) updateNewLibrary(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			m.view = viewLibraries
			return m, nil
		case "enter":
			name := m.newLibraryInput.Value()
			if name == "" {
				m.message = "Name is required"
				return m, nil
			}
			m.message = "Creating..."
			return m, func() tea.Msg {
				_, err := m.api.CreateLibrary(name)
				return libraryCreatedMsg{err: err}
			}
		}

	case libraryCreatedMsg:
		if msg.err != nil {
			m.message = errorStyle.Render(msg.err.Error())
			return m, nil
		}
		m.view = viewLibraries
		m.result = successStyle.Render("Library created")
		return m, m.loadLibraries
	}

	var cmd tea.Cmd
	m.newLibraryInput, cmd = m.newLibraryInput.Update(msg)
	return m, cmd
}

func (m model) renderNewLibrary() string {
	header := []string{titleStyle.Render("Create Library"), ""}
	body := []string{"Name:", m.newLibraryInput.View()}
	return m.frame(header, body, m.footer(headerRows, createHelp))
}

// --- Confirm Delete View ---

// How far the walk behind the delete confirmation will go before it answers
// with a floor instead of a total.
//
// The question the screen has to answer is "is there anything here worth
// stopping for", not "exactly how much"; once the answer is 500 files the
// warning reads the same whether the true figure is 500 or 500,000. The
// request cap covers the other shape of large library — a wide, shallow tree
// of empty-ish directories, where the files run out long before the round
// trips do.
const (
	countFileCap    = 500
	countRequestCap = 100
)

// libraryContents is how much a library holds, as far as the walk got.
type libraryContents struct {
	files, dirs int
	// partial says the walk stopped at one of its caps rather than at the end
	// of the tree, so the counts are a floor and have to be reported as one.
	partial bool
	// err is a listing that failed. The counts are then unknown rather than
	// zero, which is why empty() is false: an unreachable server is a reason
	// to ask for more confirmation, not less.
	err error
}

func (c libraryContents) empty() bool {
	return c.err == nil && c.files == 0 && c.dirs == 0
}

// phrase reads "1,204 files in 12 folders" — the counts alone, for a sentence
// to be built around.
//
// No byte total: a directory listing does not carry sizes (see DirEntry.Size),
// so a total would mean a manifest read per file, and the walk is already the
// slowest thing this screen does.
func (c libraryContents) phrase() string {
	var s string
	switch {
	case c.files == 0:
		s = plural(c.dirs, "folder")
	case c.dirs == 0:
		s = plural(c.files, "file")
	default:
		s = plural(c.files, "file") + " in " + plural(c.dirs, "folder")
	}
	if c.partial {
		return "at least " + s
	}
	return s
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return format.Count(n) + " " + noun + "s"
}

// countTree walks a library and counts what is in it, bounded by the caps
// above.
//
// Breadth first rather than depth first, so that a library whose everything
// sits one folder down is described from the top rather than from whichever
// branch the walk fell into: the counts are a floor either way, and a floor
// gathered from the shallow end is the one that matches what the user would
// have seen had they gone looking themselves.
//
// list is passed in rather than taken from the client, so this is testable
// against a tree rather than against a server.
func countTree(list func(dir string) ([]client.DirEntry, error)) libraryContents {
	var c libraryContents
	queue := []string{"/"}
	for requests := 0; len(queue) > 0; requests++ {
		if requests >= countRequestCap || c.files >= countFileCap {
			c.partial = true
			return c
		}
		current := queue[0]
		queue = queue[1:]

		entries, err := list(current)
		if err != nil {
			c.err = err
			return c
		}
		for _, e := range entries {
			if e.Type == "dir" {
				c.dirs++
				queue = append(queue, path.Join(current, e.Name))
				continue
			}
			c.files++
		}
	}
	return c
}

// countLibrary is the walk as a command, tagged with the library it is about.
func (m model) countLibrary(libraryID string) tea.Cmd {
	api := m.api
	return func() tea.Msg {
		return libraryCountedMsg{
			libraryID: libraryID,
			contents: countTree(func(dir string) ([]client.DirEntry, error) {
				return api.ListDir(libraryID, dir)
			}),
		}
	}
}

// enterDeleteLibrary opens the confirmation and sends it to find out what it
// is confirming.
func (m model) enterDeleteLibrary(library client.Library) (model, tea.Cmd) {
	m.view = viewConfirm
	m.message = ""
	m.deleteLibraryID = library.ID
	m.deleteLibraryName = library.Name
	m.deleteContents = nil
	m.deleteConfirmInput.SetValue("")
	m.deleteConfirmInput.Focus()
	return m, tea.Batch(textinput.Blink, m.countLibrary(library.ID))
}

// leaveDeleteLibrary backs out, leaving nothing typed behind for the next
// library the user lands on.
func (m model) leaveDeleteLibrary() model {
	m.view = viewLibraries
	m.message = ""
	m.deleteContents = nil
	m.deleteConfirmInput.SetValue("")
	m.deleteConfirmInput.Blur()
	return m
}

// deletePhrase is what has to be typed back to delete a library that holds
// something: its own name, which is what the user picked it by. A library with
// no name has nothing to type back, so it asks for the word instead.
func (m model) deletePhrase() string {
	if name := strings.TrimSpace(m.deleteLibraryName); name != "" {
		return name
	}
	return "delete"
}

func (m model) deleteLibraryCmd() tea.Cmd {
	api, libraryID := m.api, m.deleteLibraryID
	return func() tea.Msg {
		return libraryDeletedMsg{err: api.DeleteLibrary(libraryID)}
	}
}

// updateConfirm runs two confirmations behind one view, and which one is on
// screen is decided by what the library holds rather than by the user.
//
// An empty library is a y/n, as it always was. One with anything in it asks for
// its name typed back, because 'd' and 'y' are one key apart under the same
// finger and the thing on the other side of them is not recoverable: the
// deletion takes the whole library, and there is no undo behind it.
func (m model) updateConfirm(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case libraryCountedMsg:
		// An answer about a library this screen is no longer about is dropped
		// rather than shown.
		if msg.libraryID != m.deleteLibraryID {
			return m, nil
		}
		contents := msg.contents
		m.deleteContents = &contents
		return m, nil

	case tea.KeyMsg:
		if msg.String() == "esc" {
			return m.leaveDeleteLibrary(), nil
		}
		switch {
		case m.deleteContents == nil:
			// Still counting. Nothing is confirmable yet, and a keystroke
			// meant for one shape of the prompt must not be taken by the
			// other when it arrives.
			return m, nil

		case m.deleteContents.empty():
			switch msg.String() {
			case "y", "Y":
				m.message = "Deleting..."
				return m, m.deleteLibraryCmd()
			case "n", "N":
				return m.leaveDeleteLibrary(), nil
			}
			return m, nil

		case msg.String() == "enter":
			if strings.TrimSpace(m.deleteConfirmInput.Value()) != m.deletePhrase() {
				m.message = errorStyle.Render("That is not the name. Nothing was deleted.")
				return m, nil
			}
			m.message = "Deleting..."
			return m, m.deleteLibraryCmd()
		}

	case libraryDeletedMsg:
		if msg.err != nil {
			m = m.leaveDeleteLibrary()
			m.message = errorStyle.Render(msg.err.Error())
			return m, nil
		}
		m = m.leaveDeleteLibrary()
		m.result = successStyle.Render("Library deleted")
		return m, m.loadLibraries
	}

	// Everything else is typing into the name field. Only reached with a
	// non-empty library on screen: the two cases above return before here.
	var cmd tea.Cmd
	m.deleteConfirmInput, cmd = m.deleteConfirmInput.Update(msg)
	return m, cmd
}

func (m model) renderConfirm() string {
	name := m.deleteLibraryName
	if strings.TrimSpace(name) == "" {
		name = "(unnamed)"
	}
	header := []string{titleStyle.Render("Delete Library"), ""}

	switch {
	case m.deleteContents == nil:
		body := []string{
			fmt.Sprintf("Delete %q?", name),
			"",
			dimStyle.Render("Checking what it holds..."),
		}
		return m.frame(header, body, m.footer(headerRows, deleteWaitHelp))

	case m.deleteContents.empty():
		body := []string{
			fmt.Sprintf("Are you sure you want to delete %q?", name),
			"",
			dimStyle.Render("It is empty."),
		}
		return m.frame(header, body, m.footer(headerRows, confirmHelp))
	}

	warning := fmt.Sprintf("%q holds %s.", name, m.deleteContents.phrase())
	if m.deleteContents.err != nil {
		warning = fmt.Sprintf("Could not check what %q holds: %v", name, m.deleteContents.err)
	}

	body := append([]string{}, wrapText(errorStyle.Render(warning), m.width)...)
	body = append(body, "")
	body = append(body, wrapText("Deleting the library deletes all of it, on the server and for everyone it is shared with. This cannot be undone.", m.width)...)
	body = append(body,
		"",
		fmt.Sprintf("Type %q to confirm:", m.deletePhrase()),
		m.deleteConfirmInput.View(),
	)
	return m.frame(header, body, m.footer(headerRows, deleteConfirmHelp))
}

// --- Browse View ---

func (m model) loadDir() tea.Msg {
	entries, err := m.api.ListDir(m.browseLibraryID, m.browsePath)
	return dirLoadedMsg{entries: entries, err: err}
}

func (m model) updateBrowse(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		m.message = "" // Clear status on any keypress
		if c, o, ok := moveCursor(msg.String(), m.browseCursor, m.browseOffset, len(m.dirEntries), m.browseRows()); ok {
			m.browseCursor, m.browseOffset = c, o
			return m, nil
		}
		switch msg.String() {
		case "enter":
			if m.browseCursor < len(m.dirEntries) {
				entry := m.dirEntries[m.browseCursor]
				if entry.Type == "dir" {
					m.browsePath = m.entryPath(entry.Name)
					m.browseCursor = 0
					m.message = ""
					return m, m.loadDir
				}
				// File — download
				libraryPath := m.entryPath(entry.Name)
				localPath := entry.Name

				// Check if local file exists
				if _, err := os.Stat(localPath); err == nil {
					m.pendingDownloadLibraryPath = libraryPath
					m.pendingDownloadLocalPath = localPath
					m.view = viewConfirmOverwrite
					m.message = ""
					return m, nil
				}

				m.message = "Downloading..."
				libraryID := m.browseLibraryID
				return m, func() tea.Msg {
					err := m.api.DownloadFile(libraryID, libraryPath, localPath)
					return downloadDoneMsg{err: err}
				}
			}
		case "m":
			m.view = viewMkdir
			m.mkdirInput.SetValue("")
			m.mkdirInput.Focus()
			m.message = ""
			return m, textinput.Blink
		case "r":
			if m.browseCursor < len(m.dirEntries) {
				m.renameInput.SetValue(m.dirEntries[m.browseCursor].Name)
				m.renameInput.Focus()
				m.view = viewRename
				m.message = ""
				return m, textinput.Blink
			}
		case "v":
			if m.browseCursor < len(m.dirEntries) {
				entry := m.dirEntries[m.browseCursor]
				m.moveSrcPath = m.entryPath(entry.Name)
				m.movePickerPath = "/"
				m.movePickerCursor = 0
				m.view = viewMove
				m.message = ""
				libraryID := m.browseLibraryID
				return m, func() tea.Msg {
					entries, err := m.api.ListDir(libraryID, "/")
					return movePickerLoadedMsg{dirs: entries, err: err}
				}
			}
		case "x":
			if m.browseCursor < len(m.dirEntries) {
				m.view = viewConfirmDelete
				m.message = ""
			}
		case "backspace", "h":
			if m.browsePath != "/" {
				// Go up one level
				parts := strings.Split(m.browsePath, "/")
				if len(parts) > 1 {
					m.browsePath = strings.Join(parts[:len(parts)-1], "/")
					if m.browsePath == "" {
						m.browsePath = "/"
					}
				}
				m.browseCursor = 0
				m.message = ""
				return m, m.loadDir
			}
		case "esc":
			m.view = viewLibraries
			m.message = ""
			return m, nil
		case "u":
			m.view = viewUpload
			m.uploadInput.SetValue("")
			m.uploadInput.Focus()
			m.message = ""
			return m, textinput.Blink
		}

	case downloadDoneMsg:
		if msg.err != nil {
			m.message = errorStyle.Render(msg.err.Error())
		} else {
			m.message = successStyle.Render("Downloaded to current directory")
		}
		return m, nil

	case dirLoadedMsg:
		if msg.err != nil {
			m.message = errorStyle.Render(msg.err.Error())
			return m, nil
		}
		m.dirEntries = msg.entries
		m.message, m.result = m.result, ""
		if m.browseCursor >= len(m.dirEntries) {
			m.browseCursor = max(0, len(m.dirEntries)-1)
		}
	}

	return m, nil
}

func (m model) renderBrowse() string {
	from, to, counter := window(m.browseOffset, len(m.dirEntries), m.browseRows())
	header := []string{titleStyle.Render(m.browseLibraryName+" "+m.browsePath) + counter, ""}

	var body []string
	if len(m.dirEntries) == 0 {
		body = append(body, dimStyle.Render("  (empty directory)"))
	}
	for i := from; i < to; i++ {
		entry := m.dirEntries[i]
		cursor := "  "
		name := entry.Name
		if entry.Type == "dir" {
			name += "/"
		}
		if i == m.browseCursor {
			cursor = "> "
			name = selectedStyle.Render(name)
		}
		ts := ""
		if entry.Mtime > 0 {
			ts = dimStyle.Render("  " + time.Unix(entry.Mtime, 0).Format("2006-01-02 15:04"))
		}
		if entry.Type == "dir" {
			body = append(body, cursor+name+ts)
			continue
		}
		// A file whose size the server did not send shows nothing rather than
		// "0 B", which would read as an empty file.
		size := ""
		if entry.Size != nil {
			size = "  " + dimStyle.Render(format.Bytes(*entry.Size))
		}
		body = append(body, cursor+name+size+ts)
	}

	return m.frame(header, body, m.footer(headerRows, browseHelp))
}

// --- Upload View ---

func (m model) updateUpload(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			m.view = viewBrowse
			return m, nil
		case "enter":
			localPath := expandHome(m.uploadInput.Value())
			if localPath == "" {
				m.message = "File path is required"
				return m, nil
			}
			info, err := os.Stat(localPath)
			if err != nil {
				m.message = errorStyle.Render(err.Error())
				return m, nil
			}

			libraryID := m.browseLibraryID
			parentDir := m.browsePath
			if info.IsDir() {
				m.message = "Uploading directory..."
				return m, func() tea.Msg {
					up, err := m.api.UploadDir(libraryID, parentDir, localPath, nil)
					return uploadDoneMsg{err: err, summary: uploadSummary(up)}
				}
			}
			m.message = "Uploading..."
			return m, func() tea.Msg {
				err := m.api.UploadFile(libraryID, parentDir, localPath)
				return uploadDoneMsg{err: err, summary: "File uploaded"}
			}
		}

	case uploadDoneMsg:
		if msg.err != nil {
			m.message = errorStyle.Render(msg.err.Error())
			return m, nil
		}
		m.view = viewBrowse
		m.result = successStyle.Render(msg.summary)
		return m, m.loadDir
	}

	var cmd tea.Cmd
	m.uploadInput, cmd = m.uploadInput.Update(msg)
	return m, cmd
}

func (m model) renderUpload() string {
	header := []string{titleStyle.Render("Upload to " + m.browseLibraryName + " " + m.browsePath), ""}
	body := []string{
		"Local file or directory:",
		m.uploadInput.View(),
		"",
		dimStyle.Render("A directory is uploaded whole, keeping its own name."),
	}
	return m.frame(header, body, m.footer(headerRows, uploadHelp))
}

// uploadSummary says what a directory upload did in one line. Chunks held back
// is the number worth showing: it is the content the server already had, and
// the reason a re-run of a large tree finishes in seconds.
func uploadSummary(up *client.TreeUpload) string {
	if up == nil {
		return "Uploaded"
	}
	summary := fmt.Sprintf("Uploaded %d files in %d directories", up.Files, up.Dirs)
	if up.ChunksHeld > 0 {
		summary += fmt.Sprintf(" (%d chunks already on the server)", up.ChunksHeld)
	}
	return summary
}

// expandHome makes "~/photos" mean what it does in a shell. The input is typed
// by hand into a field, not expanded by one.
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
}

// --- Mkdir View ---

func (m model) updateMkdir(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			m.view = viewBrowse
			return m, nil
		case "enter":
			name := m.mkdirInput.Value()
			if name == "" {
				m.message = "Directory name is required"
				return m, nil
			}
			fullPath := m.entryPath(name)
			libraryID := m.browseLibraryID
			m.message = "Creating directory..."
			return m, func() tea.Msg {
				err := m.api.Mkdir(libraryID, fullPath)
				return mkdirDoneMsg{err: err}
			}
		}

	case mkdirDoneMsg:
		if msg.err != nil {
			m.message = errorStyle.Render(msg.err.Error())
			return m, nil
		}
		m.view = viewBrowse
		m.result = successStyle.Render("Directory created")
		return m, m.loadDir
	}

	var cmd tea.Cmd
	m.mkdirInput, cmd = m.mkdirInput.Update(msg)
	return m, cmd
}

func (m model) renderMkdir() string {
	header := []string{titleStyle.Render("Create directory in " + m.browseLibraryName + " " + m.browsePath), ""}
	body := []string{"Directory name:", m.mkdirInput.View()}
	return m.frame(header, body, m.footer(headerRows, createHelp))
}

// --- Confirm Delete File View ---

func (m model) updateConfirmDeleteFile(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y":
			entry := m.dirEntries[m.browseCursor]
			filePath := m.entryPath(entry.Name)
			libraryID := m.browseLibraryID
			m.message = "Deleting..."
			return m, func() tea.Msg {
				err := m.api.DeleteFile(libraryID, filePath)
				return deleteFileDoneMsg{err: err}
			}
		case "n", "N", "esc":
			m.view = viewBrowse
			return m, nil
		}

	case deleteFileDoneMsg:
		if msg.err != nil {
			m.view = viewBrowse
			m.message = errorStyle.Render(msg.err.Error())
			return m, nil
		}
		m.view = viewBrowse
		m.result = successStyle.Render("Deleted")
		return m, m.loadDir
	}

	return m, nil
}

func (m model) renderConfirmDeleteFile() string {
	name := "(unknown)"
	if m.browseCursor < len(m.dirEntries) {
		name = m.dirEntries[m.browseCursor].Name
	}
	header := []string{titleStyle.Render("Delete"), ""}
	body := []string{fmt.Sprintf("Are you sure you want to delete %q?", name)}
	return m.frame(header, body, m.footer(headerRows, confirmHelp))
}

// --- Rename View ---

func (m model) updateRename(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			m.view = viewBrowse
			return m, nil
		case "enter":
			newName := m.renameInput.Value()
			if newName == "" {
				m.message = "Name is required"
				return m, nil
			}
			entry := m.dirEntries[m.browseCursor]
			filePath := m.entryPath(entry.Name)
			libraryID := m.browseLibraryID
			m.message = "Renaming..."
			return m, func() tea.Msg {
				err := m.api.RenameFile(libraryID, filePath, newName)
				return renameDoneMsg{err: err}
			}
		}

	case renameDoneMsg:
		if msg.err != nil {
			m.message = errorStyle.Render(msg.err.Error())
			return m, nil
		}
		m.view = viewBrowse
		m.result = successStyle.Render("Renamed")
		return m, m.loadDir
	}

	var cmd tea.Cmd
	m.renameInput, cmd = m.renameInput.Update(msg)
	return m, cmd
}

func (m model) renderRename() string {
	name := "(unknown)"
	if m.browseCursor < len(m.dirEntries) {
		name = m.dirEntries[m.browseCursor].Name
	}
	header := []string{titleStyle.Render(fmt.Sprintf("Rename %q", name)), ""}
	body := []string{"New name:", m.renameInput.View()}
	return m.frame(header, body, m.footer(headerRows, renameHelp))
}

// --- Move View (remote directory picker) ---

func (m model) loadMoveDirs() tea.Cmd {
	libraryID := m.browseLibraryID
	pickerPath := m.movePickerPath
	return func() tea.Msg {
		entries, err := m.api.ListDir(libraryID, pickerPath)
		return movePickerLoadedMsg{dirs: entries, err: err}
	}
}

func (m model) updateMove(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		m.message = ""
		if c, o, ok := moveCursor(msg.String(), m.movePickerCursor, m.movePickerOffset, len(m.movePickerDirs), m.moveRows()); ok {
			m.movePickerCursor, m.movePickerOffset = c, o
			return m, nil
		}
		switch msg.String() {
		case "enter":
			if m.movePickerCursor < len(m.movePickerDirs) {
				// Navigate into selected directory
				dir := m.movePickerDirs[m.movePickerCursor]
				m.movePickerPath = path.Join(m.movePickerPath, dir.Name)
				m.movePickerCursor = 0
				return m, m.loadMoveDirs()
			}
		case "backspace", "h":
			if m.movePickerPath != "/" {
				m.movePickerPath = path.Dir(m.movePickerPath)
				m.movePickerCursor = 0
				return m, m.loadMoveDirs()
			}
		case " ":
			// Space = select current directory as destination
			srcName := path.Base(m.moveSrcPath)
			dst := path.Join(m.movePickerPath, srcName)
			if dst == m.moveSrcPath {
				m.message = errorStyle.Render("Cannot move to same location")
				return m, nil
			}
			libraryID := m.browseLibraryID
			src := m.moveSrcPath
			m.view = viewBrowse
			m.message = "Moving..."
			return m, func() tea.Msg {
				err := m.api.MoveFile(libraryID, src, dst)
				return moveDoneMsg{err: err}
			}
		case "esc":
			m.view = viewBrowse
			m.message = ""
			return m, nil
		}

	case movePickerLoadedMsg:
		if msg.err != nil {
			m.message = errorStyle.Render(msg.err.Error())
			return m, nil
		}
		// Filter to directories only
		m.movePickerDirs = nil
		for _, e := range msg.dirs {
			if e.Type == "dir" {
				m.movePickerDirs = append(m.movePickerDirs, e)
			}
		}

	case moveDoneMsg:
		if msg.err != nil {
			m.message = errorStyle.Render(msg.err.Error())
			return m, nil
		}
		m.view = viewBrowse
		m.result = successStyle.Render("Moved")
		return m, m.loadDir
	}

	return m, nil
}

func (m model) renderMove() string {
	from, to, counter := window(m.movePickerOffset, len(m.movePickerDirs), m.moveRows())
	header := []string{
		titleStyle.Render(fmt.Sprintf("Move %q", path.Base(m.moveSrcPath))) + counter,
		dimStyle.Render("Select destination: " + m.movePickerPath),
		"",
	}

	var body []string
	if len(m.movePickerDirs) == 0 {
		body = append(body, dimStyle.Render("  (no subdirectories)"))
	}
	for i := from; i < to; i++ {
		cursor := "  "
		name := m.movePickerDirs[i].Name + "/"
		if i == m.movePickerCursor {
			cursor = "> "
			name = selectedStyle.Render(name)
		}
		body = append(body, cursor+name)
	}

	return m.frame(header, body, m.footer(moveHeaderRows, moveHelp))
}

// --- Confirm Overwrite View ---

func (m model) updateConfirmOverwrite(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y":
			libraryID := m.browseLibraryID
			libraryPath := m.pendingDownloadLibraryPath
			localPath := m.pendingDownloadLocalPath
			m.view = viewBrowse
			m.message = "Downloading..."
			return m, func() tea.Msg {
				err := m.api.DownloadFile(libraryID, libraryPath, localPath)
				return downloadDoneMsg{err: err}
			}
		case "n", "N", "esc":
			m.view = viewBrowse
			m.message = ""
			return m, nil
		}
	}
	return m, nil
}

func (m model) renderConfirmOverwrite() string {
	header := []string{titleStyle.Render("File exists"), ""}
	body := []string{fmt.Sprintf("Overwrite local file %q?", m.pendingDownloadLocalPath)}
	return m.frame(header, body, m.footer(headerRows, confirmHelp))
}

// --- Layout ---

// Key help, one binding per item. The layout has to measure it: it wraps to
// the terminal width, and how many rows that takes decides how many rows are
// left for the list above it.
var (
	loginHelp     = []string{"tab: switch field", "enter: login", "ctrl+c: quit"}
	setupHelp     = []string{"tab: switch field", "enter: create account", "ctrl+c: quit"}
	librariesHelp = []string{"j/k: navigate", "g/G: top/bottom", "n: new", "d: delete", "r: refresh", "a: account", "q: quit"}
	accountHelp   = []string{"j/k: navigate", "enter: select", "esc: back", "q: quit"}
	passwordHelp  = []string{"tab: switch field", "enter: change password", "esc: cancel"}
	browseHelp    = []string{"j/k: navigate", "g/G: top/bottom", "enter: open/download", "u: upload", "m: mkdir", "r: rename", "v: move", "x: delete", "esc: back", "q: quit"}
	moveHelp      = []string{"j/k: navigate", "enter: open dir", "space: move here", "backspace: up", "esc: cancel"}
	confirmHelp   = []string{"y: yes", "n: no"}
	// The two halves of the library-delete confirmation: nothing to press
	// while it is still counting but escape, and no "y" once it knows there is
	// something in there.
	deleteWaitHelp    = []string{"esc: cancel"}
	deleteConfirmHelp = []string{"enter: delete", "esc: cancel"}
	createHelp        = []string{"enter: create", "esc: cancel"}
	uploadHelp        = []string{"enter: upload", "esc: cancel"}
	renameHelp        = []string{"enter: rename", "esc: cancel"}
)

// wrapWords packs items into lines no wider than width, joined by sep and
// broken only between whole items.
//
// The two callers below differ in what an item is and how they are joined,
// which is all they ever differed in: help lines break between bindings because
// breaking inside "r: rename" would read as two of them, and prose breaks
// between words. A width of zero -- the login screen before its first
// WindowSizeMsg -- wraps nothing, because a clipped line reads better than a
// column one word wide.
func wrapWords(items []string, sep string, width int) []string {
	if len(items) == 0 {
		return nil
	}
	var lines []string
	line := items[0]
	for _, item := range items[1:] {
		if width > 0 && lipgloss.Width(line)+lipgloss.Width(sep)+lipgloss.Width(item) > width {
			lines = append(lines, line)
			line = item
			continue
		}
		line += sep + item
	}
	return append(lines, line)
}

// wrapText breaks a sentence to width, at spaces. frame clips rather than
// wraps, so a sentence handed through unbroken loses its tail rather than
// gaining a line.
func wrapText(s string, width int) []string {
	lines := wrapWords(strings.Fields(s), " ", width)
	if lines == nil {
		// One empty line rather than none: a caller rendering prose is holding
		// a row for it either way.
		return []string{""}
	}
	return lines
}

// wrapHelp packs bindings into lines no wider than width.
func wrapHelp(items []string, width int) []string {
	return wrapWords(items, "  ", width)
}

// How many rows each screen spends on chrome above its body. The renderers
// and the scroll bookkeeping both read these, so both compute the same window.
const (
	headerRows      = 2 // title, blank
	moveHeaderRows  = 3 // title, destination, blank
	libraryItemRows = 2 // name line, id line
)

// bodyRows is what is left of the terminal once a screen's header and footer
// have taken theirs. Never less than one, so a tiny window degrades instead
// of computing a negative one.
func (m model) bodyRows(header, footer int) int {
	rows := m.height - header - footer
	if rows < 1 {
		return 1
	}
	return rows
}

// How many list items fit on each of the scrolling screens.
func (m model) librariesRows() int {
	return max(1, m.bodyRows(headerRows, len(m.footer(headerRows, librariesHelp)))/libraryItemRows)
}

func (m model) browseRows() int {
	return m.bodyRows(headerRows, len(m.footer(headerRows, browseHelp)))
}

func (m model) moveRows() int {
	return m.bodyRows(moveHeaderRows, len(m.footer(moveHeaderRows, moveHelp)))
}

// scrollTo slides offset the shortest distance that keeps cursor inside a
// window of rows over n items, and keeps that window inside the list.
func scrollTo(offset, cursor, n, rows int) int {
	if n <= rows {
		return 0
	}
	if cursor < offset {
		offset = cursor
	}
	if cursor >= offset+rows {
		offset = cursor - rows + 1
	}
	if offset > n-rows {
		offset = n - rows
	}
	if offset < 0 {
		offset = 0
	}
	return offset
}

// syncScroll re-derives every offset from the cursors, the list lengths and
// the current terminal size. Update runs it after each message, so growing
// the window, reloading a directory and moving the cursor all agree.
func (m model) syncScroll() model {
	m.librariesOffset = scrollTo(m.librariesOffset, m.cursor, len(m.libraries), m.librariesRows())
	m.browseOffset = scrollTo(m.browseOffset, m.browseCursor, len(m.dirEntries), m.browseRows())
	m.movePickerOffset = scrollTo(m.movePickerOffset, m.movePickerCursor, len(m.movePickerDirs), m.moveRows())
	return m
}

// moveCursor applies the list keys every scrolling screen shares — arrows,
// page, top and bottom — and reports whether it took the key.
//
// The arrows only move the cursor and leave the window to syncScroll, which
// scrolls the least it can. The paging and jump keys move the window as well,
// because a page key that slid the list by one row would not have paged.
func moveCursor(key string, cursor, offset, n, rows int) (int, int, bool) {
	switch key {
	case "up", "k":
		cursor--
	case "down", "j":
		cursor++
	case "pgup", "ctrl+b":
		cursor -= rows
		offset -= rows
	case "pgdown", "ctrl+f":
		cursor += rows
		offset += rows
	case "home", "g":
		cursor, offset = 0, 0
	case "end", "G":
		cursor, offset = n-1, n-rows
	default:
		return cursor, offset, false
	}
	return min(max(cursor, 0), max(n-1, 0)), max(offset, 0), true
}

// window is the half-open range of items to draw for a list scrolled to
// offset, plus a "3-11 of 40" counter for the title — empty while the whole
// list fits, so the counter only appears when there is something off screen.
func window(offset, n, rows int) (from, to int, counter string) {
	from = min(offset, max(n-1, 0))
	to = min(n, from+rows)
	if n > rows {
		counter = dimStyle.Render(fmt.Sprintf("  %d-%d of %d", from+1, to, n))
	}
	return from, to, counter
}

// footer is the bottom of every screen: the status message, the key help, and
// the server bar. The message row is always drawn, so a screen does not jump
// when a message arrives or clears.
//
// The help wraps to the terminal width, so on a narrow window it can be
// several rows and on a short one it does not fit at all. What has to give
// gives in this order: the padding row, then the help from the bottom up,
// then the message. The server bar is the line that always survives.
func (m model) footer(header int, help []string) []string {
	status := m.renderStatusBar()
	var helpLines []string
	for _, line := range wrapHelp(help, m.width) {
		helpLines = append(helpLines, helpStyle.Render(line))
	}

	// The body is owed at least one row; the rest of the screen is the
	// footer's to spend.
	budget := m.height - header - 1

	lines := append([]string{"", m.message}, helpLines...)
	lines = append(lines, status)
	if len(lines) <= budget {
		return lines
	}

	lines = append(append([]string{m.message}, helpLines...), status)
	for len(lines) > budget && len(lines) > 2 {
		lines = append(lines[:len(lines)-2], status)
	}
	if len(lines) > budget {
		lines = []string{status}
	}
	return lines
}

// frame lays a screen out as a fixed header, a body padded to fill whatever
// is left, and a footer on the last rows. Lines are clipped to the terminal
// width, because one wrapped line would push the footer off the bottom.
func (m model) frame(header, body, footer []string) string {
	avail := m.bodyRows(len(header), len(footer))
	if len(body) > avail {
		body = body[:avail]
	}

	lines := make([]string, 0, len(header)+avail+len(footer))
	lines = append(lines, header...)
	lines = append(lines, body...)
	for i := len(body); i < avail; i++ {
		lines = append(lines, "")
	}
	lines = append(lines, footer...)

	// A terminal too short for even the trimmed footer: draw what fits from
	// the top rather than scrolling the screen out from under itself.
	if m.height > 0 && len(lines) > m.height {
		lines = lines[:m.height]
	}

	if m.width > 0 {
		clip := lipgloss.NewStyle().MaxWidth(m.width)
		for i, line := range lines {
			lines[i] = clip.Render(line)
		}
	}
	return strings.Join(lines, "\n")
}

// --- Account View ---

// accountEmail is the address this session signed in as.
//
// Read back off the login field rather than out of the client, because the
// field is where it was typed and is also where an auto-login from the
// environment puts it -- one source, whichever way the session started.
func (m model) accountEmail() string {
	return m.emailInput.Value()
}

func (m model) updateAccount(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if c, _, ok := moveCursor(msg.String(), m.accountCursor, 0, len(accountItems), len(accountItems)); ok {
			m.accountCursor = c
			return m, nil
		}
		switch msg.String() {
		case "esc":
			m.view = viewLibraries
			m.message = ""
			return m, nil
		case "enter":
			if m.accountCursor >= len(accountItems) {
				return m, nil
			}
			if accountItems[m.accountCursor].view == viewPassword {
				return m.enterPasswordChange(), textinput.Blink
			}
		}
	}
	return m, nil
}

func (m model) renderAccount() string {
	header := []string{titleStyle.Render("Account"), ""}

	var body []string
	if email := m.accountEmail(); email != "" {
		body = append(body, dimStyle.Render("  "+email), "")
	}
	for i, item := range accountItems {
		cursor := "  "
		label := item.label
		if i == m.accountCursor {
			cursor = "> "
			label = selectedStyle.Render(label)
		}
		body = append(body, cursor+label)
	}
	return m.frame(header, body, m.footer(headerRows, accountHelp))
}

// --- Change Password View ---

// enterPasswordChange opens the form with all three fields empty.
//
// Emptied on the way in rather than on the way out, so that whatever ends the
// form -- escape, a success, a quit -- leaves nothing to remember to clear, and
// so that a password is never sitting in the model waiting to be redrawn on a
// screen somebody walked away from.
func (m model) enterPasswordChange() model {
	m.view = viewPassword
	m.currentPasswordInput.SetValue("")
	m.newPasswordInput.SetValue("")
	m.confirmPasswordInput.SetValue("")
	m.passwordFocus = 0
	m.focusPasswordField()
	m.message = ""
	return m
}

// focusPasswordField moves the cursor to whichever field passwordFocus names,
// and blurs the rest -- for the reason focusLoginField gives: two focused
// inputs each get every rune.
func (m *model) focusPasswordField() {
	m.currentPasswordInput.Blur()
	m.newPasswordInput.Blur()
	m.confirmPasswordInput.Blur()

	switch m.passwordFocus {
	case 0:
		m.currentPasswordInput.Focus()
	case 1:
		m.newPasswordInput.Focus()
	case 2:
		m.confirmPasswordInput.Focus()
	}
}

func (m model) updatePassword(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			m.view = viewAccount
			m.message = ""
			return m, nil

		case "tab", "down":
			m.passwordFocus = (m.passwordFocus + 1) % passwordFields
			m.focusPasswordField()
			return m, nil

		case "shift+tab", "up":
			m.passwordFocus = (m.passwordFocus + passwordFields - 1) % passwordFields
			m.focusPasswordField()
			return m, nil

		case "enter":
			current := m.currentPasswordInput.Value()
			next := m.newPasswordInput.Value()
			if current == "" || next == "" {
				m.message = errorStyle.Render("The current and new passwords are both required")
				return m, nil
			}
			// The confirmation field is not politeness. Both fields are
			// masked, so a mistyped new password is a password nobody knows:
			// the change succeeds, the server signs every session out, and
			// getting back in needs an operator with shell access and
			// `silo user passwd`.
			if next != m.confirmPasswordInput.Value() {
				m.message = errorStyle.Render("The new passwords do not match")
				return m, nil
			}
			m.message = "Changing the password..."
			return m, func() tea.Msg {
				revoked, err := m.api.ChangePassword(current, next)
				return passwordChangedMsg{revoked: revoked, err: err}
			}
		}

	}

	var cmds []tea.Cmd
	var cmd tea.Cmd
	m.currentPasswordInput, cmd = m.currentPasswordInput.Update(msg)
	cmds = append(cmds, cmd)
	m.newPasswordInput, cmd = m.newPasswordInput.Update(msg)
	cmds = append(cmds, cmd)
	m.confirmPasswordInput, cmd = m.confirmPasswordInput.Update(msg)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

// passwordChangedSummary counts this session out of the number the server gave.
//
// The server revokes every credential the account holds, with no carve-out for
// the one that asked, and its count says so. Reported as it stands, it would
// tell somebody who is still looking at a working library list that they have
// been signed out of it -- they have, and the client signed straight back in,
// which is not a thing worth explaining on a status row.
//
// "Places" rather than "sessions" for the rest of the number. What is signed
// out now includes mounted devices, and somebody who reads "sessions" will not
// connect the count to their drive asking for a password an hour later. See
// docs/auth.md § Changing a password, and what it revokes.
func passwordChangedSummary(revoked int) string {
	others := revoked - 1
	switch {
	case others < 1:
		return "Password changed"
	case others == 1:
		return "Password changed; signed out of 1 other place"
	default:
		return fmt.Sprintf("Password changed; signed out of %d other places", others)
	}
}

func (m model) renderPassword() string {
	header := []string{titleStyle.Render("Change password"), ""}

	body := wrapText(
		"Your current password is asked for even though you are signed in, so that a "+
			"credential somebody else is holding cannot turn itself into the account.",
		m.width)
	body = append(body, "")
	body = append(body, wrapText(
		"Everything signed in with this account is signed out, mounted devices included. "+
			"They will each ask for the new password.", m.width)...)

	body = append(body,
		"",
		"Current password:",
		m.currentPasswordInput.View(),
		"",
		"New password:",
		m.newPasswordInput.View(),
		"",
		"Confirm new password:",
		m.confirmPasswordInput.View(),
	)
	return m.frame(header, body, m.footer(headerRows, passwordHelp))
}

// --- Status bar ---

func (m model) renderStatusBar() string {
	if m.serverVersion == "" {
		return dimStyle.Render(m.serverURL)
	}
	return dimStyle.Render(fmt.Sprintf("%s (v%s)", m.serverURL, m.serverVersion))
}

// --- View dispatch ---

func (m model) View() string {
	switch m.view {
	case viewLogin:
		return m.renderLogin()
	case viewLibraries:
		return m.renderLibraries()
	case viewNewLibrary:
		return m.renderNewLibrary()
	case viewConfirm:
		return m.renderConfirm()
	case viewBrowse:
		return m.renderBrowse()
	case viewUpload:
		return m.renderUpload()
	case viewMkdir:
		return m.renderMkdir()
	case viewConfirmDelete:
		return m.renderConfirmDeleteFile()
	case viewConfirmOverwrite:
		return m.renderConfirmOverwrite()
	case viewRename:
		return m.renderRename()
	case viewMove:
		return m.renderMove()
	case viewAccount:
		return m.renderAccount()
	case viewPassword:
		return m.renderPassword()
	}
	return ""
}

// Run starts the Bubble Tea TUI. The caller supplies the server URL and
// optional auto-login credentials; if email and password are both non-empty,
// the TUI skips the login view and signs in on startup.
func Run(serverURL, autoEmail, autoPassword string) error {
	p := tea.NewProgram(initialModel(serverURL, autoEmail, autoPassword), tea.WithAltScreen())
	final, err := p.Run()
	if m, ok := final.(model); ok && m.watcher != nil {
		m.watcher.Close()
	}
	return err
}
