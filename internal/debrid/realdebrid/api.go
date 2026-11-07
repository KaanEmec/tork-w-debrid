package realdebrid

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kaanemec/tork-w-debrid/internal/debrid"
)

// AddMagnet sends a magnet link (POST torrents/addMagnet). Never retried.
func (c *Client) AddMagnet(ctx context.Context, magnet string) (string, error) {
	if !strings.HasPrefix(strings.TrimSpace(magnet), "magnet:") {
		return "", &debrid.Error{Provider: ProviderName, Op: "torrents/addMagnet", Message: "not a magnet link"}
	}
	var out struct {
		ID string `json:"id"`
	}
	err := c.do(ctx, call{method: http.MethodPost, op: "torrents/addMagnet", path: "torrents/addMagnet",
		form: url.Values{"magnet": {strings.TrimSpace(magnet)}}}, &out)
	if err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", &debrid.Error{Provider: ProviderName, Op: "torrents/addMagnet", Message: "no torrent ID in response"}
	}
	return out.ID, nil
}

// AddTorrentFile uploads a .torrent file (PUT torrents/addTorrent). Never
// retried.
func (c *Client) AddTorrentFile(ctx context.Context, data []byte) (string, error) {
	if len(data) == 0 {
		return "", &debrid.Error{Provider: ProviderName, Op: "torrents/addTorrent", Message: "empty torrent file"}
	}
	var out struct {
		ID string `json:"id"`
	}
	err := c.do(ctx, call{method: http.MethodPut, op: "torrents/addTorrent", path: "torrents/addTorrent",
		body: data, contentType: "application/x-bittorrent"}, &out)
	if err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", &debrid.Error{Provider: ProviderName, Op: "torrents/addTorrent", Message: "no torrent ID in response"}
	}
	return out.ID, nil
}

type torrentInfo struct {
	ID            string  `json:"id"`
	Filename      string  `json:"filename"`
	OriginalName  string  `json:"original_filename"`
	Hash          string  `json:"hash"`
	Bytes         int64   `json:"bytes"`
	OriginalBytes int64   `json:"original_bytes"`
	Progress      float64 `json:"progress"`
	Status        string  `json:"status"`
	Added         string  `json:"added"`
	Ended         string  `json:"ended"`
	Speed         int64   `json:"speed"`
	Seeders       int     `json:"seeders"`
	Files         []struct {
		ID       int    `json:"id"`
		Path     string `json:"path"`
		Bytes    int64  `json:"bytes"`
		Selected int    `json:"selected"`
	} `json:"files"`
	Links []string `json:"links"`
}

// Info reads a torrent's state (GET torrents/info/{id}).
func (c *Client) Info(ctx context.Context, id string) (debrid.RemoteInfo, error) {
	if err := checkID("torrents/info", id); err != nil {
		return debrid.RemoteInfo{}, err
	}
	var t torrentInfo
	if err := c.do(ctx, call{method: http.MethodGet, op: "torrents/info", path: "torrents/info/" + id, retry: true}, &t); err != nil {
		return debrid.RemoteInfo{}, err
	}
	return t.remote(), nil
}

func (t torrentInfo) remote() debrid.RemoteInfo {
	status, reason := MapStatus(t.Status)
	name := t.Filename
	if name == "" {
		name = t.OriginalName
	}
	info := debrid.RemoteInfo{
		ID: t.ID, Name: name, Hash: strings.ToLower(t.Hash),
		Status: status, RawStatus: t.Status, Reason: reason,
		Progress: t.Progress, Bytes: t.Bytes, OriginalBytes: t.OriginalBytes,
		SpeedBps: t.Speed, Seeders: t.Seeders, Links: t.Links,
		Added: parseTime(t.Added), Ended: parseTime(t.Ended),
	}
	for _, f := range t.Files {
		info.Files = append(info.Files, debrid.RemoteFile{
			ID: f.ID, Path: strings.TrimLeft(f.Path, "/"), Bytes: f.Bytes, Selected: f.Selected == 1,
		})
	}
	return info
}

// MapStatus maps a Real-Debrid status word onto a debrid.RemoteStatus and, for
// failures, a short reason.
func MapStatus(raw string) (debrid.RemoteStatus, string) {
	switch raw {
	case "magnet_conversion":
		return debrid.Converting, ""
	case "waiting_files_selection":
		return debrid.WaitingSelection, ""
	case "queued":
		return debrid.Queued, ""
	case "downloading", "compressing", "uploading":
		return debrid.Downloading, ""
	case "downloaded":
		return debrid.Ready, ""
	case "error":
		return debrid.Failed, "Real-Debrid reported an error"
	case "magnet_error":
		return debrid.Failed, "magnet could not be read"
	case "virus":
		return debrid.Failed, "virus detected"
	case "dead":
		return debrid.Failed, "dead torrent"
	}
	// An unknown word is most likely a new in-progress state; keep polling.
	return debrid.Queued, ""
}

// SelectFiles chooses files (POST torrents/selectFiles/{id}); an empty list
// selects all. Selecting again is harmless, so it is retried.
func (c *Client) SelectFiles(ctx context.Context, id string, fileIDs []int) error {
	if err := checkID("torrents/selectFiles", id); err != nil {
		return err
	}
	files := "all"
	if len(fileIDs) > 0 {
		parts := make([]string, len(fileIDs))
		for i, f := range fileIDs {
			parts[i] = strconv.Itoa(f)
		}
		files = strings.Join(parts, ",")
	}
	err := c.do(ctx, call{method: http.MethodPost, op: "torrents/selectFiles", path: "torrents/selectFiles/" + id,
		form: url.Values{"files": {files}}, retry: true}, nil)
	var de *debrid.Error
	if errors.As(err, &de) && de.Code == 31 { // action already done
		return nil
	}
	return err
}

type unrestricted struct {
	Filename string `json:"filename"`
	MimeType string `json:"mimeType"`
	Filesize int64  `json:"filesize"`
	Link     string `json:"link"`
	Download string `json:"download"`
}

// Unlock unrestricts a hoster link (POST unrestrict/link). It creates nothing,
// so it is retried like a GET.
func (c *Client) Unlock(ctx context.Context, link string) (debrid.Unlocked, error) {
	link = strings.TrimSpace(link)
	if link == "" {
		return debrid.Unlocked{}, &debrid.Error{Provider: ProviderName, Op: "unrestrict/link", Message: "empty link"}
	}
	var raw jsonOneOrMany[unrestricted]
	if err := c.do(ctx, call{method: http.MethodPost, op: "unrestrict/link", path: "unrestrict/link",
		form: url.Values{"link": {link}}, retry: true}, &raw); err != nil {
		return debrid.Unlocked{}, err
	}
	if len(raw) == 0 || raw[0].Download == "" {
		return debrid.Unlocked{}, &debrid.Error{Provider: ProviderName, Op: "unrestrict/link", Message: "no download link in response"}
	}
	u := raw[0]
	if parsed, err := url.Parse(u.Download); err != nil || parsed.Scheme != "https" {
		return debrid.Unlocked{}, &debrid.Error{Provider: ProviderName, Op: "unrestrict/link", Message: "download link is not https"}
	}
	return debrid.Unlocked{URL: u.Download, Filename: u.Filename, Bytes: u.Filesize, MimeType: u.MimeType, Link: link}, nil
}

// Delete removes a torrent (DELETE torrents/delete/{id}). Deleting is
// idempotent, so it is retried.
func (c *Client) Delete(ctx context.Context, id string) error {
	if err := checkID("torrents/delete", id); err != nil {
		return err
	}
	return c.do(ctx, call{method: http.MethodDelete, op: "torrents/delete", path: "torrents/delete/" + id, retry: true}, nil)
}

// Account reads the user (GET user).
func (c *Client) Account(ctx context.Context) (debrid.Account, error) {
	var u struct {
		Username   string `json:"username"`
		Type       string `json:"type"`
		Premium    int64  `json:"premium"`
		Expiration string `json:"expiration"`
	}
	if err := c.do(ctx, call{method: http.MethodGet, op: "user", path: "user", retry: true}, &u); err != nil {
		return debrid.Account{}, err
	}
	a := debrid.Account{Username: u.Username, Premium: u.Type == "premium" && u.Premium > 0}
	if a.Premium {
		a.Expiry = parseTime(u.Expiration)
	}
	return a, nil
}

func checkID(op, id string) error {
	if id == "" || strings.ContainsAny(id, "/?#%\\ ") || id == "." || id == ".." {
		return &debrid.Error{Provider: ProviderName, Op: op, Message: "invalid torrent ID"}
	}
	return nil
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// jsonOneOrMany decodes either a JSON object or an array of them; unrestrict
// returns an array when one link yields several downloads.
type jsonOneOrMany[T any] []T

func (j *jsonOneOrMany[T]) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "[") {
		var many []T
		if err := json.Unmarshal(data, &many); err != nil {
			return err
		}
		*j = many
		return nil
	}
	var one T
	if err := json.Unmarshal(data, &one); err != nil {
		return err
	}
	*j = []T{one}
	return nil
}
