package tui

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/kaanemec/tork-w-debrid/internal/debrid"
	"github.com/kaanemec/tork-w-debrid/internal/debrid/realdebrid"
)

// Settings › Real-Debrid (J2.4): link the account with the device-code flow,
// see it, switch "use for downloads" and "delete cloud copy after download",
// and unlink it.
//
//	Real-Debrid
//	▍ account                           kaan · premium, 112 days left    x unlink
//	  use for downloads                 on                           space toggle
//	  delete cloud copy after download  off                          space toggle

type rdPhase int

const (
	rdUnknown    rdPhase = iota
	rdNotLinked          // no debrid.json
	rdLinked             // a usable link
	rdNeedsLogin         // Real-Debrid rejected a refresh
	rdBroken             // debrid.json can't be used (unreadable, insecure)
)

// rdSettings is what the Real-Debrid section knows about the account.
type rdSettings struct {
	loaded     bool
	phase      rdPhase
	problem    string // why the link can't be used (rdBroken)
	account    debrid.Account
	haveAcct   bool
	checking   bool // an account lookup is running
	accountErr string
	gen        int // account lookups and logins; older answers are dropped

	// endpoints and now are for tests (a fake server, a fixed clock).
	endpoints realdebrid.Endpoints
	now       func() time.Time
}

func (a *App) rdNow() time.Time {
	if a.settings.rd.now != nil {
		return a.settings.rd.now()
	}
	return time.Now()
}

func init() {
	registerSettingSection(10, func(a *App) settingSection {
		if a.cfg == nil {
			return settingSection{}
		}
		rows := []settingRow{rdAccountRow{}}
		if p := a.settings.rd.phase; p == rdLinked || p == rdNeedsLogin {
			rows = append(rows, rdUseRow{}, rdDeleteRow{})
		}
		return settingSection{title: "Real-Debrid", rows: rows}
	})
	settingsOpeners = append(settingsOpeners, func(a *App) tea.Cmd { return a.loadRDSettings() })
}

// loadDebridStatus sets the footer's ☁ RD marker from debrid.json and
// debrid.enabled. It reads one small file and makes no request.
func (a *App) loadDebridStatus() {
	if a.cfg == nil || a.cfg.Dir() == "" {
		return
	}
	l, err := debrid.NewStore(a.cfg).Load()
	a.SetDebridStatus(err == nil && l.Usable(), a.cfg.Debrid.Enabled)
}

// loadRDSettings reads the link and, when there is one, looks the account up
// in the background ("checking…" until it answers).
func (a *App) loadRDSettings() tea.Cmd {
	rd := &a.settings.rd
	rd.loaded = true
	rd.problem, rd.accountErr = "", ""
	l, err := debrid.NewStore(a.cfg).Load()
	switch {
	case errors.Is(err, debrid.ErrNotLinked):
		rd.phase = rdNotLinked
	case err != nil:
		rd.phase, rd.problem = rdBroken, err.Error()
	case l.NeedsLogin || !l.Usable():
		rd.phase = rdNeedsLogin
	default:
		rd.phase = rdLinked
	}
	a.SetDebridStatus(rd.phase == rdLinked, a.cfg.Debrid.Enabled)
	if rd.phase != rdLinked {
		return nil
	}
	return a.lookUpAccount()
}

type rdAccountMsg struct {
	gen     int
	account debrid.Account
	err     error
}

func (a *App) lookUpAccount() tea.Cmd {
	rd := &a.settings.rd
	rd.gen++
	rd.checking = true
	gen, cfg, ep := rd.gen, a.cfg, rd.endpoints
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		client, _ := realdebrid.NewFromConfig(cfg, ep)
		acct, err := client.Account(ctx)
		return rdAccountMsg{gen: gen, account: acct, err: err}
	}
}

func (m rdAccountMsg) applySettings(a *App) tea.Cmd {
	rd := &a.settings.rd
	if m.gen != rd.gen {
		return nil
	}
	rd.checking = false
	switch {
	case m.err == nil:
		rd.account, rd.haveAcct, rd.accountErr = m.account, true, ""
	case errors.Is(m.err, debrid.ErrNotLinked):
		rd.phase = rdNeedsLogin
		a.SetDebridStatus(false, a.cfg.Debrid.Enabled)
	default:
		rd.accountErr = debrid.Message(m.err)
	}
	return nil
}

// accountText is "kaan · premium, 112 days left".
func accountText(acct debrid.Account, now time.Time) string {
	var plan string
	switch {
	case !acct.Premium:
		plan = "not premium"
	case acct.Expiry.IsZero():
		plan = "premium"
	default:
		days := acct.DaysLeft(now)
		plan = fmt.Sprintf("premium, %d days left", days)
		if days == 1 {
			plan = "premium, 1 day left"
		}
	}
	if acct.Username == "" {
		return plan
	}
	return acct.Username + " · " + plan
}

// rdAccountRow is the account line; what it says and does follows the link.
type rdAccountRow struct{}

func (rdAccountRow) label() string { return "account" }

func (rdAccountRow) value(a *App) string {
	rd := &a.settings.rd
	switch rd.phase {
	case rdNotLinked:
		return styleDim.Render("not linked")
	case rdNeedsLogin:
		return styleWarn.Render("Real-Debrid asks you to log in again")
	case rdBroken:
		return styleErr.Render(rd.problem)
	case rdLinked:
		switch {
		case rd.haveAcct:
			text := routeBadge(accountText(rd.account, a.rdNow()))
			if !rd.account.Premium {
				text = routeBadge(rd.account.Username) + styleWarn.Render(" · not premium")
			}
			return text
		case rd.accountErr != "":
			return styleRoute.Render("linked") + styleDim.Render(" · "+rd.accountErr)
		}
	}
	return styleDim.Render("checking…")
}

func (rdAccountRow) hint(a *App) string {
	switch a.settings.rd.phase {
	case rdNotLinked:
		return "enter link account"
	case rdNeedsLogin:
		return "enter link again"
	case rdLinked, rdBroken:
		return "x unlink"
	}
	return ""
}

func (rdAccountRow) keys(a *App) []keyHint {
	switch a.settings.rd.phase {
	case rdNotLinked:
		return []keyHint{onStrip("enter", "link account")}
	case rdNeedsLogin:
		return []keyHint{onStrip("enter", "link again"), onStrip("x", "unlink")}
	case rdLinked, rdBroken:
		return []keyHint{onStrip("x", "unlink"), inCard("r", "check the account again")}
	}
	return nil
}

func (rdAccountRow) update(a *App, key tea.KeyMsg) (bool, tea.Cmd) {
	rd := &a.settings.rd
	switch key.String() {
	case "enter":
		if rd.phase == rdNotLinked || rd.phase == rdNeedsLogin {
			return true, a.startRDLink()
		}
	case "x":
		if rd.phase == rdNotLinked || rd.phase == rdUnknown {
			return false, nil
		}
		a.settings.confirm = &settingConfirm{question: "unlink Real-Debrid on this machine?", yes: (*App).unlinkRD}
		return true, nil
	case "r":
		if rd.phase == rdLinked {
			return true, a.lookUpAccount()
		}
	}
	return false, nil
}

func (a *App) unlinkRD() tea.Cmd {
	if err := debrid.NewStore(a.cfg).Remove(); err != nil {
		return a.showError("unlink failed: " + err.Error())
	}
	rd := &a.settings.rd
	rd.gen++
	rd.phase, rd.account, rd.haveAcct, rd.accountErr, rd.checking = rdNotLinked, debrid.Account{}, false, "", false
	a.SetDebridStatus(false, a.cfg.Debrid.Enabled)
	return a.showToast("unlinked · tork's access can also be revoked on real-debrid.com", toastRoute, toastLong)
}

// rdUseRow is "use for downloads" (debrid.enabled). The next download reads
// it, so it applies without a restart.
type rdUseRow struct{}

func (rdUseRow) label() string       { return "use for downloads" }
func (rdUseRow) value(a *App) string { return onOff(a.cfg.Debrid.Enabled) }
func (rdUseRow) hint(*App) string    { return "space toggle" }
func (rdUseRow) keys(*App) []keyHint { return []keyHint{onStrip("space", "toggle")} }
func (rdUseRow) update(a *App, key tea.KeyMsg) (bool, tea.Cmd) {
	if !isToggleKey(key) {
		return false, nil
	}
	on := !a.cfg.Debrid.Enabled
	if err := a.cfg.WriteDebridEnabled(on); err != nil {
		return true, a.showError("not saved: " + err.Error())
	}
	a.cfg.Debrid.Enabled = on
	a.SetDebridStatus(a.settings.rd.phase == rdLinked, on)
	return true, nil
}

// rdDeleteRow is "delete cloud copy after download"
// (debrid.delete_after_download).
type rdDeleteRow struct{}

func (rdDeleteRow) label() string       { return "delete cloud copy after download" }
func (rdDeleteRow) value(a *App) string { return onOff(a.cfg.Debrid.DeleteAfterDownload) }
func (rdDeleteRow) hint(*App) string    { return "space toggle" }
func (rdDeleteRow) keys(*App) []keyHint { return []keyHint{onStrip("space", "toggle")} }
func (rdDeleteRow) update(a *App, key tea.KeyMsg) (bool, tea.Cmd) {
	if !isToggleKey(key) {
		return false, nil
	}
	on := !a.cfg.Debrid.DeleteAfterDownload
	if err := a.cfg.WriteDebridDeleteAfterDownload(on); err != nil {
		return true, a.showError("not saved: " + err.Error())
	}
	a.cfg.Debrid.DeleteAfterDownload = on
	if a.eng != nil {
		a.eng.SetDebridSettings(a.cfg.Debrid)
	}
	return true, nil
}

// --- link dialog ------------------------------------------------------------

type rdLinkPhase int

const (
	rdLinkStarting rdLinkPhase = iota // asking for a code
	rdLinkWaiting                     // code shown, polling
	rdLinkExpired
	rdLinkFailed
)

// rdLinkDialog is the "Link Real-Debrid" box over the dimmed settings. The
// login runs in tea.Cmds under a context that esc, leaving the screen or
// quitting cancels, which stops the polling.
type rdLinkDialog struct {
	gen    int
	cancel context.CancelFunc
	phase  rdLinkPhase
	code   realdebrid.DeviceCode
	err    string
}

type rdCodeMsg struct {
	gen  int
	code realdebrid.DeviceCode
	err  error
	wait tea.Cmd
}

type rdLinkDoneMsg struct {
	gen     int
	account debrid.Account
	acctErr error
	err     error
}

type rdCountdownMsg struct{ gen int }

func (a *App) startRDLink() tea.Cmd {
	a.closeSettingOverlay()
	rd := &a.settings.rd
	rd.gen++
	ctx, cancel := context.WithCancel(context.Background())
	d := &rdLinkDialog{gen: rd.gen, cancel: cancel}
	a.settings.overlay = d
	cfg, ep, gen := a.cfg, rd.endpoints, rd.gen
	login := realdebrid.NewLogin(cfg, ep)
	return func() tea.Msg {
		code, err := login.Start(ctx)
		if err != nil {
			return rdCodeMsg{gen: gen, err: err}
		}
		wait := func() tea.Msg {
			creds, err := login.Wait(ctx)
			if err != nil {
				return rdLinkDoneMsg{gen: gen, err: err}
			}
			acct, acctErr := realdebrid.SaveLogin(ctx, cfg, ep, creds)
			return rdLinkDoneMsg{gen: gen, account: acct, acctErr: acctErr}
		}
		return rdCodeMsg{gen: gen, code: code, wait: wait}
	}
}

func (a *App) rdDialog(gen int) *rdLinkDialog {
	d, ok := a.settings.overlay.(*rdLinkDialog)
	if !ok || d.gen != gen {
		return nil
	}
	return d
}

func (m rdCodeMsg) applySettings(a *App) tea.Cmd {
	d := a.rdDialog(m.gen)
	if d == nil {
		return nil
	}
	if m.err != nil {
		if errors.Is(m.err, context.Canceled) {
			return nil
		}
		d.phase, d.err = rdLinkFailed, debrid.Message(m.err)
		return nil
	}
	d.phase, d.code = rdLinkWaiting, m.code
	return tea.Batch(m.wait, rdCountdown(m.gen))
}

func rdCountdown(gen int) tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return rdCountdownMsg{gen: gen} })
}

// The countdown redraws once a second even with animations off.
func (m rdCountdownMsg) applySettings(a *App) tea.Cmd {
	if d := a.rdDialog(m.gen); d != nil && d.phase == rdLinkWaiting {
		return rdCountdown(m.gen)
	}
	return nil
}

func (m rdLinkDoneMsg) applySettings(a *App) tea.Cmd {
	d := a.rdDialog(m.gen)
	if d == nil {
		return nil
	}
	switch {
	case errors.Is(m.err, context.Canceled):
		return nil
	case errors.Is(m.err, debrid.ErrCodeExpired):
		d.phase = rdLinkExpired
		return nil
	case m.err != nil:
		d.phase, d.err = rdLinkFailed, debrid.Message(m.err)
		return nil
	}
	a.closeSettingOverlay()
	rd := &a.settings.rd
	rd.phase, rd.checking = rdLinked, false
	rd.account, rd.haveAcct, rd.accountErr = m.account, m.acctErr == nil, debrid.Message(m.acctErr)
	a.SetDebridStatus(true, a.cfg.Debrid.Enabled)
	text := "linked"
	if m.acctErr == nil && m.account.Username != "" {
		text = "linked as " + m.account.Username
	}
	return a.showToast(text, toastRoute, toastLong)
}

func (d *rdLinkDialog) crumb() string { return "link Real-Debrid" }
func (d *rdLinkDialog) typing() bool  { return true }
func (d *rdLinkDialog) dialog() bool  { return true }
func (d *rdLinkDialog) close(*App) {
	if d.cancel != nil {
		d.cancel()
	}
}
func (d *rdLinkDialog) animating(*App) bool {
	return d.phase == rdLinkStarting || d.phase == rdLinkWaiting
}

func (d *rdLinkDialog) keys(*App) []keyHint {
	switch d.phase {
	case rdLinkWaiting:
		return []keyHint{onStrip("c", "copy code"), onStrip("o", "open browser"), onStrip("esc", "cancel")}
	case rdLinkExpired, rdLinkFailed:
		return []keyHint{onStrip("enter", "start again"), onStrip("esc", "close")}
	}
	return []keyHint{onStrip("esc", "cancel")}
}

// footer replaces the key strip: the dialog's footer shows only its own keys.
func (d *rdLinkDialog) footer(a *App) string {
	var parts []string
	for _, k := range d.keys(a) {
		parts = append(parts, hint(k.key, k.label))
	}
	return hints(parts...)
}

func (d *rdLinkDialog) update(a *App, key tea.KeyMsg) tea.Cmd {
	switch key.String() {
	case "esc":
		a.closeSettingOverlay()
	case "c":
		if d.phase == rdLinkWaiting {
			return a.yankDownloadValue("code", d.code.UserCode)
		}
	case "o":
		if d.phase == rdLinkWaiting {
			url := d.code.DirectURL
			if url == "" {
				url = d.code.VerificationURL
			}
			return openURLCmd(url)
		}
	case "enter":
		if d.phase == rdLinkExpired || d.phase == rdLinkFailed {
			return a.startRDLink()
		}
	}
	return nil
}

const rdDialogWidth = 48

func (d *rdLinkDialog) view(a *App, width, _ int) string {
	w := min(rdDialogWidth, width)
	frame := a.vframe()
	lines := []string{"", styleRouteBold.Render("Link Real-Debrid"), ""}
	switch d.phase {
	case rdLinkStarting:
		lines = append(lines, spinnerGlyphIn(frame, styleRoute)+" "+styleDim.Render("asking Real-Debrid for a code…"))
	case rdLinkWaiting:
		lines = append(lines,
			styleDim.Render("open  ")+styleFg.Render(shortURL(d.code.VerificationURL)),
			styleDim.Render("code  ")+codePulse(frame).Render(spacedCode(d.code.UserCode)),
			"",
			spinnerGlyphIn(frame, styleRoute)+" "+styleDim.Render("waiting for approval · "+clockText(d.code.Expires.Sub(a.rdNow()))),
		)
	case rdLinkExpired:
		lines = append(lines, styleErr.Render("code expired, start again"))
	case rdLinkFailed:
		lines = append(lines, styleErr.Render(d.err))
	}
	lines = append(lines, "")
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colRoute).
		Padding(0, 2).
		Width(max(10, w-2))
	inner := max(1, w-6)
	for i := range lines {
		lines[i] = truncate(lines[i], inner)
	}
	return box.Render(strings.Join(lines, "\n"))
}

var styleRoutePulse = lipgloss.NewStyle().Bold(true).Foreground(mixColor(colRoute, colWhite, 0.35))

// codePulse breathes the code between lavender and a lighter lavender, 2 s a
// cycle. The still frame is plain bold lavender.
func codePulse(frame int) lipgloss.Style {
	if posMod(frame, 20) >= 10 {
		return styleRoutePulse
	}
	return styleRouteBold
}

// spacedCode letter-spaces a code and splits it in half: "A B C D   1 2 3 4".
func spacedCode(code string) string {
	r := []rune(strings.ReplaceAll(code, " ", ""))
	if len(r) == 0 {
		return ""
	}
	spaced := func(rs []rune) string {
		parts := make([]string, len(rs))
		for i, c := range rs {
			parts[i] = string(c)
		}
		return strings.Join(parts, " ")
	}
	if len(r) >= 6 && len(r)%2 == 0 {
		return spaced(r[:len(r)/2]) + "   " + spaced(r[len(r)/2:])
	}
	return spaced(r)
}

func shortURL(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	return strings.TrimSuffix(u, "/")
}

// clockText is a countdown as m:ss.
func clockText(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Round(time.Second) / time.Second)
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// openURLCmd hands a URL to the platform's browser.
func openURLCmd(url string) tea.Cmd {
	return func() tea.Msg {
		var name string
		var args []string
		switch runtime.GOOS {
		case "darwin":
			name, args = "open", []string{url}
		case "windows":
			name, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
		default:
			name, args = "xdg-open", []string{url}
		}
		cmd := exec.Command(name, args...)
		if err := cmd.Start(); err != nil {
			return revealDownloadMsg{err: fmt.Errorf("open browser failed: %w", err)}
		}
		go func() { _ = cmd.Wait() }()
		return nil
	}
}
