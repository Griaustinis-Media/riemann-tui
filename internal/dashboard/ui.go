package dashboard

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Griaustinis-Media/riemann-tui/pkg/event"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func (d *Dashboard) build() {
	// ── Header bar ─────────────────────────────────────────────────────
	d.header = tview.NewTextView().SetDynamicColors(true)
	d.header.SetBackgroundColor(tcell.ColorDarkBlue)

	// ── Services table (left panel) ────────────────────────────────────
	d.svcTbl = tview.NewTable().SetFixed(1, 0).SetSelectable(true, false)
	d.svcTbl.SetBorder(true).
		SetTitle("[::b] Services ").
		SetTitleAlign(tview.AlignLeft).
		SetBorderColor(tcell.ColorDimGray)
	d.svcTbl.SetSelectedStyle(
		tcell.StyleDefault.Background(tcell.ColorDarkSlateBlue).Foreground(tcell.ColorWhite),
	)
	d.svcTbl.SetSelectionChangedFunc(func(row, _ int) {
		if d.svcRebuilding || row <= 0 {
			return
		}
		cell := d.svcTbl.GetCell(row, 0)
		if cell == nil {
			return
		}
		if ref := cell.GetReference(); ref != nil {
			if key, ok := ref.(string); ok {
				d.filterKey = key
			}
		}
	})
	d.setSvcHeaders()

	// ── Events table (right panel) ─────────────────────────────────────
	d.evtTbl = tview.NewTable().SetFixed(1, 0).SetSelectable(false, false)
	d.evtTbl.SetBorder(true).
		SetTitle("[::b] Live Events ").
		SetTitleAlign(tview.AlignLeft).
		SetBorderColor(tcell.ColorDimGray)
	d.setEvtHeaders()

	// ── Empty state (shown instead of table when no events) ────────────
	d.evtEmpty = tview.NewTextView().SetDynamicColors(true).SetTextAlign(tview.AlignCenter)
	d.evtEmpty.SetBorder(true).
		SetTitle("[::b] Live Events ").
		SetTitleAlign(tview.AlignLeft).
		SetBorderColor(tcell.ColorDimGray)

	d.evtPages = tview.NewPages()
	d.evtPages.AddPage("table", d.evtTbl, true, false)
	d.evtPages.AddPage("empty", d.evtEmpty, true, true)

	// ── Detail page ────────────────────────────────────────────────────
	d.detail = tview.NewTextView().SetDynamicColors(true).SetScrollable(true)
	d.detail.SetBorder(true).
		SetTitle(" [ Event Detail ] ").
		SetTitleAlign(tview.AlignLeft).
		SetBorderColor(tcell.ColorTeal)

	// ── Status bar ─────────────────────────────────────────────────────
	d.statusBar = tview.NewTextView().SetDynamicColors(true)
	d.statusBar.SetBackgroundColor(tcell.ColorDarkBlue)

	// ── Main page layout ───────────────────────────────────────────────
	mainBody := tview.NewFlex().
		AddItem(d.svcTbl, 32, 0, false).
		AddItem(d.evtPages, 0, 1, true)

	mainPage := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(d.header, 1, 0, false).
		AddItem(mainBody, 0, 1, true).
		AddItem(d.statusBar, 1, 0, false)

	// ── Detail page layout ─────────────────────────────────────────────
	hint := tview.NewTextView().SetDynamicColors(true).
		SetText("  [darkgray]Esc / q: back[-]")
	hint.SetBackgroundColor(tcell.ColorDarkBlue)

	detailPage := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(hint, 1, 0, false).
		AddItem(d.detail, 0, 1, true)

	d.pages.AddPage("main", mainPage, true, true)
	d.pages.AddPage("detail", detailPage, true, false)

	d.buildGraphPage()

	d.app.SetRoot(d.pages, true).EnableMouse(true)
	d.app.SetInputCapture(d.handleKey)
}

func (d *Dashboard) setSvcHeaders() {
	for i, h := range []string{"", "SERVICE", "HOST", "METRIC"} {
		d.svcTbl.SetCell(0, i,
			tview.NewTableCell(h).
				SetTextColor(tcell.ColorYellow).
				SetSelectable(false).
				SetAttributes(tcell.AttrBold))
	}
}

func (d *Dashboard) setEvtHeaders() {
	for i, h := range []string{"TIME", "HOST", "SERVICE", "STATE", "METRIC", "TAGS", "ATTRS"} {
		d.evtTbl.SetCell(0, i,
			tview.NewTableCell(" "+h+" ").
				SetTextColor(tcell.ColorYellow).
				SetSelectable(false).
				SetAttributes(tcell.AttrBold))
	}
}

func (d *Dashboard) handleKey(ev *tcell.EventKey) *tcell.EventKey {
	front, _ := d.pages.GetFrontPage()

	if front == "detail" || front == "graph" {
		if ev.Key() == tcell.KeyEsc || ev.Rune() == 'q' || ev.Rune() == 'Q' {
			d.pages.SwitchToPage("main")
			d.app.SetFocus(d.svcTbl)
			return nil
		}
		return ev
	}

	switch {
	case ev.Key() == tcell.KeyEsc:
		if d.filterKey != "" {
			d.filterKey = ""
			return nil
		}
		d.app.Stop()
		return nil
	case ev.Rune() == 'q' || ev.Rune() == 'Q':
		d.app.Stop()
		return nil
	case ev.Key() == tcell.KeyTab:
		if d.app.GetFocus() == d.svcTbl {
			d.app.SetFocus(d.evtTbl)
		} else {
			d.app.SetFocus(d.svcTbl)
		}
		return nil
	case ev.Key() == tcell.KeyEnter:
		if d.app.GetFocus() == d.svcTbl {
			d.showServiceDetail()
		}
		return nil
	case ev.Rune() == 'g' || ev.Rune() == 'G':
		if d.app.GetFocus() == d.svcTbl {
			d.openGraph()
		}
		return nil
	}
	return ev
}

func (d *Dashboard) showServiceDetail() {
	row, _ := d.svcTbl.GetSelection()
	if row <= 0 {
		return
	}
	cell := d.svcTbl.GetCell(row, 0)
	if cell == nil {
		return
	}
	ref := cell.GetReference()
	if ref == nil {
		return
	}
	key, ok := ref.(string)
	if !ok {
		return
	}

	d.mu.Lock()
	e, ok := d.summary[key]
	d.mu.Unlock()
	if !ok {
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "\n")
	fmt.Fprintf(&sb, "  [yellow::b]HOST[-]         [white]%s[-]\n", e.Host)
	fmt.Fprintf(&sb, "  [yellow::b]SERVICE[-]      [white]%s[-]\n", e.Service)
	fmt.Fprintf(&sb, "  [yellow::b]STATE[-]        [%s]%s[-]\n", stateColorTag(e.State), e.State)
	if m := e.MetricStr(); m != "" {
		fmt.Fprintf(&sb, "  [yellow::b]METRIC[-]       [white]%s[-]\n", m)
	}
	fmt.Fprintf(&sb, "  [yellow::b]TIME[-]         [darkgray]%s[-]\n", e.TimeStr())
	if e.TTL > 0 {
		fmt.Fprintf(&sb, "  [yellow::b]TTL[-]          [darkgray]%.0f s[-]\n", e.TTL)
	}
	if e.Description != "" {
		fmt.Fprintf(&sb, "  [yellow::b]DESCRIPTION[-]  [white]%s[-]\n", e.Description)
	}
	if len(e.Tags) > 0 {
		fmt.Fprintf(&sb, "  [yellow::b]TAGS[-]         [cyan]%s[-]\n", strings.Join(e.Tags, ", "))
	}
	if len(e.Attributes) > 0 {
		attrKeys := make([]string, 0, len(e.Attributes))
		for k := range e.Attributes {
			attrKeys = append(attrKeys, k)
		}
		sort.Strings(attrKeys)
		fmt.Fprintf(&sb, "\n  [yellow::b]ATTRIBUTES[-]\n")
		for _, k := range attrKeys {
			fmt.Fprintf(&sb, "    [cyan]%-24s[-] [white]%s[-]\n", k, e.Attributes[k])
		}
	}

	d.detail.SetText(sb.String())
	d.detail.ScrollToBeginning()
	d.detail.SetTitle(fmt.Sprintf(" [ %s :: %s ] ", e.Host, e.Service))
	d.pages.SwitchToPage("detail")
}

// emptyStateText returns a descriptive message for the empty events panel.
func emptyStateText(conn, connErr string, connectedAt time.Time, scheme, addr, path, query string) string {
	wsURL := fmt.Sprintf("%s://%s%s", scheme, addr, path)
	var sb strings.Builder
	sb.WriteString("\n\n\n")
	switch conn {
	case "connected":
		elapsed := time.Since(connectedAt).Round(time.Second)
		sb.WriteString("[#00ff87]● Connected[-]\n\n")
		sb.WriteString(fmt.Sprintf("[darkgray]%s[-]\n\n", wsURL))
		sb.WriteString(fmt.Sprintf("Waiting for events...  [darkgray](%s)[-]\n\n", elapsed))
		sb.WriteString(fmt.Sprintf("[darkgray]Query: [white]%s[-]\n\n", query))
		sb.WriteString("[darkgray]No events match your query yet.\n")
		sb.WriteString(`Try [white]--query true[-][darkgray] to see all events.[-]`)
	case "connecting":
		sb.WriteString("[yellow]● Connecting...[-]\n\n")
		sb.WriteString(fmt.Sprintf("[darkgray]%s[-]\n", wsURL))
	default:
		sb.WriteString("[red]● Connection failed[-]\n\n")
		sb.WriteString(fmt.Sprintf("[darkgray]%s[-]\n\n", wsURL))
		if connErr != "" {
			sb.WriteString(fmt.Sprintf("[red]%s[-]\n\n", connErr))
		}
		sb.WriteString("[darkgray]Retrying in 3s...[-]")
	}
	return sb.String()
}

func (d *Dashboard) refreshUI() {
	d.mu.Lock()

	now := time.Now()
	if elapsed := now.Sub(d.rateStart).Seconds(); elapsed >= 1.0 {
		d.rate = float64(d.rateCount) / elapsed
		d.rateCount = 0
		d.rateStart = now
	}

	// Evict summary entries whose TTL has elapsed.
	for k, e := range d.summary {
		if exp := e.ExpiresAt(); !exp.IsZero() && now.After(exp) {
			delete(d.summary, k)
		}
	}

	stream := make([]event.RiemannEvent, len(d.stream))
	copy(stream, d.stream)
	summary := make(map[string]event.RiemannEvent, len(d.summary))
	for k, v := range d.summary {
		summary[k] = v
	}
	total := d.total
	rate := d.rate
	conn := d.conn
	connErr := d.connErr
	connectedAt := d.connectedAt
	lastEventAt := d.lastEventAt
	d.mu.Unlock()

	// ── Header ─────────────────────────────────────────────────────────
	var dot, label string
	switch conn {
	case "connected":
		dot, label = "[#00ff87]●[-]", "[#00ff87]connected[-]"
	case "connecting":
		dot, label = "[yellow]●[-]", "[yellow]connecting[-]"
	default:
		msg := conn
		if connErr != "" {
			msg = connErr
		}
		dot, label = "[red]●[-]", fmt.Sprintf("[red]%s[-]", msg)
	}
	d.header.SetText(fmt.Sprintf(
		"  [::b]Riemann TUI[-]   %s %s   [darkgray]%s://%s%s   query: [white]%s[-]",
		dot, label, d.wsScheme, d.wsAddr, d.wsPath, d.query,
	))

	// ── Services table ─────────────────────────────────────────────────
	keys := make([]string, 0, len(summary))
	for k := range summary {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	selRow, _ := d.svcTbl.GetSelection()
	d.svcRebuilding = true
	d.svcTbl.Clear()
	d.setSvcHeaders()
	for row, k := range keys {
		e := summary[k]
		sc := stateColor(e.State)
		svc := e.Service
		if len(svc) > 18 {
			svc = svc[:17] + "…"
		}
		host := e.Host
		if len(host) > 12 {
			host = host[:11] + "…"
		}
		indicator := "●"
		if k == d.filterKey {
			indicator = "▶"
		}
		d.svcTbl.SetCell(row+1, 0, tview.NewTableCell(indicator).SetTextColor(sc).SetReference(k))
		d.svcTbl.SetCell(row+1, 1, tview.NewTableCell(svc).SetTextColor(tcell.ColorWhite))
		d.svcTbl.SetCell(row+1, 2, tview.NewTableCell(host).SetTextColor(tcell.ColorGray))
		d.svcTbl.SetCell(row+1, 3, tview.NewTableCell(e.MetricStr()).SetTextColor(tcell.ColorYellow))
	}
	if selRow > 0 && selRow <= len(keys) {
		d.svcTbl.Select(selRow, 0)
	}
	d.svcRebuilding = false

	// ── Filter stream ───────────────────────────────────────────────────
	filtered := stream
	if d.filterKey != "" {
		filtered = stream[:0:0]
		for _, e := range stream {
			if e.Host+"\x00"+e.Service == d.filterKey {
				filtered = append(filtered, e)
			}
		}
	}

	// ── Events panel title ──────────────────────────────────────────────
	if d.filterKey != "" {
		parts := strings.SplitN(d.filterKey, "\x00", 2)
		title := fmt.Sprintf("[::b] %s  [darkgray]@ %s  [darkgray::i](Esc to clear)[-] ", parts[1], parts[0])
		d.evtTbl.SetTitle(title)
		d.evtEmpty.SetTitle(title)
	} else {
		d.evtTbl.SetTitle("[::b] Live Events ")
		d.evtEmpty.SetTitle("[::b] Live Events ")
	}

	// ── Events: populate table or show empty state ─────────────────────
	if len(filtered) == 0 {
		d.evtPages.SwitchToPage("empty")
		d.evtEmpty.SetText(emptyStateText(conn, connErr, connectedAt, d.wsScheme, d.wsAddr, d.wsPath, d.query))
	} else {
		d.evtPages.SwitchToPage("table")
		d.evtTbl.Clear()
		d.setEvtHeaders()
		for i := len(filtered) - 1; i >= 0; i-- {
			row := len(filtered) - i
			e := filtered[i]
			sc := stateColor(e.State)
			tags := e.TagsStr()
			if len(tags) > 22 {
				tags = tags[:21] + "…"
			}
			attrs := e.AttrsStr()
			if len(attrs) > 40 {
				attrs = attrs[:39] + "…"
			}
			d.evtTbl.SetCell(row, 0, tview.NewTableCell(e.TimeStr()).SetTextColor(tcell.ColorGray))
			d.evtTbl.SetCell(row, 1, tview.NewTableCell(e.Host).SetTextColor(tcell.ColorAqua))
			d.evtTbl.SetCell(row, 2, tview.NewTableCell(e.Service).SetTextColor(tcell.ColorWhite))
			d.evtTbl.SetCell(row, 3, tview.NewTableCell(e.State).SetTextColor(sc))
			d.evtTbl.SetCell(row, 4, tview.NewTableCell(e.MetricStr()).SetTextColor(tcell.ColorYellow))
			d.evtTbl.SetCell(row, 5, tview.NewTableCell(tags).SetTextColor(tcell.ColorGray))
			d.evtTbl.SetCell(row, 6, tview.NewTableCell(attrs).SetTextColor(tcell.ColorDarkCyan))
		}
	}

	// ── Status bar ─────────────────────────────────────────────────────
	var lastEvtStr string
	if !lastEventAt.IsZero() {
		ago := time.Since(lastEventAt).Round(time.Second)
		lastEvtStr = fmt.Sprintf("  Last event: [white]%s ago[-]", ago)
	}
	d.statusBar.SetText(fmt.Sprintf(
		"  Events: [white]%d[-]   Rate: [white]%.1f/s[-]   Services: [white]%d[-]%s   "+
			"[darkgray]Tab: switch panel   Enter: detail   g: graph   Esc: clear filter   q: quit[-]",
		total, rate, len(summary), lastEvtStr,
	))

	front, _ := d.pages.GetFrontPage()
	if front == "graph" {
		d.updateGraphHeader()
	}
}
