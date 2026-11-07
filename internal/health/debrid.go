package health

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/debrid"
	"github.com/kaanemec/tork-w-debrid/internal/debrid/realdebrid"
)

// lowPremiumDays is the premium time left below which doctor warns.
const lowPremiumDays = 7

// DebridProbe performs the live account check behind --debrid-check. It
// reports whether it had to refresh (and so rewrite) an expired token.
type DebridProbe func(context.Context, *config.Config) (account debrid.Account, refreshed bool, err error)

// NewDebridProbe returns the production probe: GET /user over cfg's proxy
// route. It refreshes the token only when it has already expired, and never
// on a 401, so doctor rewrites debrid.json only when it had to. ep is empty
// in production; tests point it at a fake server.
func NewDebridProbe(ep realdebrid.Endpoints, opts ...realdebrid.Option) DebridProbe {
	return func(ctx context.Context, cfg *config.Config) (debrid.Account, bool, error) {
		h := realdebrid.HTTPClient(cfg)
		ts := realdebrid.NewStoreTokenSource(debrid.NewStore(cfg), realdebrid.NewOAuth(h, ep.OAuth))
		ts.Skew = 0
		all := []realdebrid.Option{realdebrid.WithHTTPClient(h)}
		if ep.API != "" {
			all = append(all, realdebrid.WithBaseURL(ep.API))
		}
		c := realdebrid.New(realdebrid.TokenOnly(ts), append(all, opts...)...)
		a, err := c.Account(ctx)
		return a, ts.Refreshes() > 0, err
	}
}

// checkDebrid reports the local debrid link and, with a probe, the account.
// Without a probe it makes no network request.
func checkDebrid(ctx context.Context, cfg *config.Config, probe DebridProbe, now time.Time) []Check {
	enabled := Check{Name: "debrid downloads", Status: StatusOK, Detail: "off · torrents use the swarm (torkwd debrid on)"}
	if cfg.Debrid.Enabled {
		enabled.Detail = "on"
	}
	link := Check{Name: "debrid"}
	store := debrid.NewStore(cfg)
	if !store.Exists() {
		link.Status = StatusOK
		link.Detail = "not linked (run 'torkwd debrid login' to use Real-Debrid)"
		return []Check{link}
	}
	secure, err := store.Secure()
	if err != nil {
		link.Status = StatusFail
		link.Detail = fmt.Sprintf("cannot inspect %s: %v", store.Path, err)
		return []Check{link, enabled}
	}
	if !secure {
		link.Status = StatusFail
		link.Detail = debrid.ErrInsecureFile.Error()
		return []Check{link, enabled}
	}
	l, err := store.Load()
	if err != nil {
		link.Status = StatusFail
		link.Detail = err.Error()
		return []Check{link, enabled}
	}
	switch {
	case l.NeedsLogin:
		link.Status = StatusWarn
		if cfg.Debrid.Enabled {
			link.Status = StatusFail
		}
		link.Detail = "access was revoked · run 'torkwd debrid login'"
		return []Check{link, enabled}
	case l.RefreshToken == "":
		link.Status = StatusFail
		link.Detail = "no refresh token · run 'torkwd debrid login'"
		return []Check{link, enabled}
	}
	link.Status = StatusOK
	link.Detail = "Real-Debrid linked · token refreshes automatically · debrid.json private"
	checks := []Check{link, enabled}
	if probe != nil {
		checks = append(checks, debridAccountCheck(ctx, cfg, probe, now))
	}
	return checks
}

func debridAccountCheck(ctx context.Context, cfg *config.Config, probe DebridProbe, now time.Time) Check {
	c := Check{Name: "debrid account"}
	a, refreshed, err := probe(ctx, cfg)
	note := ""
	if refreshed {
		note = " · token had expired; refreshed and saved debrid.json"
	}
	switch {
	case errors.Is(err, debrid.ErrNotLinked), errors.Is(err, debrid.ErrUnauthorized):
		c.Status = StatusFail
		c.Detail = "token rejected · run 'torkwd debrid login'" + note
		return c
	case errors.Is(err, debrid.ErrUnavailable), errors.Is(err, debrid.ErrRateLimited):
		c.Status = StatusWarn
		c.Detail = debrid.Message(err) + note
		return c
	case err != nil:
		c.Status = StatusFail
		c.Detail = debrid.Message(err) + note
		return c
	}
	days := a.DaysLeft(now)
	switch {
	case !a.Premium || (!a.Expiry.IsZero() && !a.Expiry.After(now)):
		c.Status = StatusFail
		c.Detail = fmt.Sprintf("%s · premium expired", a.Username)
	case !a.Expiry.IsZero() && days < lowPremiumDays:
		c.Status = StatusWarn
		c.Detail = fmt.Sprintf("%s · premium ends in %d days (%s)", a.Username, days, a.Expiry.Format("2006-01-02"))
	default:
		c.Status = StatusOK
		c.Detail = fmt.Sprintf("%s · premium, %d days left", a.Username, days)
	}
	c.Detail += note
	return c
}
