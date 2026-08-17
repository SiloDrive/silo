package tui

import (
	"fmt"
	"os"
	"path"
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
type uploadDoneMsg struct{ err error }
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
	repos   []client.Repo
	cursor  int
	message string // status message

	// New repo
	newRepoInput textinput.Model

	// Browse
	browseRepoID   string
	browseRepoName string
	browsePath     string
	dirEntries     []client.DirEntry
	browseCursor   int

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
	upload.Placeholder = "/path/to/local/file"
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
			if m.view == viewLogin || m.view == viewRepos || m.view == viewBrowse {
				return m, tea.Quit
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	}

	switch m.view {
	case viewLogin:
		return m.updateLogin(msg)
	case viewRepos:
		return m.updateRepos(msg)
	case viewNewRepo:
		return m.updateNewRepo(msg)
	case viewConfirm:
		return m.updateConfirm(msg)
	case viewBrowse:
		return m.updateBrowse(msg)
	case viewUpload:
		return m.updateUpload(msg)
	case viewMkdir:
		return m.updateMkdir(msg)
	case viewConfirmDelete:
		return m.updateConfirmDeleteFile(msg)
	case viewConfirmOverwrite:
		return m.updateConfirmOverwrite(msg)
	case viewRename:
		return m.updateRename(msg)
	case viewMove:
		return m.updateMove(msg)
	}

	return m, nil
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

	case serverInfoMsg:
		m.serverVersion = msg.version
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
	var b strings.Builder
	b.WriteString(titleStyle.Render("Silo Login") + "\n\n")
	b.WriteString("Email:\n")
	b.WriteString(m.emailInput.View() + "\n\n")
	b.WriteString("Password:\n")
	b.WriteString(m.passwordInput.View() + "\n\n")
	if m.message != "" {
		b.WriteString(m.message + "\n\n")
	}
	b.WriteString(helpStyle.Render("tab: switch field  enter: login  q: quit"))
	return b.String()
}

// --- Repos View ---

func (m model) loadRepos() tea.Msg {
	repos, err := m.api.ListRepos()
	return reposLoadedMsg{repos: repos, err: err}
}

func (m model) updateRepos(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.repos)-1 {
				m.cursor++
			}
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
		m.message = ""
		if m.cursor >= len(m.repos) {
			m.cursor = max(0, len(m.repos)-1)
		}
	}

	return m, nil
}

func (m model) renderRepos() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Libraries") + "\n\n")

	if len(m.repos) == 0 {
		b.WriteString(dimStyle.Render("  No libraries yet. Press 'n' to create one.") + "\n")
	}

	for i, repo := range m.repos {
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
		fmt.Fprintf(&b, "%s%s%s%s\n", cursor, name, ts, encrypted)
		b.WriteString(dimStyle.Render(fmt.Sprintf("    %s", repo.ID)) + "\n")
	}

	b.WriteString("\n")
	if m.message != "" {
		b.WriteString(m.message + "\n")
	}
	b.WriteString(helpStyle.Render("j/k: navigate  n: new  d: delete  r: refresh  q: quit"))
	return b.String()
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
		m.message = successStyle.Render("Library created")
		return m, m.loadRepos
	}

	var cmd tea.Cmd
	m.newRepoInput, cmd = m.newRepoInput.Update(msg)
	return m, cmd
}

func (m model) renderNewRepo() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Create Library") + "\n\n")
	b.WriteString("Name:\n")
	b.WriteString(m.newRepoInput.View() + "\n\n")
	if m.message != "" {
		b.WriteString(m.message + "\n\n")
	}
	b.WriteString(helpStyle.Render("enter: create  esc: cancel"))
	return b.String()
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
		m.message = successStyle.Render("Library deleted")
		return m, m.loadRepos
	}

	return m, nil
}

func (m model) renderConfirm() string {
	var b strings.Builder
	name := "(unnamed)"
	if m.cursor < len(m.repos) && m.repos[m.cursor].Name != "" {
		name = m.repos[m.cursor].Name
	}
	b.WriteString(titleStyle.Render("Delete Library") + "\n\n")
	fmt.Fprintf(&b, "Are you sure you want to delete %q?\n\n", name)
	b.WriteString(helpStyle.Render("y: yes  n: no"))
	return b.String()
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
		switch msg.String() {
		case "up", "k":
			if m.browseCursor > 0 {
				m.browseCursor--
			}
		case "down", "j":
			if m.browseCursor < len(m.dirEntries)-1 {
				m.browseCursor++
			}
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
		m.message = ""
		if m.browseCursor >= len(m.dirEntries) {
			m.browseCursor = max(0, len(m.dirEntries)-1)
		}
	}

	return m, nil
}

func (m model) renderBrowse() string {
	var b strings.Builder

	// Breadcrumb
	breadcrumb := m.browseRepoName + " " + m.browsePath
	b.WriteString(titleStyle.Render(breadcrumb) + "\n\n")

	if len(m.dirEntries) == 0 {
		b.WriteString(dimStyle.Render("  (empty directory)") + "\n")
	}

	for i, entry := range m.dirEntries {
		cursor := "  "
		if i == m.browseCursor {
			cursor = "> "
		}

		if entry.Type == "dir" {
			name := entry.Name + "/"
			if i == m.browseCursor {
				name = selectedStyle.Render(name)
			}
			ts := ""
			if entry.Mtime > 0 {
				ts = dimStyle.Render("  " + time.Unix(entry.Mtime, 0).Format("2006-01-02 15:04"))
			}
			fmt.Fprintf(&b, "%s%s%s\n", cursor, name, ts)
		} else {
			name := entry.Name
			if i == m.browseCursor {
				name = selectedStyle.Render(name)
			}
			size := dimStyle.Render(format.Bytes(entry.Size))
			ts := ""
			if entry.Mtime > 0 {
				ts = dimStyle.Render("  " + time.Unix(entry.Mtime, 0).Format("2006-01-02 15:04"))
			}
			fmt.Fprintf(&b, "%s%s  %s%s\n", cursor, name, size, ts)
		}
	}

	b.WriteString("\n")
	if m.message != "" {
		b.WriteString(m.message + "\n")
	}
	b.WriteString(helpStyle.Render("j/k: navigate  enter: open/download  u: upload  m: mkdir  r: rename  v: move  x: delete  q: quit"))
	return b.String()
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
			localPath := m.uploadInput.Value()
			if localPath == "" {
				m.message = "File path is required"
				return m, nil
			}
			m.message = "Uploading..."
			repoID := m.browseRepoID
			parentDir := m.browsePath
			return m, func() tea.Msg {
				err := m.api.UploadFile(repoID, parentDir, localPath)
				return uploadDoneMsg{err: err}
			}
		}

	case uploadDoneMsg:
		if msg.err != nil {
			m.message = errorStyle.Render(msg.err.Error())
			return m, nil
		}
		m.view = viewBrowse
		m.message = successStyle.Render("File uploaded")
		return m, m.loadDir
	}

	var cmd tea.Cmd
	m.uploadInput, cmd = m.uploadInput.Update(msg)
	return m, cmd
}

func (m model) renderUpload() string {
	var b strings.Builder
	breadcrumb := m.browseRepoName + " " + m.browsePath
	b.WriteString(titleStyle.Render("Upload to "+breadcrumb) + "\n\n")
	b.WriteString("Local file path:\n")
	b.WriteString(m.uploadInput.View() + "\n\n")
	if m.message != "" {
		b.WriteString(m.message + "\n\n")
	}
	b.WriteString(helpStyle.Render("enter: upload  esc: cancel"))
	return b.String()
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
		m.message = successStyle.Render("Directory created")
		return m, m.loadDir
	}

	var cmd tea.Cmd
	m.mkdirInput, cmd = m.mkdirInput.Update(msg)
	return m, cmd
}

func (m model) renderMkdir() string {
	var b strings.Builder
	breadcrumb := m.browseRepoName + " " + m.browsePath
	b.WriteString(titleStyle.Render("Create directory in "+breadcrumb) + "\n\n")
	b.WriteString("Directory name:\n")
	b.WriteString(m.mkdirInput.View() + "\n\n")
	if m.message != "" {
		b.WriteString(m.message + "\n\n")
	}
	b.WriteString(helpStyle.Render("enter: create  esc: cancel"))
	return b.String()
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
		m.message = successStyle.Render("Deleted")
		return m, m.loadDir
	}

	return m, nil
}

func (m model) renderConfirmDeleteFile() string {
	var b strings.Builder
	name := "(unknown)"
	if m.browseCursor < len(m.dirEntries) {
		name = m.dirEntries[m.browseCursor].Name
	}
	b.WriteString(titleStyle.Render("Delete") + "\n\n")
	fmt.Fprintf(&b, "Are you sure you want to delete %q?\n\n", name)
	b.WriteString(helpStyle.Render("y: yes  n: no"))
	return b.String()
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
		m.message = successStyle.Render("Renamed")
		return m, m.loadDir
	}

	var cmd tea.Cmd
	m.renameInput, cmd = m.renameInput.Update(msg)
	return m, cmd
}

func (m model) renderRename() string {
	var b strings.Builder
	name := m.dirEntries[m.browseCursor].Name
	b.WriteString(titleStyle.Render(fmt.Sprintf("Rename \"%s\"", name)) + "\n\n")
	b.WriteString("New name:\n")
	b.WriteString(m.renameInput.View() + "\n\n")
	if m.message != "" {
		b.WriteString(m.message + "\n\n")
	}
	b.WriteString(helpStyle.Render("enter: rename  esc: cancel"))
	return b.String()
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
		switch msg.String() {
		case "up", "k":
			if m.movePickerCursor > 0 {
				m.movePickerCursor--
			}
		case "down", "j":
			if m.movePickerCursor < len(m.movePickerDirs)-1 {
				m.movePickerCursor++
			}
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
		m.message = successStyle.Render("Moved")
		return m, m.loadDir
	}

	return m, nil
}

func (m model) renderMove() string {
	var b strings.Builder

	srcName := path.Base(m.moveSrcPath)
	b.WriteString(titleStyle.Render(fmt.Sprintf("Move \"%s\"", srcName)) + "\n")
	b.WriteString(dimStyle.Render("Select destination: "+m.movePickerPath) + "\n\n")

	if len(m.movePickerDirs) == 0 {
		b.WriteString(dimStyle.Render("  (no subdirectories)") + "\n")
	}

	for i, dir := range m.movePickerDirs {
		cursor := "  "
		if i == m.movePickerCursor {
			cursor = "> "
		}
		name := dir.Name + "/"
		if i == m.movePickerCursor {
			name = selectedStyle.Render(name)
		}
		fmt.Fprintf(&b, "%s%s\n", cursor, name)
	}

	b.WriteString("\n")
	if m.message != "" {
		b.WriteString(m.message + "\n")
	}
	b.WriteString(helpStyle.Render("j/k: navigate  enter: open dir  space: move here  backspace: up  esc: cancel"))
	return b.String()
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
	var b strings.Builder
	b.WriteString(titleStyle.Render("File exists") + "\n\n")
	fmt.Fprintf(&b, "Overwrite local file %q?\n\n", m.pendingDownloadLocalPath)
	b.WriteString(helpStyle.Render("y: yes  n: no"))
	return b.String()
}

// --- Status bar ---

func (m model) renderStatusBar() string {
	version := m.serverVersion
	if version == "" {
		version = "?"
	}
	return dimStyle.Render(fmt.Sprintf("%s (v%s)", m.serverURL, version))
}

// --- View dispatch ---

func (m model) View() string {
	var content string
	switch m.view {
	case viewLogin:
		return m.renderLogin()
	case viewRepos:
		content = m.renderRepos()
	case viewNewRepo:
		content = m.renderNewRepo()
	case viewConfirm:
		content = m.renderConfirm()
	case viewBrowse:
		content = m.renderBrowse()
	case viewUpload:
		content = m.renderUpload()
	case viewMkdir:
		content = m.renderMkdir()
	case viewConfirmDelete:
		content = m.renderConfirmDeleteFile()
	case viewConfirmOverwrite:
		content = m.renderConfirmOverwrite()
	case viewRename:
		content = m.renderRename()
	case viewMove:
		content = m.renderMove()
	default:
		return ""
	}
	return content + "\n" + m.renderStatusBar()
}

// Run starts the Bubble Tea TUI. The caller supplies the server URL and
// optional auto-login credentials; if email and password are both non-empty,
// the TUI skips the login view and signs in on startup.
func Run(serverURL, autoEmail, autoPassword string) error {
	p := tea.NewProgram(initialModel(serverURL, autoEmail, autoPassword), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
