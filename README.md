# riemann-tui

A terminal dashboard for [Riemann](https://riemann.io/) that streams events in real-time over WebSocket.

## Requirements

- Go 1.21+

## Install

```bash
go install github.com/Griaustinis-Media/riemann-tui@latest
```

The binary is placed in `$(go env GOPATH)/bin`. Make sure that directory is on your `$PATH`.

## Usage

```bash
riemann-tui --addr localhost:5556 --path /index --query 'true'
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--addr` | `localhost:5556` | Riemann host:port |
| `--path` | `/events` | WebSocket endpoint path (`/index` or `/events` depending on Riemann version) |
| `--query` | `true` | Riemann stream query |
| `--tls` | `false` | Use `wss://` (TLS) |
| `--insecure` | `false` | Skip TLS certificate verification (implies `--tls`) |
| `--debug` | _(off)_ | Write raw WebSocket frames to a file for debugging |

### Query examples

```bash
# All events
--query 'true'

# Specific service
--query 'service = "cpu"'

# All critical events
--query 'state = "critical"'

# Events from a host
--query 'host = "web-01"'
```

## Layout

```
 Riemann TUI  ● connected  ws://localhost:5556/index  query: true
┌─────────────────────────┬──────────────────────────────────────────┐
│ Services                │ Live Events                               │
│ ● cpu          web-01   │ TIME     HOST    SERVICE  STATE  METRIC   │
│ ● memory       web-01   │ 18:13:53 web-01  cpu      ok     45       │
│ ▶ disk         db-01    │ 18:13:52 db-01   disk     ok     12       │
└─────────────────────────┴──────────────────────────────────────────┘
 Events: 1234   Rate: 8.2/s   Services: 3   Last event: 1s ago
```

The left panel shows the latest state for each service. The right panel streams incoming events newest-first.

## Keyboard shortcuts

| Key | Action |
|-----|--------|
| `↑` / `↓` | Navigate services — filters live events to the selected service |
| `Enter` | Show full event detail for the selected service |
| `Esc` | Clear active filter (press again to quit) |
| `Tab` | Switch focus between panels |
| `q` | Quit |

## Behind a proxy

The dashboard sends WebSocket keepalive pings every 30 seconds to prevent idle connection drops from proxies such as HAProxy, nginx, or AWS load balancers.

For TLS termination at the proxy:

```bash
riemann-tui --addr riemann.example.com:443 --tls
```

For self-signed certificates:

```bash
riemann-tui --addr riemann.example.com:443 --insecure
```

## Debugging

If no events appear, run with `--debug` to inspect the raw WebSocket frames:

```bash
riemann-tui --addr localhost:5556 --path /index --query 'true' --debug /tmp/riemann.log
tail -f /tmp/riemann.log
```
