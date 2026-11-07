package realdebrid

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kaanemec/tork-w-debrid/internal/debrid"
)

// deviceServer approves the login on the approveOn-th credentials poll and
// answers the token call with tokenStatus.
type deviceServer struct {
	approveOn   int32
	tokenStatus int
	codePolls   atomic.Int32
	credPolls   atomic.Int32
	tokenCalls  atomic.Int32
	tokenForm   atomic.Value
}

func (d *deviceServer) handler(t *testing.T) func(http.ResponseWriter, *http.Request, recorded) {
	return func(w http.ResponseWriter, r *http.Request, rec recorded) {
		switch r.URL.Path {
		case "/oauth/v2/device/code":
			d.codePolls.Add(1)
			if r.URL.Query().Get("client_id") != OpenSourceClientID || r.URL.Query().Get("new_credentials") != "yes" {
				t.Errorf("device/code query = %s", r.URL.RawQuery)
			}
			writeJSON(w, http.StatusOK, fixture(t, "device_code.json"))
		case "/oauth/v2/device/credentials":
			n := d.credPolls.Add(1)
			if r.URL.Query().Get("code") != "DEVICECODESECRET" || r.URL.Query().Get("client_id") != OpenSourceClientID {
				t.Errorf("credentials query = %s", r.URL.RawQuery)
			}
			if d.approveOn == 0 || n < d.approveOn {
				writeJSON(w, http.StatusForbidden, []byte(`{"error":"authorization_pending","error_code":9}`))
				return
			}
			writeJSON(w, http.StatusOK, fixture(t, "device_credentials.json"))
		case "/oauth/v2/token":
			d.tokenCalls.Add(1)
			d.tokenForm.Store(rec.Body)
			if d.tokenStatus != 0 && d.tokenStatus != http.StatusOK {
				writeJSON(w, d.tokenStatus, []byte(`{"error":"invalid_grant"}`))
				return
			}
			writeJSON(w, http.StatusOK, fixture(t, "token.json"))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func instantAfter(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Now()
	return ch
}

func newTestLogin(srvURL string) *DeviceLogin {
	d := NewDeviceLogin(NewOAuth(nil, srvURL+"/oauth/v2/"))
	d.after = instantAfter
	return d
}

func TestDeviceLoginApprovesOnThirdPoll(t *testing.T) {
	ds := &deviceServer{approveOn: 3}
	srv, _ := serve(t, ds.handler(t))
	d := newTestLogin(srv.URL)
	code, err := d.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if code.UserCode != "ABCD1234" || code.VerificationURL != "https://real-debrid.com/device" ||
		code.DirectURL != "https://real-debrid.com/device?code=ABCD1234" || code.Interval != 5*time.Second {
		t.Fatalf("code = %+v", code)
	}
	if left := time.Until(code.Expires); left < 9*time.Minute || left > 10*time.Minute {
		t.Fatalf("expires in %v", left)
	}
	creds, err := d.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ds.credPolls.Load() != 3 || ds.tokenCalls.Load() != 1 {
		t.Fatalf("credential polls = %d, token calls = %d", ds.credPolls.Load(), ds.tokenCalls.Load())
	}
	want := debrid.Credentials{ClientID: "USERCLIENTID", ClientSecret: "USERCLIENTSECRET", AccessToken: "ACCESSTOKEN1", RefreshToken: "REFRESHTOKEN1"}
	if creds.ClientID != want.ClientID || creds.ClientSecret != want.ClientSecret || creds.AccessToken != want.AccessToken || creds.RefreshToken != want.RefreshToken {
		t.Fatalf("creds = %+v", creds)
	}
	if left := time.Until(creds.Expiry); left < 59*time.Minute || left > time.Hour {
		t.Fatalf("token expires in %v", left)
	}
	form, _ := url.ParseQuery(ds.tokenForm.Load().(string))
	if form.Get("client_id") != "USERCLIENTID" || form.Get("client_secret") != "USERCLIENTSECRET" ||
		form.Get("code") != "DEVICECODESECRET" || form.Get("grant_type") != "http://oauth.net/grant_type/device/1.0" {
		t.Fatalf("token form = %v", form)
	}
}

func TestDeviceLoginExpires(t *testing.T) {
	ds := &deviceServer{}
	srv, _ := serve(t, ds.handler(t))
	d := newTestLogin(srv.URL)
	clock := time.Now()
	d.oauth.now = func() time.Time { return clock }
	if _, err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.after = func(time.Duration) <-chan time.Time {
		clock = clock.Add(5 * time.Minute) // two polls, then past the 10-minute expiry
		return instantAfter(0)
	}
	_, err := d.Wait(context.Background())
	if !errors.Is(err, debrid.ErrCodeExpired) {
		t.Fatalf("err = %v", err)
	}
	if debrid.Message(err) != "Real-Debrid: code expired, start again" {
		t.Fatalf("message = %q", debrid.Message(err))
	}
	if ds.tokenCalls.Load() != 0 || ds.credPolls.Load() != 1 {
		t.Fatalf("polls = %d token = %d", ds.credPolls.Load(), ds.tokenCalls.Load())
	}
}

func TestDeviceLoginCancelStopsAtOnce(t *testing.T) {
	ds := &deviceServer{}
	srv, _ := serve(t, ds.handler(t))
	d := NewDeviceLogin(NewOAuth(nil, srv.URL+"/oauth/v2/")) // real 5 s interval
	if _, err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := d.Wait(ctx)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if time.Since(start) > time.Second {
			t.Fatal("cancel took over a second")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after cancel")
	}
	if ds.credPolls.Load() != 0 {
		t.Fatalf("polled %d times after cancel", ds.credPolls.Load())
	}
}

func TestDeviceLoginTokenExchangeRejected(t *testing.T) {
	ds := &deviceServer{approveOn: 1, tokenStatus: http.StatusBadRequest}
	srv, _ := serve(t, ds.handler(t))
	d := newTestLogin(srv.URL)
	if _, err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := d.Wait(context.Background())
	var de *debrid.Error
	if !errors.As(err, &de) || de.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "USERCLIENTSECRET") || strings.Contains(err.Error(), "DEVICECODESECRET") {
		t.Fatalf("error leaks a secret: %v", err)
	}
}

func TestDeviceLoginRefusedClientIsNotPremiumError(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"permission_denied","error_code":9}`))
	})
	_, err := newTestLogin(srv.URL).Start(context.Background())
	if errors.Is(err, debrid.ErrNotPremium) {
		t.Fatalf("a refused OAuth client reads as premium expired: %v", err)
	}
	if got, want := debrid.Message(err), "Real-Debrid: permission denied"; got != want {
		t.Fatalf("Message = %q, want %q", got, want)
	}
}

func TestDeviceLoginSlowDownBacksOff(t *testing.T) {
	var polls atomic.Int32
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		switch r.URL.Path {
		case "/oauth/v2/device/code":
			writeJSON(w, http.StatusOK, fixture(t, "device_code.json"))
		case "/oauth/v2/device/credentials":
			if polls.Add(1) == 1 {
				writeJSON(w, http.StatusTooManyRequests, []byte(`{"error":"slow_down","error_code":5}`))
				return
			}
			writeJSON(w, http.StatusOK, fixture(t, "device_credentials.json"))
		case "/oauth/v2/token":
			writeJSON(w, http.StatusOK, fixture(t, "token.json"))
		}
	})
	d := newTestLogin(srv.URL)
	var waits []time.Duration
	d.after = func(dur time.Duration) <-chan time.Time {
		waits = append(waits, dur)
		return instantAfter(0)
	}
	if _, err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 2 || waits[0] != 5*time.Second || waits[1] != 10*time.Second {
		t.Fatalf("waits = %v", waits)
	}
}
