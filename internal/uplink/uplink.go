// Package uplink forwards this machine's usage events to another trayapp.
//
// The motivating case is a VM running Claude Code against the same Anthropic
// account as the host. Anthropic bills both against one quota, so the host's
// scraped percentages already include the VM's spend while its event stream
// does not — the host under-counts tokens relative to its own snapshots.
// Forwarding closes that gap.
//
// Only events move. Quota snapshots stay put: a VM has no browser on
// claude.ai, and percent-of-quota is the receiver's business. The receiver's
// windows engine derives from the combined event stream as if the events had
// been local, which is correct precisely because the quota is shared.
//
// Delivery is at-least-once. A durable per-peer cursor over usage_events.id
// survives restarts and outages, and re-delivery is harmless: the receiver's
// UNIQUE(session_id, message_id) turns a repeat into a no-op.
package uplink

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

const (
	// defaultInterval is how often the backlog is drained. Usage data is
	// reviewed over hours, so sub-minute latency buys nothing and a quiet
	// VM should not wake the host's HTTP server constantly.
	defaultInterval = 30 * time.Second

	// defaultBatch caps one tick's work so a large backfill drains in
	// bounded slices instead of one long burst holding the loop open
	// against a Stop.
	defaultBatch = 200

	// requestTimeout keeps a hung peer from stalling the loop. The peer is
	// on a local adapter, so this is generous.
	requestTimeout = 10 * time.Second
)

// errPermanent marks a rejection that retrying cannot fix.
var errPermanent = errors.New("permanently rejected by peer")

// Forwarder drains usage_events to a peer trayapp's POST /log.
type Forwarder struct {
	baseURL  string
	store    *store.Store
	client   *http.Client
	interval time.Duration
	batch    int

	stopChan chan struct{}
	doneChan chan struct{}
}

// New creates a Forwarder targeting a peer's base URL (scheme://host[:port],
// no path — config.Load has already validated and normalized it).
func New(baseURL string, s *store.Store) *Forwarder {
	return &Forwarder{
		baseURL:  baseURL,
		store:    s,
		client:   &http.Client{Timeout: requestTimeout},
		interval: defaultInterval,
		batch:    defaultBatch,
		stopChan: make(chan struct{}),
		doneChan: make(chan struct{}),
	}
}

// Start begins draining in a goroutine.
func (f *Forwarder) Start() {
	slog.Info("uplink forwarding enabled", "peer", f.baseURL, "interval", f.interval)
	go f.run()
}

// Stop halts the loop and waits for the in-flight tick to finish.
func (f *Forwarder) Stop() {
	close(f.stopChan)
	<-f.doneChan
}

func (f *Forwarder) run() {
	defer close(f.doneChan)

	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()

	for {
		// Drain first so a restart does not sit on its backlog for a
		// whole interval before catching up.
		f.tick()

		select {
		case <-f.stopChan:
			return
		case <-ticker.C:
		}
	}
}

// tick runs one drain, logging rather than propagating failures. A peer that
// is asleep is the expected steady state on a laptop host, so a failed tick
// is a warning, not an incident — the cursor holds and the next tick retries.
func (f *Forwarder) tick() {
	n, err := f.forwardOnce()
	if err != nil {
		slog.Warn("uplink forward incomplete", "err", err, "peer", f.baseURL, "delivered", n)
		return
	}
	if n > 0 {
		slog.Debug("uplink forwarded events", "count", n, "peer", f.baseURL)
	}
}

// forwardOnce delivers up to one batch and returns how many events the peer
// accepted.
//
// The cursor advances only over events that are settled — delivered, or
// permanently rejected. A transient failure stops the batch with the cursor
// at the last settled event, so the next tick resumes exactly there.
func (f *Forwarder) forwardOnce() (int, error) {
	cursor, err := f.store.GetUplinkCursor(f.baseURL)
	if err != nil {
		return 0, err
	}

	events, err := f.store.ForwardableEventsAfter(cursor, f.batch)
	if err != nil {
		return 0, err
	}

	settled := cursor
	delivered := 0
	var stopErr error

	for _, e := range events {
		err := f.post(e)
		switch {
		case err == nil:
			delivered++
			settled = e.ID

		case errors.Is(err, errPermanent):
			// Retrying can never succeed — the receiver's occurred_at
			// filter answers 400 for a skewed clock, for instance. Holding
			// the cursor here would wedge every later event behind this
			// one forever, so step over it and say so loudly.
			slog.Warn("uplink: peer rejected event, skipping it",
				"err", err, "peer", f.baseURL,
				"session_id", e.SessionID, "message_id", e.MessageID,
				"occurred_at", e.OccurredAt)
			settled = e.ID

		default:
			stopErr = err
		}
		if stopErr != nil {
			break
		}
	}

	if settled > cursor {
		if err := f.store.SetUplinkCursor(f.baseURL, settled); err != nil {
			// The events did land; only the bookmark failed. Report it,
			// and accept that the next tick re-sends them — the receiver
			// dedupes, so the cost is bandwidth, not correctness.
			return delivered, fmt.Errorf("failed to persist uplink cursor: %w", err)
		}
	}

	return delivered, stopErr
}

// logPayload mirrors the receiver's LogPostRequest. It is declared here
// rather than imported so the sender is not coupled to the server package's
// internals; the wire contract is the JSON, which /log's own tests pin.
type logPayload struct {
	OccurredAt            time.Time `json:"occurred_at"`
	InputTokens           int       `json:"input_tokens"`
	OutputTokens          int       `json:"output_tokens"`
	CacheCreationTokens   int       `json:"cache_creation_tokens,omitempty"`
	CacheCreation1hTokens int       `json:"cache_creation_1h_tokens,omitempty"`
	CacheReadTokens       int       `json:"cache_read_tokens,omitempty"`
	SessionID             string    `json:"session_id"`
	MessageID             string    `json:"message_id"`
	Model                 string    `json:"model,omitempty"`
	ProjectPath           string    `json:"project_path,omitempty"`
	Source                string    `json:"source"`
}

// post delivers one event. The returned error wraps errPermanent when the
// peer's answer means "never", and is bare when it means "later".
func (f *Forwarder) post(e store.ForwardableEvent) error {
	body, err := json.Marshal(logPayload{
		OccurredAt:            e.OccurredAt,
		InputTokens:           e.InputTokens,
		OutputTokens:          e.OutputTokens,
		CacheCreationTokens:   e.CacheCreationTokens,
		CacheCreation1hTokens: e.CacheCreation1hTokens,
		CacheReadTokens:       e.CacheReadTokens,
		SessionID:             e.SessionID,
		MessageID:             e.MessageID,
		Model:                 e.Model,
		ProjectPath:           e.ProjectPath,
		// Fills the receiver's existing provenance column so its
		// events_ingested metric separates forwarded traffic from local.
		Source: "uplink",
	})
	if err != nil {
		return fmt.Errorf("%w: marshal event %d: %v", errPermanent, e.ID, err)
	}

	req, err := http.NewRequest(http.MethodPost, f.baseURL+"/log", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: build request: %v", errPermanent, err)
	}
	// The receiver rejects any POST without this — it is the CSRF guard
	// that stops a browser mounting a simple cross-origin form post.
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("post to %s: %w", f.baseURL, err)
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused for the next event in the batch.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// Includes the {"duplicate":true} answer, which is how an
		// interrupted batch reconverges.
		return nil

	case resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode == http.StatusTooManyRequests:
		// 4xx by number, "later" by meaning.
		return fmt.Errorf("peer answered %d", resp.StatusCode)

	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return fmt.Errorf("%w: peer answered %d", errPermanent, resp.StatusCode)

	default:
		return fmt.Errorf("peer answered %d", resp.StatusCode)
	}
}
