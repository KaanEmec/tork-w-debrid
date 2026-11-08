# Tork-w-Debrid

> A fork of [melqtx/tork](https://github.com/melqtx/tork). It tracks upstream
> and adds Real-Debrid downloads with fast multi-connection transfers, a new
> look, `torkwd update` (self-update from source) and more built-in sources
> ([melqtx/tork#35](https://github.com/melqtx/tork/pull/35)). Its Go module is `github.com/kaanemec/tork-w-debrid`, so
> `go install` and `torkwd update` use this fork. The command is `torkwd`, so
> it can sit beside upstream `tork`; both share the `~/.tork` config. The
> Homebrew, AUR and Nix packages install upstream tork, not this fork.

```
 /\_/\
( ^.^ )  a cozy terminal for torrent search + verified downloads
 > ^ <
```

You name it, the cat fetches it. Search public torrent indexes, compare the
actual swarm, and keep downloads in one calm little terminal app.

![tork home screen](docs/home.png)

## What it does

- Search Knaben, YTS, Nyaa, TheRARBG, LimeTorrents, and all Pirate Bay categories,
  plus your own RSS or Torznab feeds.
- Fold the same torrent listed on several indexes into a single row, and grab it
  with every tracker those indexes knew about between them.
- Group duplicate releases, rank the useful ones, and surface seeders, size,
  source, and noisy results before you download.
- Preview a magnet, queue it with one key, then pause, verify, seed, move, or
  relink it from the downloads screen.
- Browse and grab current official Linux ISOs from Ubuntu, Debian, Fedora,
  Arch, NixOS, Proxmox, and more. No sketchy mirror hunting.
- Link a Real-Debrid account and download through it: cached torrents start at
  full speed, and each file uses several connections. See
  [Real-Debrid](#real-debrid).
- Run `torkwd doctor` when a provider or local setup feels off, and use the swarm
  compass to see whether your sources and downloads are healthy over time.

![tork search results](docs/results.png)

## Real-Debrid

Link a [Real-Debrid](https://real-debrid.com) account (premium needed) and
Tork-w-Debrid downloads torrents through it instead of the swarm. Linking uses
Real-Debrid's device-code login only; no API token is ever typed in or stored
by hand.

```sh
torkwd debrid login        # shows a code and opens real-debrid.com/device
torkwd debrid status       # account, premium days left, and whether it is on
torkwd debrid logout [-y]  # forget the link on this machine
torkwd debrid on | off     # use Real-Debrid for downloads, or not (stays linked)
```

You can also link from Settings (`F5`, section Real-Debrid): `enter` on
"account" shows the code. Tork-w-Debrid uses Real-Debrid's open-source client
ID (`X245A4XAIBGVM`) and keeps the tokens in `~/.tork/debrid.json`, readable by
you only. `torkwd debrid logout` removes them here; you can also revoke access
on real-debrid.com. `torkwd doctor --debrid-check` makes one live request to
verify the account.

Once an account is linked and "use for downloads" is on (the default), every
download you start from the preview goes through Real-Debrid. That is every
download except autopilot and the Linux ISO shelf, which never use it.

- **Cached on Real-Debrid:** within about 5 seconds the row switches from
  "starting" to a normal download, written straight into your download folder
  from Real-Debrid's servers over several connections.
- **Not cached:** after 5 seconds the row shows Real-Debrid's own progress with
  a `☁` marker, and the Downloads header shows a `☁ N` chip. When Real-Debrid
  is done, the local download starts by itself.
- **Torkwd must be running** for that last step. On macOS and Linux the
  background session keeps going after you close the UI. Windows has no
  background session: cloud polling stops when you quit and picks up again the
  next time you start `torkwd`.
- Sometimes Real-Debrid packs several files into one RAR archive. The row says
  so, and the archive is downloaded as it is.
- If Real-Debrid refuses a torrent, the row says why. `t` on that row downloads
  it as a normal torrent instead, and `p` retries a row that is held.
- On the Downloads screen, `x` asks before removing a row. In the dialog `c`
  toggles "cloud copy: delete / keep". The default is delete while the torrent
  is still in the cloud phase and keep once the local download has started.
  Settings has "delete cloud copy after download" to clear the copy
  automatically (`debrid.delete_after_download`, off by default).
- Debrid downloads don't seed: the seed key is not offered on them.
- Select one multi-connection row to see its segment map and the speed of each
  connection.

### What Real-Debrid sees

- Real-Debrid sees every torrent you send it and the files you download from it.
  It is a third party with its own terms; this fork does not change that.
- With Real-Debrid on, the preview never contacts the swarm, so your IP is not
  shown to peers. The file list comes from the local metadata cache, the
  `.torrent` you gave, or Real-Debrid. Turn it off (`torkwd debrid off`) to
  get normal torrent downloads back.
- Real-Debrid ties download links to one IP. If you use the SOCKS5 proxy below,
  all Real-Debrid traffic goes through it; don't switch the proxy while a
  debrid download is running, or its links stop working.

### Connections

Each Real-Debrid file downloads with several connections:

```yaml
debrid:
  enabled: true                 # "use for downloads"
  delete_after_download: false
  connections: 4                # per file, 1-16
  max_total_connections: 8      # per torrent, 1-32, shared between its files
```

Settings (`F5`, Downloads) steps `connections per download` through 1, 2, 4, 8
and 16 and saves at once. Every segment is written straight into its place in
the final file, so there is no merge step at the end. A slow connection is
replaced and idle ones take over the biggest remaining piece. Those ideas come
from [Surge](https://github.com/surge-downloader/surge) (MIT); the code is this
project's own `internal/engine/direct_parallel.go`.

## Settings

Press `F5` anywhere. Sections: Real-Debrid (account, use for downloads, delete
cloud copy after download), Downloads (default folder, connections) and Look.
Everything is saved to `~/.tork/config.yaml` as you change it.

The **default folder** is saved as `download_dir` in that file, which belongs to
this machine. `torkwd -d DIR` overrides it for one run only, and Settings then
says "this run uses -d …". Pressing `r` on the folder row resets it to your OS
Downloads folder.

## The look

The interface uses a dark "Nightglass" look: few panels, one accent colour,
and a lavender that is only used for Real-Debrid. Small animations run at 10
frames per second, and only while something on screen moves.

- After each update, a short C64/Amiga-style intro plays once. `torkwd intro`
  plays it again at any time, and any key stops it. Settings › Look has
  `animations`, `intro after each update` and `play the intro`.
- `--no-motion`, `NO_COLOR`, `TERM=dumb`, or output that isn't a terminal turns
  every animation off.
- ASCII mode switches on by itself when the terminal can't draw the glyphs
  (`TERM=dumb` or `linux`, a locale that isn't UTF-8, or no colour at all).
  `TORKWD_ASCII=1` forces it and `TORKWD_ASCII=0` forces it off.
- Terminals down to 50 columns by 20 rows are supported.

![Tork-w-Debrid home](docs/home.png)

## Install

```sh
go install github.com/kaanemec/tork-w-debrid/cmd/torkwd@latest    # Go 1.26.10+
```

This installs a binary called `torkwd`. If Go on macOS is picking Nix's compiler,
use Apple Clang for the install:

```sh
CC=/usr/bin/clang CXX=/usr/bin/clang++ go install github.com/kaanemec/tork-w-debrid/cmd/torkwd@latest
```

Upstream's packages (`brew tap melqtx/tap && brew install tork`, `yay -S tork`,
`nix run github:melqtx/tork`) install upstream tork instead of this fork.

## Update

Installs made with `go install` or from source can update themselves:

```sh
torkwd update           # or: torkwd -update / torkwd --update
torkwd update --check   # only report whether a newer release exists
torkwd update --force   # rebuild the latest release even if already current
```

`torkwd update` asks the Go module system for the newest tagged release of
`github.com/kaanemec/tork-w-debrid` (this fork, never upstream; while the fork
has no tags, that is the newest commit on `main`), builds it from source with `go install` (so the source is verified against
sum.golang.org), and swaps the new binary in for the running one. Every
download, cache, and intermediate file lives in one temporary directory that
is removed afterwards. It needs Go 1.26.10+ on `PATH`. Homebrew, AUR, and Nix
installs are left to their package manager. On Windows the previous binary is
renamed to `torkwd.exe.old` while it is still running and deleted the next time
torkwd starts.

## Build from source

Building tork requires Go 1.26.10 or newer. From the repository root, run it
directly without creating a binary:

```sh
go run ./cmd/torkwd
```

Or build and run a local binary:

```sh
go build -o ./bin/torkwd ./cmd/torkwd
./bin/torkwd
```

Config lives in `~/.tork/` (shared with upstream tork); downloads land in
`~/Downloads/tork` (change it in Settings, or per run with `torkwd -d DIR`).

Already have a torrent? Paste its magnet link, bare infohash, local `.torrent`
path, or `.torrent` URL into the same home search box. To go straight to the
quiet file preview:

```sh
torkwd 'magnet:?xt=urn:btih:…'
torkwd ~/Downloads/linux.iso.torrent
torkwd 'https://example.org/linux.iso.torrent'
torkwd --torrent-url 'https://example.org/download?id=123'
```

Nothing starts downloading until you confirm it. `enter` takes whatever is
selected, from wherever the cursor happens to be. Tracker adverts, NFO and
checksum files, and sample clips start deselected in a collapsed Extras group — the header says how many
were grouped, and `a` puts them back. Expand the group with `→` to inspect them,
or select the entire group with Space. The preview shows selected size, the save
folder, and the Enter-to-download action together above the file list. If a bare hash is still finding metadata
peers, wait to choose individual files or press enter to queue the whole
torrent immediately. Once metadata arrives, tork keeps a private
bounded copy under `~/.tork/metainfo/`, so reopening or resuming that magnet no
longer depends on finding a metadata peer.

Cache defaults are conservative and can be changed in `~/.tork/config.yaml`:

```yaml
metadata_cache:
  enabled: true
  max_mb: 256
  max_entries: 512
```

Advanced torrent-client limits are optional; zero or omitted values retain the
anacrolix defaults:

```yaml
torrent_tuning:
  half_open_conns_per_torrent: 25
  total_half_open_conns: 100
  piece_hashers_per_torrent: 2
  max_unverified_bytes: 67108864
  dial_rate_limit: 10
  peer_high_water: 500
  peer_low_water: 50
  download_rate_limit: 0
  upload_rate_limit: 0
  disable_aggressive_upload: false
  no_upload: false
```

Transfer rates are client-wide bytes per second; dial rate is dials per second.
`no_upload` prevents all torrent uploads, including seeding. The torrent library
does not expose its webseed concurrency as a runtime client setting, so tork
does not claim to configure it here.

Direct HTTP downloads (the Linux ISO shelf) use verified byte ranges when the server supports them:

```yaml
direct:
  max_connections: 4
  min_chunk_size: "10MB"
  enable_chunking: true
```

The connection limit is per direct download. tork confirms range support and a
stable resource before splitting; small, unknown-length, or incompatible
responses automatically use one sequential connection. Paused segmented
transfers resume from an internal `.part.meta` sidecar, and the final filename
still appears only after its published SHA-1, SHA-256, or SHA-512 digest is
verified.

## SOCKS5 proxy

For the usual local Tor setup, one command is enough:

```sh
torkwd proxy tor
torkwd doctor --proxy-check
```

`torkwd proxy status` shows the redacted endpoint and strict-mode limits without
making a network request. To use another unauthenticated SOCKS5 proxy:

```sh
torkwd proxy set socks5://127.0.0.1:1080
```

For an authenticated endpoint, run `torkwd proxy set` without an argument and
enter the URL through hidden terminal input, so it never appears in shell
history or a process list. You can also configure tork by hand:

```yaml
proxy:
  socks5: "socks5://user:password@127.0.0.1:9050"
```

`socks5://` and `socks5h://` both resolve destination names at the proxy.
Username and password are optional and must be URL-encoded. For Tor, the usual
local endpoint is `socks5://127.0.0.1:9050`.

Credentials require `~/.tork/config.yaml` to be a regular file with mode
`0600`. A normal tork launch tightens an insecure regular config once; `torkwd
doctor` stays read-only and reports the problem instead.

Proxy mode is strict: searches, ISO requests, HTTP trackers, and outgoing TCP
peers use the proxy. tork disables DHT, uTP, UDP trackers, inbound peers, port
forwarding, and WebTorrent rather than risk a direct connection. Downloads may
find fewer peers as a result. The TUI keeps a visible `SOCKS strict` badge; it
becomes `((o)) Tor strict` only after a live verification confirms a Tor exit. A
check-service outage is shown as unavailable, not as a leak, and tork never
falls back to a direct connection.

## Keep an eye on it

`torkwd doctor` is read-only by default and checks config, disk, state, cached
metadata, and provider reachability. Add `--engine` for an opt-in listener
check, `--record` to save provider results in `~/.tork/health.json`, or
`--proxy-check` to ask the Tor Project's check service what egress IP it sees
through your configured proxy. That proves this explicit HTTP check used the
route; it is not a promise of anonymity or a claim about your normal
connection. Health history contains local provider timings plus torrent names
and swarm counts; it is never uploaded.

Automatic checks are off by default. Enable a local daily check with:

```yaml
health:
  enabled: true
  interval_hours: 24
```

The check sends the generic `1080p` canary query to enabled providers. Press
`H` in tork to view saved source and swarm history, or `r` there to record a
manual check.

## Search sources

Open **sources** from the home menu to see every built-in index, including
TheRARBG, LimeTorrents, The Pirate Bay (all categories), EZTV, 1337x,
[Torrents.csv](https://codeberg.org/heretic/torrents-csv-server) (an
open-source index with live seeder counts), the
[Internet Archive](https://archive.org) (public-domain films, audio, software
and books, seeded by the Archive with an HTTPS webseed), and
[SubsPlease](https://subsplease.org) (anime magnets straight from the release
group).
Use `↑↓` to choose a source and
`space` or `enter` to enable or disable it. Changes save to
`~/.tork/config.yaml` and apply to the next search. Current search results and
their magnet resolvers remain usable, even if their source is disabled or
removed. Knaben, YTS, and Nyaa remain enabled by default; existing choices
survive upgrades.

Enabled sources are checked automatically when you open Sources or enable one.
Recent readings are reused for five minutes. Press `r` to recheck all enabled
sources, or `t` to test the selected source (including a disabled one). Checks
send the `1080p` canary using the configured proxy and search timeout. A valid
empty feed is **working · 0 results**. Authentication failures, blocks,
timeouts, non-feed responses, and unsupported torrent links are separate
outcomes. Individual tests never change enabled/disabled choices. Search
reachability does not guarantee that a download will work.

A source blocked by site protection pauses for ten minutes in the current
session; other enabled sources keep returning results. Searches retry it after
the pause expires. `t` retries immediately in Sources or the `s` search-status
panel. A successful check clears the pause. No source is silently disabled,
and retrying never changes your proxy or mirror settings. Rebuilding the source
configuration resets pauses so a changed endpoint can be tried immediately.

The default EZTV integration uses its [official JSON API](https://eztvx.to/api/)
instead of the blocked HTML search. Show names are resolved to IMDb IDs using
[TVmaze's public API](https://www.tvmaze.com/api); those lookups use the same
configured proxy. Searches inspect up to 200 releases for each of the three
closest matching shows. Explicit custom EZTV HTML mirrors remain supported.
1337x tries its maintained public domains; site-side blocking is reported as
**blocked**, and never as a successful empty response. Its operator publishes
domain and availability notices on the [official status page](https://1337x-status.org/).

### Add a source

Press `a` in Sources to open the ready-to-use source catalog. Type a name to
filter, choose with `↑↓`, and press `enter` to add it. Tork saves it enabled,
checks it in the background, and keeps the picker open so you can add another.
Press `esc` when finished to see source check outcomes. No endpoint URL, account,
API key, or additional software is needed for a built-in source. A failed
network check leaves your explicit enabled choice intact; disable it with
`enter` or `space` if you do not want to search it.

- **TheRARBG** adds broad movie, TV, games, software, music and book search.
  This is TheRARBG, a separate public index, not the original RARBG service.
- **LimeTorrents** adds broad movie, TV, games, software, music and anime search.
  Search returns listings immediately; a selected listing's magnet is fetched
  on demand. Sponsored download links are not search results.
- **The Pirate Bay** searches every category, including applications and games.
  The older movie-only and TV-only configurations remain compatible, but the
  add catalog offers the all-category option to avoid confusing duplicates.

The new sources are opt-in. Updating Tork does not silently expand the list of
sites an existing installation searches. Source categories continue through the
shared content filter, including adult categories from all-category searches.

### Advanced: connect your own feed

Choose **Custom RSS / Torznab…** at the bottom of the add catalog. Enter a name,
choose the type with `←→` on the Type row, and enter its endpoint. `tab` moves
between fields; `esc` cancels without saving.

- **Torznab API:** use a public indexer's Torznab endpoint, including one copied
  from Jackett or Prowlarr. Enter its API key in the masked field. If a copied
  URL already includes `apikey=...`, testing moves that key into the masked
  field. Tork supplies `t=search`, `q`, and `extended=1` itself, preserving other
  query parameters such as categories.
- **RSS search feed:** use a search URL with `{query}` where the escaped search
  text belongs. A regular website address is not a search feed.

Press `ctrl+s` to test and save in one step, or `ctrl+t` to test first.
If the check fails, the editor stays open for repair; press `ctrl+s` again to
save it disabled. A successful check
saves the source enabled. Failed or partially supported sources can
be saved **disabled** for later repair. Changing a draft invalidates its check;
an old response cannot validate new settings. Custom-source saves make the
config owner-readable/writable only (`0600` on Unix), including RSS URLs that
contain credentials. Never share a credential-bearing config file.

Use `e` to edit a custom source; it keeps the same result identity when renamed.
Retest edited settings before saving if you want the source enabled. Use `x` to
remove a custom source, then `y` to confirm or `esc` to cancel. Built-ins can be
disabled; their mirror URLs are still edited in `config.yaml`.

Supported feed entries carry a valid v1 magnet in a link, GUID, enclosure, or
Torznab `magneturl` attribute, or a valid v1 infohash attribute. Custom hash-only
entries do not get extra public trackers injected. Private trackers and feeds
that only supply `.torrent` download URLs are unsupported in this release.
Unsupported entries are reported instead of silently appearing as an empty
successful search. Usable results from a partially supported feed still reach
search, without retrying the same permanent format error.

Advanced config example (a Torznab endpoint does not need `{query}`):

```yaml
providers:
  my_public_index:
    name: My public index
    enabled: false
    type: torznab
    search_url: http://localhost:9117/api/v2.0/indexers/INDEXER/results/torznab/api
    api_key: YOUR_API_KEY
  my_rss_index:
    name: My RSS index
    enabled: false
    type: rss
    search_url: 'https://example.org/search/rss?q={query}'
```

Configure and maintain indexers in your indexer manager, then add their
individual Torznab endpoints here. See the [Torznab specification](https://torznab.github.io/spec-1.3-draft/torznab/Specification-v1.3.html)
and [Jackett API documentation](https://github.com/Jackett/Jackett#api-usage).
Authentication, site protection, and upstream network routing managed by that
service remain its responsibility. Tork's requests to the configured endpoint
and its downloads continue to use Tork's configured route.

The screen shows checks from the current session, or the latest saved health
check for a source when available. Individual source tests are kept in memory;
`torkwd doctor --record` and the health screen retain their existing history flow.
To run live metadata-only probes of built-ins without starting a download engine
or editing your config:

```sh
TORK_LIVE_SOURCES=1 go test ./internal/sources -run '^TestLiveBuiltinSources$' -v -count=1
TORK_LIVE_SOURCES=1 go test ./internal/sources -run '^TestLiveBroadSources$' -v -count=1
```

## Keys

Press `?` anywhere for the full list of keys on the current screen. The
highlights:

- **home** type to search, `↓` focus the destination menu, `↑↓` move, `enter` activate the focused control; `esc` returns to the search field, then clears it
- **sources** `↑↓` choose, `space` / `enter` enable or disable, `t` test, `a` add, `e` edit custom, `x` remove custom, `esc` home
- **isos** `↑↓` browse, `enter` grab the latest official image
- **results** `enter` preview (or download when preview is disabled), `/` edit search, `tab` browse categories and destinations, `f` text filter, `o` sort, `z` hide zero seeders, `r` reset filters, `ctrl+f` resolution/size filters, `s` source status, `D` grab now, `Y` copy magnet
- **preview** `enter` download what's selected, `space` toggle a file or folder, `←→` fold, `a`/`n` select all or none
- **downloads** `enter` open the file, `o` show it in your file manager, `p` pause/resume (or retry a held Real-Debrid row), `t` download a refused Real-Debrid row as a torrent, `s` seed (torrents only), `v` fully verify completed data, `m` move, `r` relink, `y` copy full path, `Y` copy magnet, `x` remove from the list and keep the files, `d` stop and delete the downloaded files, partial ones too (for a Real-Debrid row, `c` in either dialog deletes or keeps the cloud copy)
- **settings** `F5`; `↑↓` move, `enter` change or act, `space` toggle, `←→` step through choices
- On home, `tab` / `shift+tab` moves between the search field and menu choices.
- In results, `tab` switches between the list and browse menu; choose Downloads there, or use `^d`. From Linux ISOs, `tab` opens downloads. In downloads, `tab` or `esc` returns to where you were.
- `F1` home, `F2` latest results, `F3` downloads, `F4` sources, `F5` settings. These also work from the search field.
- `esc` goes back or cancels the current dialog; `^c` closes the UI
- Preview, move/relink, and removal dialogs keep focus until finished or cancelled.
- The `?` help panel scrolls with `↑↓` or `pgup/pgdn` on smaller terminals.

Queuing a download leaves you where you are and confirms with a small toast, so
you can grab several things from one search. Live transfer count and speed stay
visible beside the screen title, or in the footer on home. The top bar has no
function-key navigation labels; shortcuts remain available in `?` help. Home has no duplicate
wordmark/header above its search area.

On macOS and Linux, the interactive UI connects to a persistent local session.
Closing the UI with `^c` (or `q` where offered), closing the terminal, or losing
the terminal connection leaves approved downloads and seeding running. Reopen
`torkwd` to reconnect to the same search, selection, and downloads. Pending previews
still require approval; detaching never starts downloading their files.

```sh
torkwd          # start or reconnect to the UI
torkwd status   # check the background session without opening the UI
torkwd stop     # save state and stop transfers, keeping downloaded files
```

One terminal can attach at a time. The worker uses a user-private Unix socket;
it does not expose a remote-control TCP port. It owns the engine and resume state
until `torkwd stop`, and saves progress while the UI is closed. After stopping or
rebooting, start `torkwd` to resume saved downloads; this does not install an OS
startup service. The computer must stay awake for transfers to run.

Use `torkwd --foreground` for the previous behavior (closing the UI stops transfers).
Windows currently retains that foreground behavior. Stop the background session
before running foreground/autopilot transfers, changing `-d` or `--evil`, changing
proxy/engine settings, or testing a rebuilt binary. `--evil` applies to new downloads
in that worker session; it preserves previously saved seeding choices. A rebuilt
client detects an older worker and asks you to stop it before reconnecting.
`--config-dir DIR` gives the interactive UI, `status`, and `stop` a separate config
and session, which is useful for testing. Worker startup diagnostics go to
`~/.tork/session/worker.log` (or the equivalent custom config directory).

Clipboard copies run in the attached terminal, using its clipboard and tmux/SSH
environment. The UI confirms a copy only after that terminal reports the write;
closing the terminal cancels pending copies without stopping downloads.

A result tagged `yts+2` is one torrent that three indexes listed. Rather than
spend three lines on it, tork keeps the fullest release name, the best swarm
numbers any of them reported, and — the part that matters once the download
starts — the combined tracker list, so it announces to every swarm all three
knew about instead of the slice one listing happened to carry. The status line
counts how many listings were folded away; press `v` and the detail panel names
the sources and the tracker total. Results that only link to a details page keep
their own row: without a magnet there is no infohash to compare, and tork will
not fetch every page just to find out.

Search opens a browse workspace with an editable search field above one ranked
results table. Tork keeps its centered layout and green palette. On wide
terminals, a sidebar filters by All, Games, Movies, TV, Anime, Software, Music,
Books, or Other and opens Downloads, Sources, or Home. On narrow terminals,
Tab opens the same menu without squeezing the result titles. Categories use
individual result metadata; unrecognized categories remain under Other.

Press `/` to edit the search in place, `f` to filter the current list, `o` to
cycle relevance/seeders/size, and `z` to show only results with reported seeders.
Category changes do not repeat network searches. `r` clears text, size,
resolution and seeder filters while keeping the selected category. Enter keeps
the existing file preview flow, and Escape returns to the same results.

The selected-result details stay below a fixed-height list. Optional resolution
picks and release groups remain available with `v` and `V`.

Press `ctrl+f` for resolution and size preferences: choose multiple resolutions
with Space, set a maximum size, and choose Balanced, Smaller files, or Higher quality.
Enter applies; Escape cancels. No checked resolutions means all. Choices persist across searches
in the current session. Size limits exclude unknown sizes. Title, year and episode
matches rank before reported swarm counts; these counts do not guarantee speed.
A selected release stays visible while new results stream in. Result rows use
shorter readable titles, with source, size and reported seed counts alongside
them on wider terminals. The original release filename stays in the details.
The source summary separates indexes with results, empty responses, failures,
and searches still running. Press `s` to inspect each source's outcome for this
search, then `s` or Escape to return to the selected release. Search results keep
arriving while the status panel is open.

Advanced text filters still compose with
these choices. Press `f` and try:

```text
res:1080p,2160p seeders:>20 size:<8gb
linux codec:x265 -source:cam
provider:nyaa category:anime is:trusted
is:hdr -is:dv
```

Available fields are `res`, `seeders`, `size`, `source`, `codec`, `provider`,
and `category`. Attributes include `is:trusted`, `is:hdr`, `is:dv`, and
`is:pack`; prefix any structured filter with `-` to negate it.

Verification rehashes completed torrent pieces and direct-download checksums.
Checksum mismatches are moved aside as `.corrupt`, `.corrupt.1`, and so on
before retrying.

## Autopilot (WIP)

Describe what you want and let the cat make a plan:

```sh
torkwd autopilot "all breaking bad seasons 1080p under 40GB"
torkwd autopilot --dry-run --min-seeders 20 "dune 2024 2160p"
```

Autopilot shows its picks, total known size, reasons, and a summary of rejected
results before asking to queue anything. Use `-n N` to cap the picks,
`--max-size 8GB` to cap each download, or `--category movies,anime` to narrow
provider categories. When the same limit appears in several places, the flag
wins over the request text, which wins over config defaults. `--headless` skips
the TUI but still asks before queuing; scripts and cron jobs need `--yes`.
Decisions stay local in `~/.tork/autopilot.jsonl` so a strange choice can be
inspected later.

Persistent defaults can live under `autopilot` in `~/.tork/config.yaml`:

```yaml
autopilot:
  max_downloads: 3
  min_seeders: 10
  max_size_gb: 40
  allowed_categories: [movies, anime]
```

## Legal

tork is a BitTorrent client and search tool. It does not host files, operate
trackers, or control the third-party providers it can search.

Use it only for content you are allowed to download and share, such as official
Linux ISOs, public-domain media, open-source software, and your own files. You
are responsible for following local law and the terms of any provider you
enable.

Provider availability and results can change without notice. tork does not
endorse or guarantee third-party content.

A proxy routes tork's traffic, but it is not a promise of anonymity or legal
protection.

MIT, see [LICENSE](LICENSE).
