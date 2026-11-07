package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"

	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/debrid"
	"github.com/kaanemec/tork-w-debrid/internal/debrid/realdebrid"
)

const debridUsage = "usage: torkwd debrid login|status|logout [-y]|on|off"

// debridCLI runs `torkwd debrid …`. Its fields are injectable so tests can use
// a temporary ~/.tork and a fake Real-Debrid server.
type debridCLI struct {
	out, errOut io.Writer
	in          io.Reader
	ep          realdebrid.Endpoints
	opts        []realdebrid.Option
	interactive bool
	openURL     func(string) error
	now         func() time.Time
	loadConfig  func() (*config.Config, error)
}

func newDebridCLI() *debridCLI {
	return &debridCLI{
		out: os.Stdout, errOut: os.Stderr, in: os.Stdin,
		interactive: term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stderr.Fd()),
		openURL:     openInBrowser,
		now:         time.Now,
		loadConfig:  config.LoadReadOnly,
	}
}

// runDebrid returns the process exit code; err is printed by main.
func runDebrid(args []string) (int, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return newDebridCLI().run(ctx, args)
}

func (c *debridCLI) run(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return 1, errors.New(debridUsage)
	}
	switch args[0] {
	case "login":
		if len(args) != 1 {
			return 1, errors.New("usage: torkwd debrid login")
		}
		return c.login(ctx)
	case "status":
		if len(args) != 1 {
			return 1, errors.New("usage: torkwd debrid status")
		}
		return c.status(ctx)
	case "logout":
		return c.logout(args[1:])
	case "on", "off":
		if len(args) != 1 {
			return 1, fmt.Errorf("usage: torkwd debrid %s", args[0])
		}
		return c.setEnabled(args[0] == "on")
	default:
		return 1, errors.New(debridUsage)
	}
}

func (c *debridCLI) login(ctx context.Context) (int, error) {
	cfg, err := c.loadConfig()
	if err != nil {
		return 1, err
	}
	if err := cfg.ProxyError(); err != nil {
		return 1, fmt.Errorf("proxy config is invalid: %v", err)
	}
	login := realdebrid.NewLogin(cfg, c.ep)
	code, err := login.Start(ctx)
	if err != nil {
		return 1, errors.New(debrid.Message(err))
	}
	fmt.Fprintf(c.out, "Open %s and enter code %s (expires in %s)\n", code.VerificationURL, code.UserCode, clock(time.Until(code.Expires)))
	if code.DirectURL != "" && c.openURL != nil {
		_ = c.openURL(code.DirectURL) // a missing browser is fine; the code is printed
	}
	stopSpinner := c.spinner(code.Expires)
	creds, err := login.Wait(ctx)
	stopSpinner()
	if err != nil {
		if ctx.Err() != nil {
			return 1, errors.New("login cancelled; nothing was saved")
		}
		return 1, errors.New(debrid.Message(err))
	}
	account, err := realdebrid.SaveLogin(ctx, cfg, c.ep, creds, c.opts...)
	if err != nil {
		if !isProviderError(err) && ctx.Err() == nil {
			return 1, fmt.Errorf("save the Real-Debrid link: %w", err)
		}
		fmt.Fprintf(c.out, "Linked, but the account check failed: %s\n", debrid.Message(err))
		return 0, nil
	}
	fmt.Fprintf(c.out, "Linked as %s · %s\n", account.Username, premiumText(account, c.now()))
	if !cfg.Debrid.Enabled {
		fmt.Fprintln(c.out, "downloads still go to the swarm; turn Real-Debrid on with: torkwd debrid on")
	}
	return 0, nil
}

func isProviderError(err error) bool {
	var de *debrid.Error
	return errors.As(err, &de)
}

// spinner shows a waiting line with a countdown on an interactive terminal.
func (c *debridCLI) spinner(expires time.Time) (stop func()) {
	if !c.interactive {
		fmt.Fprintln(c.errOut, "waiting for approval…")
		return func() {}
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		frames := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			fmt.Fprintf(c.errOut, "\r%c waiting for approval · %s ", frames[i%len(frames)], clock(time.Until(expires)))
			select {
			case <-done:
				fmt.Fprint(c.errOut, "\r\033[K")
				return
			case <-tick.C:
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

func (c *debridCLI) status(ctx context.Context) (int, error) {
	cfg, err := c.loadConfig()
	if err != nil {
		return 1, err
	}
	store := debrid.NewStore(cfg)
	link, err := store.Load()
	switch {
	case errors.Is(err, debrid.ErrNotLinked):
		fmt.Fprintln(c.out, "not linked · run: torkwd debrid login")
		return 1, nil
	case err != nil:
		return 1, err
	case link.NeedsLogin:
		fmt.Fprintln(c.out, "Real-Debrid: needs login (access was revoked) · run: torkwd debrid login")
		return 1, nil
	}
	enabled := "off · turn on with: torkwd debrid on"
	if cfg.Debrid.Enabled {
		enabled = "on"
	}
	client, tokens := realdebrid.NewFromConfig(cfg, c.ep, c.opts...)
	account, accErr := client.Account(ctx)
	if accErr != nil {
		fmt.Fprintf(c.out, "Real-Debrid: linked · account check failed: %s\n", debrid.Message(accErr))
	} else {
		fmt.Fprintf(c.out, "Real-Debrid: linked as %s · %s\n", account.Username, premiumText(account, c.now()))
	}
	fmt.Fprintf(c.out, "use for downloads: %s\n", enabled)
	switch {
	case errors.Is(accErr, debrid.ErrNotLinked):
		fmt.Fprintln(c.out, "token: refresh rejected · run: torkwd debrid login")
	case tokens.Link().RefreshToken != "" || link.RefreshToken != "":
		fmt.Fprintln(c.out, "token: refreshes automatically")
	}
	if accErr != nil || !account.Premium {
		return 1, nil
	}
	return 0, nil
}

func (c *debridCLI) logout(args []string) (int, error) {
	fs := flag.NewFlagSet("debrid logout", flag.ContinueOnError)
	fs.SetOutput(c.errOut)
	yes := fs.Bool("y", false, "do not ask for confirmation")
	if err := fs.Parse(args); err != nil {
		return 1, err
	}
	if fs.NArg() != 0 {
		return 1, errors.New("usage: torkwd debrid logout [-y]")
	}
	cfg, err := c.loadConfig()
	if err != nil {
		return 1, err
	}
	store := debrid.NewStore(cfg)
	if !store.Exists() {
		fmt.Fprintln(c.out, "not linked")
		return 0, nil
	}
	if !*yes {
		fmt.Fprint(c.out, "Remove the Real-Debrid link from this machine? [y/N] ")
		line, _ := bufio.NewReader(c.in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(c.out, "kept")
			return 1, nil
		}
	}
	if err := store.Remove(); err != nil {
		return 1, err
	}
	fmt.Fprintln(c.out, "unlinked · tork's access can also be revoked on real-debrid.com")
	return 0, nil
}

func (c *debridCLI) setEnabled(on bool) (int, error) {
	cfg, err := c.loadConfig()
	if err != nil {
		return 1, err
	}
	word := map[bool]string{true: "on", false: "off"}[on]
	if cfg.Debrid.Enabled == on {
		fmt.Fprintf(c.out, "Real-Debrid downloads are already %s\n", word)
		return 0, nil
	}
	if err := cfg.WriteDebridEnabled(on); err != nil {
		return 1, err
	}
	fmt.Fprintf(c.out, "Real-Debrid downloads %s; restart torkwd to apply it (run torkwd stop first if a background session is running)\n", word)
	if on && !debrid.NewStore(cfg).Exists() {
		fmt.Fprintln(c.out, "no account is linked yet · run: torkwd debrid login")
	}
	return 0, nil
}

func premiumText(a debrid.Account, now time.Time) string {
	if !a.Premium {
		return "not premium"
	}
	if a.Expiry.IsZero() {
		return "premium"
	}
	return fmt.Sprintf("premium until %s (%d days left)", a.Expiry.Format("2006-01-02"), a.DaysLeft(now))
}

// clock formats a duration as m:ss.
func clock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Round(time.Second) / time.Second)
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// openInBrowser hands a URL to the platform's default handler, as the
// downloads screen's "open" does for files.
func openInBrowser(url string) error {
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
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
