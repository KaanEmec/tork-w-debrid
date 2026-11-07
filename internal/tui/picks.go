package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/kaanemec/tork-w-debrid/internal/aggregator"
	"github.com/kaanemec/tork-w-debrid/internal/provider"
	"github.com/kaanemec/tork-w-debrid/internal/rank"
)

var resolutionOrder = []rank.Resolution{rank.Res1080, rank.Res2160, rank.Res720, rank.Res576, rank.Res480, rank.ResUnknown}

func resolutionName(r rank.Resolution) string {
	if r == rank.ResUnknown {
		return "Other / unspecified"
	}
	return r.String()
}

type pickItem struct {
	row        int // index into rows; -1 group header, -2 "+ N more" toggle
	resolution rank.Resolution
	label, key string
	count      int  // releases in the group (headers)
	best       bool // first, best-ranked release of its group
}
type pickPreferences struct {
	resolutions uint8
	size        int
	preference  int
}

var pickSizes = []int64{0, 2 << 30, 5 << 30, 10 << 30, 20 << 30, 40 << 30}
var pickModes = []string{"Balanced", "Smaller files", "Higher quality"}

func (p pickPreferences) allows(row scoredRow) bool {
	return (p.resolutions == 0 || p.resolutions&(1<<uint(row.tags.Resolution)) != 0) && (pickSizes[p.size] == 0 || row.res.SizeBytes > 0 && row.res.SizeBytes <= pickSizes[p.size])
}

// Relevance is compared before swarm size: a popular wrong title or episode
// must not displace a match. Uncertain matches remain available in the group.
func queryMatch(query string, row scoredRow) int {
	q, year := rank.SplitTitle(query)
	title, gotYear := rank.SplitTitle(row.res.Title)
	score := 0
	if q != "" {
		if q == title {
			score = 100
		} else {
			words := strings.Fields(q)
			hits := 0
			for _, w := range words {
				if strings.Contains(" "+title+" ", " "+w+" ") {
					hits++
				}
			}
			if len(words) > 0 {
				score = 60 * hits / len(words)
			}
		}
	}
	if year != "" {
		if year == gotYear {
			score += 40
		} else {
			score -= 80
		}
	}
	t := rank.Parse(query)
	if t.Season > 0 {
		if row.tags.Season == t.Season || row.tags.Season <= t.Season && row.tags.SeasonEnd >= t.Season {
			score += 40
		} else {
			score -= 80
		}
	}
	if t.Episode > 0 {
		if row.tags.Episode == t.Episode {
			score += 40
		} else {
			score -= 80
		}
	}
	return score
}
func (r *resultsModel) pickBetter(i, j int) bool {
	return r.pickRowsBetter(r.rows[i], r.rows[j])
}
func (r *resultsModel) pickRowsBetter(a, b scoredRow) bool {
	am, bm := queryMatch(r.query, a), queryMatch(r.query, b)
	if am != bm {
		return am > bm
	}
	// Do not assume an unrequested language is undesirable.
	usable := func(x scoredRow) bool { return x.res.Seeders > 0 && x.tags.Source != rank.SrcCam }
	if usable(a) != usable(b) {
		return usable(a)
	}
	switch r.pickPrefs.preference {
	case 1:
		if (a.res.SizeBytes > 0) != (b.res.SizeBytes > 0) {
			return a.res.SizeBytes > 0
		}
		if a.res.SizeBytes != b.res.SizeBytes {
			return a.res.SizeBytes < b.res.SizeBytes
		}
	case 2:
		if a.tags.Source != b.tags.Source {
			return a.tags.Source > b.tags.Source
		}
	}
	if a.score != b.score {
		return a.score > b.score
	}
	return rowIdentity(a) < rowIdentity(b)
}

// Resolution groups help video searches, but hide useful games, software and
// books behind "Other". Use one ranked list when most results lack resolution.
func (r *resultsModel) resolutionPicks() bool {
	known := 0
	for _, idx := range r.visible {
		if r.rows[idx].tags.Resolution != rank.ResUnknown {
			known++
		}
	}
	return known > 0 && known*2 >= len(r.visible)
}

func (r *resultsModel) rebuildPicks() {
	oldKey := r.pickKey
	if !r.pickPinned {
		oldKey = ""
	}
	oldOffset := r.pickWin.offset
	grouped := r.resolutionPicks()
	groups := map[rank.Resolution][]int{}
	for _, idx := range r.visible {
		res := rank.ResUnknown
		if grouped {
			res = r.rows[idx].tags.Resolution
		}
		groups[res] = append(groups[res], idx)
	}
	r.picks = nil
	for _, res := range resolutionOrder {
		indices := groups[res]
		if len(indices) == 0 {
			continue
		}
		sort.SliceStable(indices, func(i, j int) bool { return r.pickBetter(indices[i], indices[j]) })
		if grouped {
			r.picks = append(r.picks, pickItem{row: -1, resolution: res, label: resolutionLabel(res), count: len(indices)})
		}
		limit := len(indices)
		if grouped && !r.pickExpanded[res] && limit > 3 {
			limit = 3
		}
		shown := append([]int(nil), indices[:limit]...)
		if r.pickPinned && oldKey != "" && limit < len(indices) {
			for _, idx := range indices[limit:] {
				if rowIdentity(r.rows[idx]) == oldKey {
					shown[len(shown)-1] = idx
					break
				}
			}
		}
		for i, idx := range shown {
			r.picks = append(r.picks, pickItem{row: idx, resolution: res, key: rowIdentity(r.rows[idx]), best: grouped && i == 0 && idx == indices[0]})
		}
		if grouped && len(indices) > 3 {
			label := fmt.Sprintf("+ %d more", len(indices)-3)
			if r.pickExpanded[res] {
				label = "- fewer"
			}
			r.picks = append(r.picks, pickItem{row: -2, resolution: res, label: label, key: "more:" + resolutionName(res)})
		}
	}
	r.pickWin.cursor = 0
	if oldKey != "" {
		for i, it := range r.picks {
			if it.key == oldKey {
				r.pickWin.cursor = i
				break
			}
		}
	}
	r.movePick(0, max(1, len(r.picks)))
	r.pickWin.offset = oldOffset
}
func (r *resultsModel) movePick(delta, height int) {
	r.pickWin.move(delta, len(r.picks), height)
	direction := 1
	if delta < 0 {
		direction = -1
	}
	for r.pickWin.cursor >= 0 && r.pickWin.cursor < len(r.picks) && r.picks[r.pickWin.cursor].row == -1 {
		next := r.pickWin.cursor + direction
		if next < 0 || next >= len(r.picks) {
			direction = -direction
			next = r.pickWin.cursor + direction
		}
		r.pickWin.cursor = next
	}
	r.pickWin.clamp(len(r.picks), height)
	if r.pickWin.cursor < len(r.picks) {
		r.pickKey = r.picks[r.pickWin.cursor].key
	}
}

// pickRows is the list window: the body minus the column header and, on a
// tall enough body, the blank line and the detail line under the list.
func (a *App) pickRows() int {
	if a.bodyHeight() < 8 {
		return max(1, a.bodyHeight()-1)
	}
	return max(1, a.bodyHeight()-3)
}

// resolutionLabel names a group header: "2160p", or "other".
func resolutionLabel(r rank.Resolution) string {
	if r == rank.ResUnknown {
		return "other"
	}
	return r.String()
}
func (a *App) updatePickKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	r := &a.results
	switch msg.String() {
	case "up", "k", "down", "j", "pgup", "pgdown", "g", "home", "G", "end":
		r.pickPinned = true
	}
	switch msg.String() {
	case "up", "k":
		r.movePick(-1, a.pickRows())
	case "down", "j":
		r.movePick(1, a.pickRows())
	case "pgup":
		r.movePick(-a.pickRows(), a.pickRows())
	case "pgdown":
		r.movePick(a.pickRows(), a.pickRows())
	case "g", "home":
		r.pickWin.home()
		r.movePick(0, a.pickRows())
	case "G", "end":
		r.pickWin.end(len(r.picks), a.pickRows())
		r.movePick(0, a.pickRows())
	case "enter", "D", "Y":
		if r.pickWin.cursor >= len(r.picks) {
			return a, nil
		}
		it := r.picks[r.pickWin.cursor]
		if it.row == -2 {
			if r.pickExpanded == nil {
				r.pickExpanded = map[rank.Resolution]bool{}
			}
			r.pickExpanded[it.resolution] = !r.pickExpanded[it.resolution]
			r.rebuildPicks()
			return a, nil
		}
		if it.row >= 0 {
			res := r.rows[it.row].res
			switch msg.String() {
			case "D":
				return a, a.downloadResultDirect(res)
			case "Y":
				return a, a.yankResult(res)
			}
			return a, a.downloadResult(res)
		}
	}
	return a, nil
}

// viewPicks is the Results screen (design.md § Results): releases grouped by
// resolution in four columns (release, details, size, seeds), ★ on each
// group's best pick, and one detail line above the footer. While a search
// streams, new rows fade in and the header runs the copper sweep.
func (a *App) viewPicks() string {
	r := &a.results
	w := a.contentWidth()
	ctx := r.query
	if ctx == "" {
		ctx = "results"
	}
	help := a.keyStrip(a.helpBudget(w))
	if len(r.picks) == 0 {
		label := "No matches with these filters. Press / to adjust or reset."
		if r.searching {
			label = "Searching… picks appear as sources respond."
		} else if len(r.rows) == 0 {
			label = "No results. Try another search, or press s to check sources."
		}
		if r.query == "" {
			label = "Type a search on home to get started."
		}
		return a.chrome(ctx, "\n"+styleDim.Render(label), help)
	}
	lay := newPickLayout(w - 1)
	body := " " + lay.header() + "\n"
	body += renderWindow(&r.pickWin, len(r.picks), a.pickRows(), w, func(i int, selected bool) string {
		it := r.picks[i]
		switch it.row {
		case -1:
			return " " + styleBrand.Render(it.label) + styleDim.Render(fmt.Sprintf("  %d", it.count))
		case -2:
			return "  " + styleDim.Render(it.label)
		}
		row := r.rows[it.row]
		return lay.render(row, selected, it.best, a.age(row.born))
	})
	if a.bodyHeight() >= 8 {
		body += "\n\n" + a.pickDetailLine(w)
	}
	return a.chrome(ctx, body, help)
}

// pickDetailLine is the line above the footer: source, merge count, trackers
// and the checks in muted, with "☁ via Real-Debrid" on the right when
// Real-Debrid handles downloads.
func (a *App) pickDetailLine(w int) string {
	r := &a.results
	right := ""
	if a.debridRouting() {
		right = viaRoute()
	}
	left := ""
	if it := r.picks[r.pickWin.cursor]; it.row >= 0 {
		row := r.rows[it.row]
		parts := []string{sourceTag(row.res, true)}
		if n := len(row.res.Trackers()); n > 0 {
			parts = append(parts, plural(n, "tracker"))
		}
		check := "looks clean"
		if reasons := rank.NoiseReasons(r.query, row.res.Title, row.tags, row.res.Seeders); len(reasons) > 0 {
			check = "check: " + strings.Join(reasons, ", ")
		}
		parts = append(parts, check, row.res.Title) // the untouched provider title
		left = styleDim.Render(strings.Join(parts, " · "))
	} else if it.row == -2 {
		left = styleDim.Render("enter " + strings.TrimPrefix(it.label, "+ ") + " releases")
		if r.pickExpanded[it.resolution] {
			left = styleDim.Render("enter shows fewer releases")
		}
	}
	return splitResultLine(" "+left, right, w)
}

// pickLayout is the Results grid: release (34), details (24, muted), size
// (10, right), seeds (8, right) and the ★ cell. Details drop under 76 columns.
type pickLayout struct{ title, details, size, seeds int }

const pickStarW = 3

func newPickLayout(width int) pickLayout {
	l := pickLayout{size: 10, seeds: 8}
	if width >= 76 {
		l.details = 24
	}
	l.title = max(8, min(34, width-1-l.details-l.size-l.seeds-pickStarW))
	if width < 1+l.title+l.size+l.seeds+pickStarW {
		l.seeds = 0
	}
	return l
}

func (l pickLayout) header() string {
	line := " " + strings.Repeat(" ", l.title+l.details) + padLeft("size", l.size)
	if l.seeds > 0 {
		line += padLeft("seeds", l.seeds)
	}
	return styleDim.Render(line)
}

// render draws one release row. Seeds are amber under 5 and a muted "—" at
// zero (the whole row goes muted); the best pick of a group gets an amber ★.
func (l pickLayout) render(row scoredRow, selected, best bool, age int) string {
	titleStyle := styleText
	switch {
	case selected:
		titleStyle = styleFg.Bold(true)
	case row.res.Seeders <= 0:
		titleStyle = styleDim
	}
	titleStyle = fadeStyle(age, titleStyle)
	size := "?"
	if row.res.SizeBytes > 0 {
		size = humanBytes(row.res.SizeBytes)
	}
	// A cut title keeps a two-cell gap before the details, as details do.
	line := " " + titleStyle.Render(padRight(truncate(readableReleaseTitle(row.res.Title), l.title-2), l.title))
	if l.details > 0 {
		details := releaseQuality(row)
		if details == "source unknown" {
			details = "—"
		}
		line += fadeStyle(age, styleDim).Render(padRight(truncate(details, l.details-2), l.details))
	}
	line += fadeStyle(age, styleText).Render(padLeft(truncate(size, l.size), l.size))
	if l.seeds > 0 {
		seeds, st := fmt.Sprint(row.res.Seeders), styleText
		switch {
		case row.res.Seeders <= 0:
			seeds, st = "—", styleDim
		case row.res.Seeders < 5:
			st = styleWarn
		}
		line += fadeStyle(age, st).Render(padLeft(seeds, l.seeds))
	}
	if best {
		line += styleBest.Render("  " + glyphs.star)
	}
	return line
}

// resultsFact is the Results header's right side: a spinner and
// "searching · 5 of 7 sources" while the search streams, then
// "128 results · 7 sources".
func (a *App) resultsFact() string {
	r := &a.results
	if r.query == "" || a.agg == nil {
		return ""
	}
	total := len(a.agg.Providers())
	if r.searching {
		answered := 0
		for _, p := range a.agg.Providers() {
			if ev, ok := r.status[provider.DisplayName(p)]; ok && ev.State != aggregator.StateSearching {
				answered++
			}
		}
		return spinnerGlyph(a.vframe()) + styleDim.Render(fmt.Sprintf(" searching · %d of %d sources", answered, total))
	}
	count := fmt.Sprintf("%d results", len(r.rows))
	if len(r.visible) != len(r.rows) {
		count = fmt.Sprintf("%d/%d results", len(r.visible), len(r.rows))
	}
	return styleDim.Render(count + " · " + plural(total, "source"))
}

func detailRule(label string, width int) string {
	label = truncate(label, width)
	line := styleKey.Render(label)
	if remaining := width - lipgloss.Width(label) - 2; remaining > 0 {
		line += "  " + rule(remaining)
	}
	return truncate(line, width)
}
