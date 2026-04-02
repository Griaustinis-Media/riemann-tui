package dashboard

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Griaustinis-Media/riemann-tui/pkg/event"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func (d *Dashboard) buildGraphPage() {
	d.graphHeader = tview.NewTextView().SetDynamicColors(true)
	d.graphHeader.SetBackgroundColor(tcell.ColorDarkBlue)

	d.graphBox = tview.NewBox()
	d.graphBox.SetDrawFunc(d.drawGraphCanvas)

	hint := tview.NewTextView().SetDynamicColors(true).
		SetText("  [darkgray]Esc / q: back[-]")
	hint.SetBackgroundColor(tcell.ColorDarkBlue)

	graphPage := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(d.graphHeader, 1, 0, false).
		AddItem(d.graphBox, 0, 1, true).
		AddItem(hint, 1, 0, false)

	d.pages.AddPage("graph", graphPage, true, false)
}

func (d *Dashboard) openGraph() {
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
	d.graphKey = key
	d.updateGraphHeader()
	d.pages.SwitchToPage("graph")
	d.app.SetFocus(d.graphBox)
}

func (d *Dashboard) updateGraphHeader() {
	if d.graphKey == "" {
		return
	}
	parts := strings.SplitN(d.graphKey, "\x00", 2)
	host, svc := "", ""
	if len(parts) == 2 {
		host, svc = parts[0], parts[1]
	}

	d.mu.Lock()
	pts := d.history[d.graphKey]
	var cur, vMin, vMax float64
	n := len(pts)
	if n > 0 {
		cur = pts[n-1].Val
		vMin, vMax = pts[0].Val, pts[0].Val
		for _, p := range pts[1:] {
			if p.Val < vMin {
				vMin = p.Val
			}
			if p.Val > vMax {
				vMax = p.Val
			}
		}
	}
	d.mu.Unlock()

	fmtVal := func(v float64) string {
		if v == float64(int64(v)) && math.Abs(v) < 1e15 {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%.4g", v)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "  [::b]%s[white] :: [::b]%s[-]", host, svc)
	if n > 0 {
		fmt.Fprintf(&sb, "   Current: [yellow]%s[-]   Min: [darkgray]%s[-]   Max: [darkgray]%s[-]   [darkgray](%d pts)[-]",
			fmtVal(cur), fmtVal(vMin), fmtVal(vMax), n)
	} else {
		fmt.Fprintf(&sb, "   [darkgray]no metric data yet[-]")
	}
	d.graphHeader.SetText(sb.String())
}

// drawGraphCanvas is the SetDrawFunc callback for graphBox. It renders a
// real-time braille line graph of the selected service's metric history.
func (d *Dashboard) drawGraphCanvas(screen tcell.Screen, x, y, width, height int) (int, int, int, int) {
	d.mu.Lock()
	pts := append([]event.MetricPoint(nil), d.history[d.graphKey]...)
	d.mu.Unlock()

	bgStyle := tcell.StyleDefault
	for row := y; row < y+height; row++ {
		for col := x; col < x+width; col++ {
			screen.SetContent(col, row, ' ', nil, bgStyle)
		}
	}

	if len(pts) < 2 {
		msg := "Waiting for metric data..."
		midX := x + (width-len(msg))/2
		midY := y + height/2
		grayStyle := bgStyle.Foreground(tcell.ColorGray)
		for i, ch := range msg {
			screen.SetContent(midX+i, midY, ch, nil, grayStyle)
		}
		return x, y, width, height
	}

	// Time range.
	tMin := pts[0].T
	tMax := pts[len(pts)-1].T
	if tMax.Equal(tMin) {
		tMin = tMax.Add(-time.Second)
	}
	tRangeS := tMax.Sub(tMin).Seconds()

	// Value range.
	vMin, vMax := pts[0].Val, pts[0].Val
	for _, p := range pts[1:] {
		if p.Val < vMin {
			vMin = p.Val
		}
		if p.Val > vMax {
			vMax = p.Val
		}
	}
	if vMin == vMax {
		vMin--
		vMax++
	}
	vRange := vMax - vMin

	// Braille canvas: each terminal cell holds a 2×4 dot grid.
	dotW := width * 2
	dotH := height * 4

	// cells[row][col] accumulates braille dot bits (OR'd into 0x2800 at render).
	cells := make([][]uint8, height)
	for i := range cells {
		cells[i] = make([]uint8, width)
	}

	// Bit value for each dot position within a braille cell (row 0–3, col 0–1).
	brailleMap := [4][2]uint8{
		{0x01, 0x08},
		{0x02, 0x10},
		{0x04, 0x20},
		{0x40, 0x80},
	}

	setDot := func(dotX, dotY int) {
		if dotX < 0 || dotX >= dotW || dotY < 0 || dotY >= dotH {
			return
		}
		cells[dotY/4][dotX/2] |= brailleMap[dotY%4][dotX%2]
	}

	toCanvas := func(p event.MetricPoint) (int, int) {
		tx := (p.T.Sub(tMin).Seconds() / tRangeS) * float64(dotW-1)
		ty := (1.0 - (p.Val-vMin)/vRange) * float64(dotH-1)
		return int(math.Round(tx)), int(math.Round(ty))
	}

	absInt := func(n int) int {
		if n < 0 {
			return -n
		}
		return n
	}

	// Bresenham's line algorithm connecting consecutive data points.
	drawLine := func(x0, y0, x1, y1 int) {
		dx := absInt(x1 - x0)
		dy := -absInt(y1 - y0)
		sx, sy := 1, 1
		if x0 > x1 {
			sx = -1
		}
		if y0 > y1 {
			sy = -1
		}
		err := dx + dy
		for {
			setDot(x0, y0)
			if x0 == x1 && y0 == y1 {
				break
			}
			e2 := 2 * err
			if e2 >= dy {
				err += dy
				x0 += sx
			}
			if e2 <= dx {
				err += dx
				y0 += sy
			}
		}
	}

	for i := 1; i < len(pts); i++ {
		cx0, cy0 := toCanvas(pts[i-1])
		cx1, cy1 := toCanvas(pts[i])
		drawLine(cx0, cy0, cx1, cy1)
	}

	// Write braille characters to screen.
	lineStyle := bgStyle.Foreground(tcell.ColorAqua)
	for row := 0; row < height; row++ {
		for col := 0; col < width; col++ {
			if bits := cells[row][col]; bits != 0 {
				screen.SetContent(x+col, y+row, rune(0x2800)|rune(bits), nil, lineStyle)
			}
		}
	}

	return x, y, width, height
}
