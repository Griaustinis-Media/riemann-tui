// Package event contains the Riemann wire types and their helper methods.
package event

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AttrMap decodes Riemann attributes from either a JSON object
// {"k":"v"} or the cheshire array format [{"key":"k","value":"v"}].
type AttrMap map[string]string

func (a *AttrMap) UnmarshalJSON(b []byte) error {
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
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*a = m
	return nil
}

// flexInt64 unmarshals from either a JSON number or a JSON-string containing digits.
// Riemann's cheshire transport sometimes encodes time_micros as a quoted string.
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		*f = flexInt64(n)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexInt64(n)
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
	TimeMicros  flexInt64   `json:"time_micros"`
	// Metric variants: Riemann protobuf has metric_sint64, metric_d, metric_f.
	// Some transports unify them under "metric".
	Metric       interface{} `json:"metric"`
	MetricSint64 *int64      `json:"metric_sint64"`
	MetricD      *float64    `json:"metric_d"`
	MetricF      *float32    `json:"metric_f"`
	Attributes   AttrMap     `json:"attributes"`
}

func (e RiemannEvent) MetricStr() string {
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

func (e RiemannEvent) MetricFloat() (float64, bool) {
	var v float64
	var ok bool
	if e.Metric != nil {
		if f, is := e.Metric.(float64); is {
			v, ok = f, true
		}
	} else if e.MetricD != nil {
		v, ok = *e.MetricD, true
	} else if e.MetricF != nil {
		v, ok = float64(*e.MetricF), true
	} else if e.MetricSint64 != nil {
		v, ok = float64(*e.MetricSint64), true
	}
	if ok && (math.IsNaN(v) || math.IsInf(v, 0)) {
		return 0, false
	}
	return v, ok
}

func (e RiemannEvent) TimeStr() string {
	if e.TimeMicros != 0 {
		return time.UnixMicro(int64(e.TimeMicros)).Local().Format("15:04:05")
	}
	switch v := e.Time.(type) {
	case string:
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t.Local().Format("15:04:05")
		}
	case float64:
		return time.Unix(int64(v), 0).Format("15:04:05")
	}
	return time.Now().Format("15:04:05")
}

// EventTime returns the event timestamp, falling back to zero.
func (e RiemannEvent) EventTime() time.Time {
	if e.TimeMicros != 0 {
		return time.UnixMicro(int64(e.TimeMicros))
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

// ExpiresAt returns the time after which this event should be evicted from the
// summary, or zero if the event has no TTL.
func (e RiemannEvent) ExpiresAt() time.Time {
	if e.TTL <= 0 {
		return time.Time{}
	}
	t := e.EventTime()
	if t.IsZero() {
		return time.Time{}
	}
	return t.Add(time.Duration(e.TTL * float64(time.Second)))
}

func (e RiemannEvent) TagsStr() string {
	return strings.Join(e.Tags, " ")
}

func (e RiemannEvent) AttrsStr() string {
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

// MetricPoint is a single timestamped metric sample stored for graph rendering.
type MetricPoint struct {
	T   time.Time
	Val float64
}
