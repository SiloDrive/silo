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
	viewRepos            = "repos"
	viewNewRepo          = "new_repo"
	viewConfirm          = "confirm_delete"
	viewBrowse           = "browse"
	viewUpload           = "upload"
	viewMkdir            = "mkdir"
	viewConfirmDelete    = "confirm_delete_file"
	viewConfirmOverwrite = "confirm_overwrite"
	viewRename           = "rename"
	viewMove             = "move"
)

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
type reposLoadedMsg struct {
	repos []client.Repo
	err   error
}
type repoCreatedMsg struct{ err error }
type repoDeletedMsg struct{ err error }
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
type movePickerLoadedMsg struct {
	dirs []client.DirEntry
	err  error
}
type serverInfoMsg struct {
	version string
}

type model struct {
	api  *client.APIClient
	view string

	// Login
	emailInput    textinput.Model
	passwordInput textinput.Model
	loginFocus    int // 0=email, 1=password

	// Repos
	repos       []client.Repo
	cursor      int
	reposOffset int    // index of the first library drawn
	message     string // status message
	// result is what to say once the reload a write kicks off has finished.
	// Put in message directly it would be gone before it was read: the reload
	// lands a moment later and clears the row it was written to.
	result string

	// New repo
	newRepoInput textinput.Model

	// Browse
	browseRepoID   string
	browseRepoName string
	browsePath     string
	dirEntries     []client.DirEntry
	browseCursor   int
	browseOffset   int // index of the first entry drawn

	// Upload
	uploadInput textinput.Model

	// Mkdir
	mkdirInput textinput.Model

	// Rename
	renameInput textinput.Model

	// Move (remote directory picker)
	moveSrcPath      string
	movePickerPath   string
	movePickerDirs   []client.DirEntry
	movePickerCursor int
	movePickerOffset int // index of the first directory drawn

	// Pending download (for overwrite confirmation)
	pendingDownloadRepoPath  string
	pendingDownloadLocalPath string

	// Auto-login from env
	autoEmail    string
	autoPassword string

	// Server info
	serverURL     string
	serverVersion string

	width  int
	height int
}

func (m model) fetchServerInfo() tea.Msg {
	info, _ := m.api.GetServerInfo()
	return serverInfoMsg{version: info.Version}
}

// entryPath builds a full repo path from the current browse path and an entry name.
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

	newRepo := textinput.New()
	newRepo.Placeholder = "Library name"
	newRepo.CharLimit = 255

	upload := textinput.New()
	upload.Placeholder = "/path/to/local/file-or-directory"
	upload.CharLimit = 1024

	mkdirIn := textinput.New()
	mkdirIn.Placeholder = "directory name"
	mkdirIn.CharLimit = 255

	renameIn := textinput.New()
	renameIn.Placeholder = "new name"
	renameIn.CharLimit = 255

	m := model{
		api:           client.NewClient(serverURL),
		view:          viewLogin,
		emailInput:    email,
		passwordInput: password,
		newRepoInput:  newRepo,
		uploadInput:   upload,
		mkdirInput:    mkdirIn,
		renameInput:   renameIn,
		autoEmail:     autoEmail,
		autoPassword:  autoPassword,
		serverURL:     serverURL,
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
		return func() tea.Msg {
			err := m.api.Login(m.autoEmail, m.autoPassword)
			return loginDoneMsg{err: err}
		}
	}
	return textinput.Blink
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
			if m.view == viewRepos || m.view == viewBrowse {
				return m, tea.Quit
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	// The server answers after login has already handed the screen to the
	// library list, so this cannot live in the login view's update.
	case serverInfoMsg:
		m.serverVersion = msg.version
	}

	var (
		next tea.Model
		cmd  tea.Cmd
	)
	switch m.view {
	case viewLogin:
		next, cmd = m.updateLogin(msg)
	case viewRepos:
		next, cmd = m.updateRepos(msg)
	case viewNewRepo:
		next, cmd = m.updateNewRepo(msg)
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

func (m model) updateLogin(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "tab", "shift+tab", "down", "up":
			m.loginFocus = (m.loginFocus + 1) % 2
			if m.loginFocus == 0 {
				m.emailInput.Focus()
				m.passwordInput.Blur()
			} else {
				m.emailInput.Blur()
				m.passwordInput.Focus()
			}
			return m, nil

		case "enter":
			email := m.emailInput.Value()
			password := m.passwordInput.Value()
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
		m.view = viewRepos
		m.message = ""
		return m, tea.Batch(m.loadRepos, m.fetchServerInfo)
	}

	var cmds []tea.Cmd
	var cmd tea.Cmd
	m.emailInput, cmd = m.emailInput.Update(msg)
	cmds = append(cmds, cmd)
	m.passwordInput, cmd = m.passwordInput.Update(msg)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

func (m model) renderLogin() string {
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

// --- Repos View ---

func (m model) loadRepos() tea.Msg {
	repos, err := m.api.ListRepos()
	return reposLoadedMsg{repos: repos, err: err}
}

func (m model) updateRepos(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if c, o, ok := moveCursor(msg.String(), m.cursor, m.reposOffset, len(m.repos), m.reposRows()); ok {
			m.cursor, m.reposOffset = c, o
			return m, nil
		}
		switch msg.String() {
		case "n":
			m.view = viewNewRepo
			m.newRepoInput.SetValue("")
			m.newRepoInput.Focus()
			m.message = ""
			return m, textinput.Blink
		case "d":
			if len(m.repos) > 0 {
				m.view = viewConfirm
				m.message = ""
			}
		case "r":
			m.message = "Refreshing..."
			return m, m.loadRepos
		case "enter":
			if len(m.repos) > 0 {
				repo := m.repos[m.cursor]
				m.browseRepoID = repo.ID
				m.browseRepoName = repo.Name
				m.browsePath = "/"
				m.browseCursor = 0
				m.view = viewBrowse
				m.message = ""
				return m, m.loadDir
			}
		}

	case reposLoadedMsg:
		if msg.err != nil {
			m.message = errorStyle.Render(msg.err.Error())
			return m, nil
		}
		m.repos = msg.repos
		m.message, m.result = m.result, ""
		if m.cursor >= len(m.repos) {
			m.cursor = max(0, len(m.repos)-1)
		}
	}

	return m, nil
}

func (m model) renderRepos() string {
	from, to, counter := window(m.reposOffset, len(m.repos), m.reposRows())
	header := []string{titleStyle.Render("Libraries") + counter, ""}

	var body []string
	if len(m.repos) == 0 {
		body = append(body, dimStyle.Render("  No libraries yet. Press 'n' to create one."))
	}
	for i := from; i < to; i++ {
		repo := m.repos[i]
		cursor := "  "
		name := repo.Name
		if repo.Name == "" {
			name = "(unnamed)"
		}
		if i == m.cursor {
			cursor = "> "
			name = selectedStyle.Render(name)
		}
		ts := ""
		if repo.UpdateTime > 0 {
			ts = dimStyle.Render(" " + time.Unix(repo.UpdateTime, 0).Format("2006-01-02 15:04"))
		}
		encrypted := ""
		if repo.Encrypted {
			encrypted = dimStyle.Render(" [encrypted]")
		}
		body = append(body, cursor+name+ts+encrypted, dimStyle.Render("    "+repo.ID))
	}

	return m.frame(header, body, m.footer(headerRows, reposHelp))
}

// --- New Repo View ---

func (m model) updateNewRepo(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			m.view = viewRepos
			return m, nil
		case "enter":
			name := m.newRepoInput.Value()
			if name == "" {
				m.message = "Name is required"
				return m, nil
			}
			m.message = "Creating..."
			return m, func() tea.Msg {
				_, err := m.api.CreateRepo(name)
				return repoCreatedMsg{err: err}
			}
		}

	case repoCreatedMsg:
		if msg.err != nil {
			m.message = errorStyle.Render(msg.err.Error())
			return m, nil
		}
		m.view = viewRepos
		m.result = successStyle.Render("Library created")
		return m, m.loadRepos
	}

	var cmd tea.Cmd
	m.newRepoInput, cmd = m.newRepoInput.Update(msg)
	return m, cmd
}

func (m model) renderNewRepo() string {
	header := []string{titleStyle.Render("Create Library"), ""}
	body := []string{"Name:", m.newRepoInput.View()}
	return m.frame(header, body, m.footer(headerRows, createHelp))
}

// --- Confirm Delete View ---

func (m model) updateConfirm(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y":
			repo := m.repos[m.cursor]
			m.message = "Deleting..."
			return m, func() tea.Msg {
				err := m.api.DeleteRepo(repo.ID)
				return repoDeletedMsg{err: err}
			}
		case "n", "N", "esc":
			m.view = viewRepos
			return m, nil
		}

	case repoDeletedMsg:
		if msg.err != nil {
			m.view = viewRepos
			m.message = errorStyle.Render(msg.err.Error())
			return m, nil
		}
		m.view = viewRepos
		m.result = successStyle.Render("Library deleted")
		return m, m.loadRepos
	}

	return m, nil
}

func (m model) renderConfirm() string {
	name := "(unnamed)"
	if m.cursor < len(m.repos) && m.repos[m.cursor].Name != "" {
		name = m.repos[m.cursor].Name
	}
	header := []string{titleStyle.Render("Delete Library"), ""}
	body := []string{fmt.Sprintf("Are you sure you want to delete %q?", name)}
	return m.frame(header, body, m.footer(headerRows, confirmHelp))
}

// --- Browse View ---

func (m model) loadDir() tea.Msg {
	entries, err := m.api.ListDir(m.browseRepoID, m.browsePath)
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
				repoPath := m.entryPath(entry.Name)
				localPath := entry.Name

				// Check if local file exists
				if _, err := os.Stat(localPath); err == nil {
					m.pendingDownloadRepoPath = repoPath
					m.pendingDownloadLocalPath = localPath
					m.view = viewConfirmOverwrite
					m.message = ""
					return m, nil
				}

				m.message = "Downloading..."
				repoID := m.browseRepoID
				return m, func() tea.Msg {
					err := m.api.DownloadFile(repoID, repoPath, localPath)
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
				repoID := m.browseRepoID
				return m, func() tea.Msg {
					entries, err := m.api.ListDir(repoID, "/")
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
			m.view = viewRepos
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
	header := []string{titleStyle.Render(m.browseRepoName+" "+m.browsePath) + counter, ""}

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
		body = append(body, cursor+name+"  "+dimStyle.Render(format.Bytes(entry.Size))+ts)
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

			repoID := m.browseRepoID
			parentDir := m.browsePath
			if info.IsDir() {
				m.message = "Uploading directory..."
				return m, func() tea.Msg {
					up, err := m.api.UploadDir(repoID, parentDir, localPath, nil)
					return uploadDoneMsg{err: err, summary: uploadSummary(up)}
				}
			}
			m.message = "Uploading..."
			return m, func() tea.Msg {
				err := m.api.UploadFile(repoID, parentDir, localPath)
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
	header := []string{titleStyle.Render("Upload to " + m.browseRepoName + " " + m.browsePath), ""}
	body := []string{
		"Local file or directory:",
		m.uploadInput.View(),
		"",
		dimStyle.Render("A directory is uploaded whole, keeping its own name."),
	}
	return m.frame(header, body, m.footer(headerRows, uploadHelp))
}

// uploadSummary says what a directory upload did in one line. Blocks held back
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
			repoID := m.browseRepoID
			m.message = "Creating directory..."
			return m, func() tea.Msg {
				err := m.api.Mkdir(repoID, fullPath)
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
	header := []string{titleStyle.Render("Create directory in " + m.browseRepoName + " " + m.browsePath), ""}
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
			repoID := m.browseRepoID
			m.message = "Deleting..."
			return m, func() tea.Msg {
				err := m.api.DeleteFile(repoID, filePath)
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
			repoID := m.browseRepoID
			m.message = "Renaming..."
			return m, func() tea.Msg {
				err := m.api.RenameFile(repoID, filePath, newName)
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
	repoID := m.browseRepoID
	pickerPath := m.movePickerPath
	return func() tea.Msg {
		entries, err := m.api.ListDir(repoID, pickerPath)
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
			repoID := m.browseRepoID
			src := m.moveSrcPath
			m.view = viewBrowse
			m.message = "Moving..."
			return m, func() tea.Msg {
				err := m.api.MoveFile(repoID, src, dst)
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
			repoID := m.browseRepoID
			repoPath := m.pendingDownloadRepoPath
			localPath := m.pendingDownloadLocalPath
			m.view = viewBrowse
			m.message = "Downloading..."
			return m, func() tea.Msg {
				err := m.api.DownloadFile(repoID, repoPath, localPath)
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
	loginHelp   = []string{"tab: switch field", "enter: login", "ctrl+c: quit"}
	reposHelp   = []string{"j/k: navigate", "g/G: top/bottom", "n: new", "d: delete", "r: refresh", "q: quit"}
	browseHelp  = []string{"j/k: navigate", "g/G: top/bottom", "enter: open/download", "u: upload", "m: mkdir", "r: rename", "v: move", "x: delete", "esc: back", "q: quit"}
	moveHelp    = []string{"j/k: navigate", "enter: open dir", "space: move here", "backspace: up", "esc: cancel"}
	confirmHelp = []string{"y: yes", "n: no"}
	createHelp  = []string{"enter: create", "esc: cancel"}
	uploadHelp  = []string{"enter: upload", "esc: cancel"}
	renameHelp  = []string{"enter: rename", "esc: cancel"}
)

// wrapHelp packs bindings into lines no wider than width, breaking only
// between them. A general-purpose word wrap breaks inside "r: rename", which
// then reads as two bindings.
func wrapHelp(items []string, width int) []string {
	if len(items) == 0 {
		return nil
	}
	var lines []string
	line := items[0]
	for _, item := range items[1:] {
		if width > 0 && lipgloss.Width(line)+2+lipgloss.Width(item) > width {
			lines = append(lines, line)
			line = item
			continue
		}
		line += "  " + item
	}
	return append(lines, line)
}

// How many rows each screen spends on chrome above its body. The renderers
// and the scroll bookkeeping both read these, so both compute the same window.
const (
	headerRows     = 2 // title, blank
	moveHeaderRows = 3 // title, destination, blank
	repoItemRows   = 2 // name line, id line
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
func (m model) reposRows() int {
	return max(1, m.bodyRows(headerRows, len(m.footer(headerRows, reposHelp)))/repoItemRows)
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
	m.reposOffset = scrollTo(m.reposOffset, m.cursor, len(m.repos), m.reposRows())
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
	case viewRepos:
		return m.renderRepos()
	case viewNewRepo:
		return m.renderNewRepo()
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
	}
	return ""
}

// Run starts the Bubble Tea TUI. The caller supplies the server URL and
// optional auto-login credentials; if email and password are both non-empty,
// the TUI skips the login view and signs in on startup.
func Run(serverURL, autoEmail, autoPassword string) error {
	p := tea.NewProgram(initialModel(serverURL, autoEmail, autoPassword), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
