package dashboard

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Griaustinis-Media/riemann-tui/pkg/event"
	"github.com/gorilla/websocket"
)

// Run starts the WebSocket ingestion loop and the UI refresh ticker,
// then blocks until the application exits.
func (d *Dashboard) Run() error {
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
	if d.DebugLog != nil {
		fmt.Fprintf(d.DebugLog, "[%s] dialing %s\n", time.Now().Format(time.RFC3339), u.String())
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
	if d.DebugLog != nil {
		fmt.Fprintf(d.DebugLog, "[%s] sending subscribe: %s\n", time.Now().Format(time.RFC3339), subMsg)
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
			if d.DebugLog != nil {
				fmt.Fprintf(d.DebugLog, "[%s] read error: %v\n", time.Now().Format(time.RFC3339), err)
			}
			return err
		}
		conn.SetReadDeadline(time.Now().Add(pingInterval + pongWait))
		if d.DebugLog != nil {
			fmt.Fprintf(d.DebugLog, "[%s] recv: %s\n", time.Now().Format(time.RFC3339), msg)
		}
		d.parseMsg(msg)
	}
}

func (d *Dashboard) parseMsg(msg []byte) {
	// Riemann cheshire WebSocket transport wraps events in {"ok":true,"events":[...]}
	var wrapper struct {
		OK     bool                 `json:"ok"`
		Events []event.RiemannEvent `json:"events"`
	}
	if json.Unmarshal(msg, &wrapper) == nil && len(wrapper.Events) > 0 {
		for _, e := range wrapper.Events {
			if e.Host == "" && e.Service == "" {
				continue
			}
			d.addEvent(e)
		}
		return
	}

	// Fallback: bare JSON array of events
	var batch []event.RiemannEvent
	if json.Unmarshal(msg, &batch) == nil && len(batch) > 0 {
		for _, e := range batch {
			d.addEvent(e)
		}
		return
	}

	// Fallback: single event object
	var e event.RiemannEvent
	if err := json.Unmarshal(msg, &e); err != nil {
		if d.DebugLog != nil {
			fmt.Fprintf(d.DebugLog, "[parse error] %v -- msg: %s\n", err, msg)
		}
		return
	}
	if e.Host == "" && e.Service == "" {
		if d.DebugLog != nil {
			fmt.Fprintf(d.DebugLog, "[parse skip] no host/service: %s\n", msg)
		}
		return
	}
	d.addEvent(e)
}

func (d *Dashboard) addEvent(e event.RiemannEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.total++
	d.rateCount++
	d.lastEventAt = time.Now()
	d.stream = append(d.stream, e)
	if len(d.stream) > maxStream {
		d.stream = d.stream[1:]
	}
	key := e.Host + "\x00" + e.Service
	d.summary[key] = e
	if val, ok := e.MetricFloat(); ok {
		d.history[key] = append(d.history[key], event.MetricPoint{T: time.Now(), Val: val})
		if len(d.history[key]) > maxHistory {
			d.history[key] = d.history[key][len(d.history[key])-maxHistory:]
		}
	}
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
