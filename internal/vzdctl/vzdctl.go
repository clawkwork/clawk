// Package vzdctl is the control channel between the clawk CLI and a running
// vzd daemon: a tiny HTTP API served over a unix socket in the sandbox's VM
// dir. It exists so network-policy edits apply to the live VM (no down/up
// cycle) and so the CLI can read the daemon's denial ledger.
package vzdctl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/clawkwork/clawk/internal/netfilter"
)

// SocketName is the control socket's filename inside a sandbox's VM dir,
// next to vz.pid and agent.sock.
const SocketName = "control.sock"

// SocketPath returns the control socket path for a sandbox's VM dir.
func SocketPath(vmDir string) string { return filepath.Join(vmDir, SocketName) }

// Handlers are the daemon-side callbacks the server dispatches to. Denials
// and Reload are required; Gate is optional.
type Handlers struct {
	// Denials returns the current denial ledger snapshot.
	Denials func() []netfilter.Denial

	// Reload re-reads the sandbox's network policy from the store and
	// applies it to the live allow list.
	Reload func() error

	// Gate, if non-nil, powers the interactive allow/deny endpoints
	// (/v1/events, /v1/decide, /v1/pending). When nil those endpoints
	// report 404 and the daemon serves only the denial ledger + reload.
	Gate *netfilter.Gate

	// Lifecycle, if non-nil, powers the VM lifecycle endpoints
	// (/v1/lifecycle, /v1/pause, /v1/resume, /v1/suspend). When nil those
	// endpoints report 404, which the client maps to
	// ErrLifecycleUnsupported — the daemon predates lifecycle control.
	Lifecycle *LifecycleHandlers
}

// LifecycleHandlers are the daemon-side callbacks behind the VM lifecycle
// endpoints. State is required when the struct is set; the verbs may be nil
// individually (each nil verb reports 404).
type LifecycleHandlers struct {
	// State reports the VM's live lifecycle snapshot.
	State func() LifecycleState

	// Pause suspends the guest's vCPUs in place (memory stays resident).
	Pause func() error

	// Resume restarts the vCPUs after a Pause.
	Resume func() error

	// Suspend saves memory + device state to disk and stops the VM without
	// resuming it; the daemon exits shortly after. Blocking — the response
	// is written only once the state file is on disk.
	Suspend func() error
}

// LifecycleState is the wire shape of GET /v1/lifecycle.
type LifecycleState struct {
	// State is "booting" (machine not constructed yet), "running", or
	// "paused".
	State string `json:"state"`
	// Restored reports whether this boot restored the guest from a
	// suspend-to-disk state file rather than cold-booting it. Callers use
	// it to skip boot-time hooks whose effects survived inside the guest.
	Restored bool `json:"restored"`
}

// Lifecycle state values for LifecycleState.State.
const (
	LifecycleBooting = "booting"
	LifecycleRunning = "running"
	LifecyclePaused  = "paused"
)

// Server serves the control API on a unix socket until Close.
type Server struct {
	ln  net.Listener
	srv *http.Server
}

// Start removes any stale socket at path, listens, and serves the control
// API in a background goroutine that exits on Close.
func Start(path string, h Handlers) (*Server, error) {
	if h.Denials == nil || h.Reload == nil {
		return nil, errors.New("vzdctl: both Handlers.Denials and Handlers.Reload are required")
	}
	// A previous daemon that died without cleanup leaves the socket file
	// behind; net.Listen refuses to bind over it.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("removing stale control socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on control socket: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/denials", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(denialsResponse{
			Schema:  "1",
			Denials: h.Denials(),
		}); err != nil {
			// Headers are gone; nothing useful left to do but drop the conn.
			return
		}
	})
	mux.HandleFunc("POST /v1/reload", func(w http.ResponseWriter, _ *http.Request) {
		if err := h.Reload(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		serveEvents(w, r, h.Gate)
	})
	mux.HandleFunc("GET /v1/pending", func(w http.ResponseWriter, _ *http.Request) {
		if h.Gate == nil {
			http.Error(w, "interactive gate not enabled", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(pendingResponse{
			Schema:        "1",
			Pending:       h.Gate.Pending(),
			AllowAllUntil: h.Gate.AllowAllUntil(),
		}); err != nil {
			return
		}
	})
	mux.HandleFunc("POST /v1/decide", func(w http.ResponseWriter, r *http.Request) {
		serveDecide(w, r, h.Gate)
	})
	mux.HandleFunc("POST /v1/allow-all", func(w http.ResponseWriter, r *http.Request) {
		serveAllowAll(w, r, h.Gate)
	})
	mux.HandleFunc("GET /v1/lifecycle", func(w http.ResponseWriter, _ *http.Request) {
		if h.Lifecycle == nil || h.Lifecycle.State == nil {
			http.Error(w, "lifecycle control not enabled", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(lifecycleResponse{
			Schema:         "1",
			LifecycleState: h.Lifecycle.State(),
		}); err != nil {
			return
		}
	})
	mux.HandleFunc("POST /v1/pause", func(w http.ResponseWriter, _ *http.Request) {
		serveLifecycleVerb(w, h.Lifecycle, "pause")
	})
	mux.HandleFunc("POST /v1/resume", func(w http.ResponseWriter, _ *http.Request) {
		serveLifecycleVerb(w, h.Lifecycle, "resume")
	})
	mux.HandleFunc("POST /v1/suspend", func(w http.ResponseWriter, _ *http.Request) {
		serveLifecycleVerb(w, h.Lifecycle, "suspend")
	})

	s := &Server{ln: ln, srv: &http.Server{Handler: mux}}
	go func() {
		// ErrServerClosed is the normal Close path; anything else has no
		// receiver here — the daemon notices via failing CLI calls.
		_ = s.srv.Serve(ln)
	}()
	return s, nil
}

// Close stops the server and removes the socket file.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

type denialsResponse struct {
	Schema  string             `json:"schema"`
	Denials []netfilter.Denial `json:"denials"`
}

type lifecycleResponse struct {
	Schema string `json:"schema"`
	LifecycleState
}

// serveLifecycleVerb runs one of the lifecycle callbacks, mapping "handler
// absent" to 404 (the client reads that as ErrLifecycleUnsupported) and a
// callback error to 500 with the error text as the body.
func serveLifecycleVerb(w http.ResponseWriter, l *LifecycleHandlers, verb string) {
	var fn func() error
	if l != nil {
		switch verb {
		case "pause":
			fn = l.Pause
		case "resume":
			fn = l.Resume
		case "suspend":
			fn = l.Suspend
		}
	}
	if fn == nil {
		http.Error(w, "lifecycle control not enabled", http.StatusNotFound)
		return
	}
	if err := fn(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type pendingResponse struct {
	Schema  string              `json:"schema"`
	Pending []netfilter.Pending `json:"pending"`
	// AllowAllUntil is when the time-boxed "allow all" bypass expires, or
	// the zero time when no bypass is active.
	AllowAllUntil time.Time `json:"allow_all_until"`
}

// allowAllRequest is the body of POST /v1/allow-all. A non-positive Seconds
// clears the bypass.
type allowAllRequest struct {
	Seconds int `json:"seconds"`
}

// decideRequest is the body of POST /v1/decide. Action is "allow" or "deny";
// Scope is "once", "session", or "always" (empty means once).
type decideRequest struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Scope  string `json:"scope"`
}

// sseHeartbeat is how often the event stream emits a comment line so a dead
// connection surfaces as a write error instead of lingering forever.
const sseHeartbeat = 25 * time.Second

// serveEvents streams gate events to the client as Server-Sent Events until
// the client disconnects or the subscription is dropped.
func serveEvents(w http.ResponseWriter, r *http.Request, gate *netfilter.Gate) {
	if gate == nil {
		http.Error(w, "interactive gate not enabled", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Subscribe before sending the 200 so the client is registered as a
	// decider by the time its request returns — otherwise a connection it
	// triggers immediately after could race ahead of the subscription and
	// be denied for want of any decider.
	events, cancel := gate.Subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-events:
			if !ok {
				return // gate dropped this subscriber
			}
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// serveDecide resolves one held connection from a POST /v1/decide body.
func serveDecide(w http.ResponseWriter, r *http.Request, gate *netfilter.Gate) {
	if gate == nil {
		http.Error(w, "interactive gate not enabled", http.StatusNotFound)
		return
	}
	var req decideRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decoding decide request: %v", err), http.StatusBadRequest)
		return
	}
	if req.ID == "" {
		http.Error(w, "missing decision id", http.StatusBadRequest)
		return
	}
	decision, err := netfilter.ParseDecision(req.Action, req.Scope)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch err := gate.Resolve(req.ID, decision); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, netfilter.ErrUnknownDecision):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// serveAllowAll opens (or clears) the time-boxed allow-all bypass.
func serveAllowAll(w http.ResponseWriter, r *http.Request, gate *netfilter.Gate) {
	if gate == nil {
		http.Error(w, "interactive gate not enabled", http.StatusNotFound)
		return
	}
	var req allowAllRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decoding allow-all request: %v", err), http.StatusBadRequest)
		return
	}
	gate.AllowAll(time.Duration(req.Seconds) * time.Second)
	w.WriteHeader(http.StatusNoContent)
}

// Client talks to a vzd control socket. The zero value is not usable; build
// one with NewClient.
type Client struct {
	path  string
	httpc *http.Client
}

// NewClient returns a client for the control socket at path. It does not
// dial; connection errors surface on the first call.
func NewClient(path string) *Client {
	return &Client{
		path: path,
		httpc: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", path)
				},
			},
		},
	}
}

// ErrNotRunning reports that no daemon is listening — the sandbox is down
// (or predates the control socket). Callers check with errors.Is.
var ErrNotRunning = errors.New("control socket not available (sandbox not running?)")

// ErrLifecycleUnsupported reports that the daemon answered but has no
// lifecycle endpoints — it predates lifecycle control. Callers check with
// errors.Is and suggest a sandbox restart.
var ErrLifecycleUnsupported = errors.New("daemon does not support lifecycle control (restart the sandbox to upgrade its daemon)")

// Lifecycle fetches the VM's live lifecycle snapshot.
func (c *Client) Lifecycle(ctx context.Context) (LifecycleState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://vzd/v1/lifecycle", nil)
	if err != nil {
		return LifecycleState{}, fmt.Errorf("building lifecycle request: %w", err)
	}
	resp, err := c.do(req)
	if err != nil {
		return LifecycleState{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return LifecycleState{}, fmt.Errorf("%w: %s", ErrLifecycleUnsupported, responseError(resp))
	default:
		return LifecycleState{}, fmt.Errorf("lifecycle: %s", responseError(resp))
	}
	var out lifecycleResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return LifecycleState{}, fmt.Errorf("decoding lifecycle response: %w", err)
	}
	return out.LifecycleState, nil
}

// Pause asks the daemon to suspend the guest's vCPUs in place.
func (c *Client) Pause(ctx context.Context) error { return c.lifecycleVerb(ctx, "pause") }

// Resume asks the daemon to restart the vCPUs after a Pause.
func (c *Client) Resume(ctx context.Context) error { return c.lifecycleVerb(ctx, "resume") }

// Suspend asks the daemon to save the VM's memory + device state to disk
// and stop without resuming; the daemon exits shortly after the call
// returns. Blocking — pass a ctx generous enough to write a multi-GiB
// memory image.
func (c *Client) Suspend(ctx context.Context) error { return c.lifecycleVerb(ctx, "suspend") }

func (c *Client) lifecycleVerb(ctx context.Context, verb string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://vzd/v1/"+verb, nil)
	if err != nil {
		return fmt.Errorf("building %s request: %w", verb, err)
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrLifecycleUnsupported, responseError(resp))
	default:
		return fmt.Errorf("%s: %s", verb, responseError(resp))
	}
}

// Denials fetches the daemon's denial ledger, most recent first.
func (c *Client) Denials(ctx context.Context) ([]netfilter.Denial, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://vzd/v1/denials", nil)
	if err != nil {
		return nil, fmt.Errorf("building denials request: %w", err)
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("denials: %s", responseError(resp))
	}
	var out denialsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding denials response: %w", err)
	}
	return out.Denials, nil
}

// Reload asks the daemon to re-read the sandbox's network policy from the
// store and apply it to the live allow list.
func (c *Client) Reload(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://vzd/v1/reload", nil)
	if err != nil {
		return fmt.Errorf("building reload request: %w", err)
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("reload: %s", responseError(resp))
	}
	return nil
}

// Pending fetches the daemon's outstanding interactive holds.
func (c *Client) Pending(ctx context.Context) ([]netfilter.Pending, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://vzd/v1/pending", nil)
	if err != nil {
		return nil, fmt.Errorf("building pending request: %w", err)
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pending: %s", responseError(resp))
	}
	var out pendingResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding pending response: %w", err)
	}
	return out.Pending, nil
}

// Decide resolves a held connection by id. action is "allow" or "deny"; scope
// is "once", "session", or "always". It returns ErrUnknownDecision (wrapped)
// when the hold is no longer outstanding.
func (c *Client) Decide(ctx context.Context, id, action, scope string) error {
	body, err := json.Marshal(decideRequest{ID: id, Action: action, Scope: scope})
	if err != nil {
		return fmt.Errorf("encoding decide request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://vzd/v1/decide", strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("building decide request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", netfilter.ErrUnknownDecision, responseError(resp))
	default:
		return fmt.Errorf("decide: %s", responseError(resp))
	}
}

// AllowAll opens the time-boxed allow-all bypass for d (every destination
// passes until it elapses) and releases any currently held connections. A
// non-positive d clears the bypass.
func (c *Client) AllowAll(ctx context.Context, d time.Duration) error {
	body, err := json.Marshal(allowAllRequest{Seconds: int(d.Seconds())})
	if err != nil {
		return fmt.Errorf("encoding allow-all request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://vzd/v1/allow-all", strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("building allow-all request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("allow-all: %s", responseError(resp))
	}
	return nil
}

// Events streams gate events (pending/resolved) over the control socket. The
// returned channel is closed when ctx is cancelled or the stream ends; the
// reader goroutine's lifetime is bound to ctx. Connection errors surface from
// Events itself, not the channel.
func (c *Client) Events(ctx context.Context) (<-chan netfilter.Event, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://vzd/v1/events", nil)
	if err != nil {
		return nil, fmt.Errorf("building events request: %w", err)
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, fmt.Errorf("events: %s", responseError(resp))
	}

	out := make(chan netfilter.Event)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue // heartbeat comment or blank separator line
			}
			var ev netfilter.Event
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				continue
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// do sends the request, mapping a missing/refused socket to ErrNotRunning so
// callers can tell "sandbox is down" apart from real failures.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.httpc.Do(req)
	if err == nil {
		return resp, nil
	}
	if _, statErr := os.Stat(c.path); errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("%w (no socket at %s)", ErrNotRunning, c.path)
	}
	var sysErr *net.OpError
	if errors.As(err, &sysErr) {
		return nil, fmt.Errorf("%w (dial %s: %v)", ErrNotRunning, c.path, sysErr.Err)
	}
	return nil, fmt.Errorf("control socket request: %w", err)
}

// responseError extracts the error text the server wrote with http.Error,
// falling back to the status line.
func responseError(resp *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil || len(body) == 0 {
		return resp.Status
	}
	return fmt.Sprintf("%s: %s", resp.Status, string(body))
}
