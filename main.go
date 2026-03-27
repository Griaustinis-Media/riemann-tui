package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/gorilla/websocket"
	"github.com/rivo/tview"
)

const (
	maxStream    = 500
	refreshRate  = 200 * time.Millisecond
	pingInterval = 30 * time.Second
	pongWait     = 10 * time.Second
)

// AttrMap decodes Riemann attributes from either a JSON object
// {"k":"v"} or the cheshire array format [{"key":"k","value":"v"}].
type AttrMap map[string]string

func (a *AttrMap) UnmarshalJSON(b []byte) error {
	// Try array of {key, value} first (cheshire / standard Riemann wire format)
	var arr []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if json.Unmarshal(b, &arr) == nil {
		m := make(map[string]string, len(arr))
		for _, kv := range arr {
			m[kv.Key] = kv.Value
		}
		*a = m
		return nil
	}
	// Fall back to plain object
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*a = m
	return nil
}

// RiemannEvent is a Riemann monitoring event decoded from JSON.
// Riemann WebSocket transport converts protobuf to JSON via cheshire.
// The metric field may appear as a unified "metric" or as typed variants.
type RiemannEvent struct {
	Host        string      `json:"host"`
	Service     string      `json:"service"`
	State       string      `json:"state"`
	Description string      `json:"description"`
	Tags        []string    `json:"tags"`
	TTL         float64     `json:"ttl"`
	Time        interface{} `json:"time"` // string ISO-8601 or int64 unix epoch
	TimeMicros  int64       `json:"time_micros"`
	// Metric variants: Riemann protobuf has metric_sint64, metric_d, metric_f
	// Some transports unify them under "metric"
	Metric       interface{} `json:"metric"`
	MetricSint64 *int64      `json:"metric_sint64"`
	MetricD      *float64    `json:"metric_d"`
	MetricF      *float32    `json:"metric_f"`
	Attributes   AttrMap     `json:"attributes"`
}

func (e RiemannEvent) metricStr() string {
	if e.Metric != nil {
		switch v := e.Metric.(type) {
		case float64:
			if v == float64(int64(v)) && v < 1e15 {
				return fmt.Sprintf("%d", int64(v))
			}
			return fmt.Sprintf("%.4g", v)
		default:
			return fmt.Sprintf("%v", v)
		}
	}
	if e.MetricD != nil {
		return fmt.Sprintf("%.4g", *e.MetricD)
	}
	if e.MetricF != nil {
		return fmt.Sprintf("%.4g", *e.MetricF)
	}
	if e.MetricSint64 != nil {
		return fmt.Sprintf("%d", *e.MetricSint64)
	}
	return ""
}

func (e RiemannEvent) timeStr() string {
	if e.TimeMicros != 0 {
		return time.UnixMicro(e.TimeMicros).Local().Format("15:04:05")
	}
	switch v := e.Time.(type) {
	case string:
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t.Local().Format("15:04:05")
		}
	case float64: // JSON numbers decode as float64
		return time.Unix(int64(v), 0).Format("15:04:05")
	}
	return time.Now().Format("15:04:05")
}

// eventTime returns the event timestamp, falling back to zero.
func (e RiemannEvent) eventTime() time.Time {
	if e.TimeMicros != 0 {
		return time.UnixMicro(e.TimeMicros)
	}
	switch v := e.Time.(type) {
	case string:
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t
		}
	case float64:
		return time.Unix(int64(v), 0)
	}
	return time.Time{}
}

// expiresAt returns the time after which this event should be evicted from the
// summary, or zero if the event has no TTL.
func (e RiemannEvent) expiresAt() time.Time {
	if e.TTL <= 0 {
		return time.Time{}
	}
	t := e.eventTime()
	if t.IsZero() {
		return time.Time{}
	}
	return t.Add(time.Duration(e.TTL * float64(time.Second)))
}

func (e RiemannEvent) tagsStr() string {
	return strings.Join(e.Tags, " ")
}

func (e RiemannEvent) attrsStr() string {
	if len(e.Attributes) == 0 {
		return ""
	}
	keys := make([]string, 0, len(e.Attributes))
	for k := range e.Attributes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+e.Attributes[k])
	}
	return strings.Join(parts, " ")
}

func stateColor(state string) tcell.Color {
	switch strings.ToLower(state) {
	case "ok":
		return tcell.NewRGBColor(0, 255, 135)
	case "warning", "warn":
		return tcell.ColorYellow
	case "critical", "error":
		return tcell.NewRGBColor(255, 95, 95)
	default:
		return tcell.ColorGray
	}
}

func stateColorTag(state string) string {
	switch strings.ToLower(state) {
	case "ok":
		return "#00ff87"
	case "warning", "warn":
		return "yellow"
	case "critical", "error":
		return "#ff5f5f"
	default:
		return "gray"
	}
}

// Dashboard holds all UI state and the WebSocket connection logic.
type Dashboard struct {
	app   *tview.Application
	pages *tview.Pages

	header    *tview.TextView
	svcTbl    *tview.Table
	evtPages  *tview.Pages   // switches between evtTbl and evtEmpty
	evtTbl    *tview.Table
	evtEmpty  *tview.TextView // shown when no events yet
	detail    *tview.TextView
	statusBar *tview.TextView

	mu          sync.Mutex
	stream      []RiemannEvent          // recent event stream (ring buffer)
	summary     map[string]RiemannEvent // latest event per "host\x00service"
	connectedAt time.Time               // zero if not currently connected
	lastEventAt time.Time               // zero if no events received yet

	total     int64
	rateCount int64
	rateStart time.Time
	rate      float64

	// filterKey is "host\x00service" of the selected service, empty = show all.
	// Only accessed from the UI goroutine — no mutex needed.
	filterKey    string
	svcRebuilding bool // suppresses filterKey updates during table rebuild

	wsScheme string // "ws" or "wss"
	wsAddr   string
	wsPath   string
	query    string
	insecure bool
	debugLog *os.File
	conn     string // "connecting" | "connected" | "disconnected"
	connErr  string
}

func newDashboard(scheme, addr, path, query string, insecure bool) *Dashboard {
	d := &Dashboard{
		app:       tview.NewApplication(),
		pages:     tview.NewPages(),
		summary:   make(map[string]RiemannEvent),
		wsScheme:  scheme,
		wsAddr:    addr,
		wsPath:    path,
		query:     query,
		insecure:  insecure,
		conn:      "connecting",
		rateStart: time.Now(),
	}
	d.build()
	return d
}

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

	if front == "detail" {
		if ev.Key() == tcell.KeyEsc || ev.Rune() == 'q' || ev.Rune() == 'Q' {
			d.pages.SwitchToPage("main")
			return nil
		}
		return ev
	}

	// Main page input
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
	if m := e.metricStr(); m != "" {
		fmt.Fprintf(&sb, "  [yellow::b]METRIC[-]       [white]%s[-]\n", m)
	}
	fmt.Fprintf(&sb, "  [yellow::b]TIME[-]         [darkgray]%s[-]\n", e.timeStr())
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

func (d *Dashboard) addEvent(e RiemannEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.total++
	d.rateCount++
	d.lastEventAt = time.Now()
	d.stream = append(d.stream, e)
	if len(d.stream) > maxStream {
		d.stream = d.stream[1:]
	}
	d.summary[e.Host+"\x00"+e.Service] = e
}

func (d *Dashboard) setConnStatus(status, errMsg string) {
	d.mu.Lock()
	d.conn = status
	d.connErr = errMsg
	if status == "connected" {
		d.connectedAt = time.Now()
	} else {
		d.connectedAt = time.Time{}
	}
	d.mu.Unlock()
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
		if exp := e.expiresAt(); !exp.IsZero() && now.After(exp) {
			delete(d.summary, k)
		}
	}

	stream := make([]RiemannEvent, len(d.stream))
	copy(stream, d.stream)
	summary := make(map[string]RiemannEvent, len(d.summary))
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
		// Highlight the row that matches the active filter
		indicator := "●"
		if k == d.filterKey {
			indicator = "▶"
		}
		d.svcTbl.SetCell(row+1, 0, tview.NewTableCell(indicator).SetTextColor(sc).SetReference(k))
		d.svcTbl.SetCell(row+1, 1, tview.NewTableCell(svc).SetTextColor(tcell.ColorWhite))
		d.svcTbl.SetCell(row+1, 2, tview.NewTableCell(host).SetTextColor(tcell.ColorGray))
		d.svcTbl.SetCell(row+1, 3, tview.NewTableCell(e.metricStr()).SetTextColor(tcell.ColorYellow))
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
			tags := e.tagsStr()
			if len(tags) > 22 {
				tags = tags[:21] + "…"
			}
			attrs := e.attrsStr()
			if len(attrs) > 40 {
				attrs = attrs[:39] + "…"
			}
			d.evtTbl.SetCell(row, 0, tview.NewTableCell(e.timeStr()).SetTextColor(tcell.ColorGray))
			d.evtTbl.SetCell(row, 1, tview.NewTableCell(e.Host).SetTextColor(tcell.ColorAqua))
			d.evtTbl.SetCell(row, 2, tview.NewTableCell(e.Service).SetTextColor(tcell.ColorWhite))
			d.evtTbl.SetCell(row, 3, tview.NewTableCell(e.State).SetTextColor(sc))
			d.evtTbl.SetCell(row, 4, tview.NewTableCell(e.metricStr()).SetTextColor(tcell.ColorYellow))
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
			"[darkgray]Tab: switch panel   Enter: detail   Esc: clear filter   q: quit[-]",
		total, rate, len(summary), lastEvtStr,
	))
}

func (d *Dashboard) run() error {
	go d.wsLoop()
	go func() {
		t := time.NewTicker(refreshRate)
		defer t.Stop()
		for range t.C {
			d.app.QueueUpdateDraw(d.refreshUI)
		}
	}()
	return d.app.Run()
}

func (d *Dashboard) wsLoop() {
	for {
		if err := d.wsConnect(); err != nil {
			d.setConnStatus("disconnected", err.Error())
		}
		time.Sleep(3 * time.Second)
		d.setConnStatus("connecting", "")
	}
}

func (d *Dashboard) wsConnect() error {
	u := &url.URL{Scheme: d.wsScheme, Host: d.wsAddr, Path: d.wsPath}
	q := u.Query()
	q.Set("subscribe", "true")
	q.Set("query", d.query)
	u.RawQuery = q.Encode()

	dialer := websocket.DefaultDialer
	if d.insecure {
		dialer = &websocket.Dialer{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			Proxy:           http.ProxyFromEnvironment,
		}
	}

	headers := http.Header{
		"Origin": []string{"http://" + d.wsAddr},
	}
	if d.debugLog != nil {
		fmt.Fprintf(d.debugLog, "[%s] dialing %s\n", time.Now().Format(time.RFC3339), u.String())
	}
	conn, resp, err := dialer.Dial(u.String(), headers)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			resp.Body.Close()
			hint := strings.TrimSpace(string(body))
			if hint == "" {
				hint = resp.Status
			}
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, hint)
		}
		return err
	}
	defer conn.Close()

	// Riemann WebSocket protocol: send a subscription message after the
	// handshake. URL query params alone are not enough for most versions.
	subMsg, _ := json.Marshal(map[string]interface{}{
		"subscribe": true,
		"query":     d.query,
	})
	if d.debugLog != nil {
		fmt.Fprintf(d.debugLog, "[%s] sending subscribe: %s\n", time.Now().Format(time.RFC3339), subMsg)
	}
	if err := conn.WriteMessage(websocket.TextMessage, subMsg); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	d.setConnStatus("connected", "")

	// Reset read deadline on every pong so we detect a dead connection
	// if no pong arrives within pongWait after a ping.
	conn.SetReadDeadline(time.Now().Add(pingInterval + pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pingInterval + pongWait))
		return nil
	})

	// Send a ping every pingInterval to keep the connection alive through
	// proxies (HAProxy, nginx, etc.) that drop idle TCP connections.
	go func() {
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for range t.C {
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if d.debugLog != nil {
				fmt.Fprintf(d.debugLog, "[%s] read error: %v\n", time.Now().Format(time.RFC3339), err)
			}
			return err
		}
		conn.SetReadDeadline(time.Now().Add(pingInterval + pongWait))
		if d.debugLog != nil {
			fmt.Fprintf(d.debugLog, "[%s] recv: %s\n", time.Now().Format(time.RFC3339), msg)
		}
		d.parseMsg(msg)
	}
}

func (d *Dashboard) parseMsg(msg []byte) {
	// Riemann may send a JSON array of events or a single event object
	var batch []RiemannEvent
	if json.Unmarshal(msg, &batch) == nil {
		for _, e := range batch {
			d.addEvent(e)
		}
		return
	}
	var e RiemannEvent
	if err := json.Unmarshal(msg, &e); err != nil {
		if d.debugLog != nil {
			fmt.Fprintf(d.debugLog, "[parse error] %v -- msg: %s\n", err, msg)
		}
		return
	}
	if e.Host == "" && e.Service == "" {
		if d.debugLog != nil {
			fmt.Fprintf(d.debugLog, "[parse skip] no host/service: %s\n", msg)
		}
		return
	}
	d.addEvent(e)
}

func main() {
	addr := flag.String("addr", "localhost:5556", "Riemann host:port")
	path := flag.String("path", "/index", "WebSocket endpoint path")
	query := flag.String("query", "true", `Riemann stream query, e.g. 'service = "cpu"'`)
	useTLS := flag.Bool("tls", false, "Use wss:// (TLS)")
	insecure := flag.Bool("insecure", false, "Skip TLS certificate verification (implies --tls)")
	debugFile := flag.String("debug", "", "Write raw WebSocket frames to this file for debugging")
	flag.Parse()

	scheme := "ws"
	if *useTLS || *insecure {
		scheme = "wss"
	}

	d := newDashboard(scheme, *addr, *path, *query, *insecure)

	if *debugFile != "" {
		f, err := os.OpenFile(*debugFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot open debug file: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		d.debugLog = f
	}

	if err := d.run(); err != nil {
		panic(err)
	}
}
