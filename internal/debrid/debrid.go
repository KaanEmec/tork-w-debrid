// Package debrid is tork's provider-neutral view of a debrid service: a cloud
// that downloads a torrent for you and hands back plain HTTPS links. The
// engine and the TUI talk only to Provider, so another service can be added
// without touching them. Real-Debrid lives in the realdebrid subpackage.
package debrid

import (
	"context"
	"time"
)

// Provider is the set of calls the engine and the TUI need from a debrid
// service. Implementations are safe for concurrent use; every method honours
// ctx cancellation. Errors are *Error values (or wrap one) that match the
// sentinels in errors.go with errors.Is.
type Provider interface {
	// Name is the service's display name, such as "Real-Debrid".
	Name() string
	// AddMagnet adds a magnet link and returns the remote torrent ID. It is
	// never retried automatically, since a retry could add a duplicate.
	AddMagnet(ctx context.Context, magnet string) (string, error)
	// AddTorrentFile adds a .torrent file's bytes and returns the remote ID.
	AddTorrentFile(ctx context.Context, data []byte) (string, error)
	// Info returns the remote torrent's status, progress, files and links.
	Info(ctx context.Context, id string) (RemoteInfo, error)
	// SelectFiles chooses which files the service downloads. File IDs are
	// RemoteFile.ID values; an empty list selects every file.
	SelectFiles(ctx context.Context, id string, fileIDs []int) error
	// Unlock turns a RemoteInfo link into a direct HTTPS download.
	Unlock(ctx context.Context, link string) (Unlocked, error)
	// Delete removes the remote torrent and its cloud copy.
	Delete(ctx context.Context, id string) error
	// Account returns the linked account's user name and premium state.
	Account(ctx context.Context) (Account, error)
}

// RemoteStatus is a provider-neutral torrent state. The string values are
// stable, so callers may persist them.
type RemoteStatus string

const (
	// Converting: the service is still fetching the magnet's metadata.
	Converting RemoteStatus = "converting"
	// WaitingSelection: the file list is known and the service waits for
	// SelectFiles.
	WaitingSelection RemoteStatus = "waiting_selection"
	// Queued: files are selected and the service has not started yet.
	Queued RemoteStatus = "queued"
	// Downloading: the service is downloading (or packing/uploading).
	Downloading RemoteStatus = "downloading"
	// Ready: every selected file is on the service and Links are valid.
	Ready RemoteStatus = "ready"
	// Failed: the service gave up; RemoteInfo.Reason says why.
	Failed RemoteStatus = "failed"
)

// Terminal reports whether the status will not change by itself.
func (s RemoteStatus) Terminal() bool { return s == Ready || s == Failed }

// RemoteFile is one file inside a remote torrent.
type RemoteFile struct {
	// ID is the provider's file ID, used by SelectFiles.
	ID int
	// Path is the file's path inside the torrent, with forward slashes and
	// no leading slash. It is untrusted: sanitise it before touching disk.
	Path     string
	Bytes    int64
	Selected bool
}

// RemoteInfo is a remote torrent's state at one moment.
type RemoteInfo struct {
	ID string
	// Name is the torrent's name as the service reports it.
	Name string
	// Hash is the lower-case hex infohash.
	Hash   string
	Status RemoteStatus
	// RawStatus is the provider's own status word (e.g. "magnet_error").
	RawStatus string
	// Reason explains a Failed status in a few words ("dead torrent").
	Reason string
	// Progress is 0–100 for the cloud download.
	Progress float64
	// Packing: the download is done and the service is packing the files
	// into an archive or moving them to its hosts (Status is Downloading).
	Packing bool
	// Bytes is the size of the selected files; OriginalBytes the whole torrent.
	Bytes         int64
	OriginalBytes int64
	SpeedBps      int64
	Seeders       int
	Files         []RemoteFile
	// Links are the hoster links of the selected files, in the order of
	// SelectedFiles once Status is Ready. Pass each to Unlock.
	Links []string
	Added time.Time
	Ended time.Time
}

// SelectedFiles returns the selected files in ID order, which is the order
// the provider uses for Links.
func (r RemoteInfo) SelectedFiles() []RemoteFile {
	var out []RemoteFile
	for _, f := range r.Files {
		if f.Selected {
			out = append(out, f)
		}
	}
	return out
}

// Unlocked is a direct download produced by Provider.Unlock. URL is tied to
// the IP that unlocked it: download it over the same route. Never log URL;
// use RedactURL.
type Unlocked struct {
	URL      string
	Filename string
	Bytes    int64
	MimeType string
	// Link is the hoster link that was unlocked.
	Link string
}

// Account describes the linked account.
type Account struct {
	Username string
	Premium  bool
	// Expiry is when premium ends; zero when unknown or not premium.
	Expiry time.Time
}

// DaysLeft is the whole number of premium days left at now; 0 when expired.
func (a Account) DaysLeft(now time.Time) int {
	if !a.Premium || a.Expiry.IsZero() || !a.Expiry.After(now) {
		return 0
	}
	return int(a.Expiry.Sub(now) / (24 * time.Hour))
}
