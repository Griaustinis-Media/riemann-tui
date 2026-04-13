// Package dashboard implements the Riemann TUI: layout, WebSocket ingestion,
// and all interactive pages (main, detail, graph).
package dashboard

import (
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Griaustinis-Media/riemann-tui/pkg/event"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

const (
	maxStream    = 500
	maxHistory   = 500
	refreshRate  = 200 * time.Millisecond
	pingInterval = 30 * time.Second
	pongWait     = 10 * time.Second
)

// Dashboard holds all UI state and the WebSocket connection logic.
type Dashboard struct {
	app   *tview.Application
	pages *tview.Pages

	header    *tview.TextView
	svcTbl    *tview.Table
	evtPages  *tview.Pages    // switches between evtTbl and evtEmpty
	evtTbl    *tview.Table
	evtEmpty  *tview.TextView // shown when no events yet
	detail    *tview.TextView
	statusBar *tview.TextView

	mu          sync.Mutex
	stream      []event.RiemannEvent          // recent event stream (ring buffer)
	summary     map[string]event.RiemannEvent // latest event per "host\x00service"
	connectedAt time.Time                     // zero if not currently connected
	lastEventAt time.Time                     // zero if no events received yet

	total     int64
	rateCount int64
	rateStart time.Time
	rate      float64

	// history holds timestamped metric samples per "host\x00service" key.
	history map[string][]event.MetricPoint

	// svcFilter is the text search bar below the services table.
	svcFilter *tview.InputField

	// filterKey is "host\x00service" of the selected service, empty = show all.
	// Only accessed from the UI goroutine — no mutex needed.
	filterKey     string
	svcRebuilding bool // suppresses filterKey updates during table rebuild

	// graph page state — only accessed from the UI goroutine.
	graphKey    string
	graphBox    *tview.Box
	graphHeader *tview.TextView

	wsScheme string // "ws" or "wss"
	wsAddr   string
	wsPath   string
	query    string
	insecure bool
	DebugLog *os.File
	conn     string // "connecting" | "connected" | "disconnected"
	connErr  string
}

// New creates, wires, and returns a Dashboard ready to Run.
func New(scheme, addr, path, query string, insecure bool) *Dashboard {
	d := &Dashboard{
		app:       tview.NewApplication(),
		pages:     tview.NewPages(),
		summary:   make(map[string]event.RiemannEvent),
		history:   make(map[string][]event.MetricPoint),
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
