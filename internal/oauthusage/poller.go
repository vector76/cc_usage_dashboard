package oauthusage

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Snapshot is a completed reading, ready to be recorded.
type Snapshot struct {
	Reading
	// ObservedAt is the time of the request. Unlike the userscript, which
	// must back-date by the page's "Last updated: N minutes ago"
	// indicator, this source reports current figures, so there is no
	// staleness to subtract.
	ObservedAt         time.Time
	ContinuousWithPrev *bool
}

// Sink records a snapshot. It runs on the poll goroutine, so it should not
// block for long.
type Sink func(Snapshot) error

// Status is the source's health, for the tray and the dashboard.
type Status struct {
	// Available is true when the most recent attempt produced a reading.
	Available bool
	// CredentialStale separates "we cannot read right now because the
	// token needs Claude Code to refresh it" from "something is broken".
	// The first is routine on an idle host and self-heals; only the
	// second deserves a user's attention.
	CredentialStale bool
	LastAttempt     time.Time
	LastSuccess     time.Time
	LastError       string

	// RefreshEnabled is true when a RefreshFunc is configured.
	RefreshEnabled bool
	// RefreshArmed is true when the next stale credential will trigger a
	// refresh. An attempt disarms it and only a successful poll re-arms
	// it, so each stale episode gets at most one try.
	RefreshArmed bool
	// LastRefreshAttempt and LastRefreshError describe the most recent
	// refresh. LastRefreshError is empty when that attempt worked.
	LastRefreshAttempt time.Time
	LastRefreshError   string
}

// PollerConfig configures a Poller. Client and Sink are required.
type PollerConfig struct {
	Interval        time.Duration
	CredentialsPath string
	Client          *Client
	Sink            Sink
	// Refresh, when set, is run once when a poll finds the credential
	// stale, and the poll is retried straight after. Nil disables it.
	Refresh RefreshFunc
	// RefreshTimeout bounds one refresh. Zero means defaultRefreshTimeout.
	RefreshTimeout time.Duration
	// Now defaults to time.Now. Injected in tests.
	Now func() time.Time
}

// Poller reads the usage endpoint on a timer and hands each reading to a
// Sink.
//
// It is non-perturbing: the endpoint reports usage rather than consuming
// it, and repeated reads were observed not to open a 5-hour window. That
// is what admits it as a scheduled source where invoking Claude Code to
// read the same figures is not (docs/data-sources.md, "Tier 0").
type Poller struct {
	interval time.Duration
	credPath string
	client   *Client
	sink     Sink
	now      func() time.Time

	refresh        RefreshFunc
	refreshTimeout time.Duration

	stopChan chan struct{}
	doneChan chan struct{}
	stopOnce sync.Once
	started  bool

	mu     sync.Mutex
	prev   *Reading
	prevAt time.Time
	status Status
	// staleLogged keeps a token that has been expired for hours from
	// writing the same warning every interval. Set when the condition is
	// first reported, cleared on recovery.
	staleLogged bool
	// refreshArmed is what stops a refresh that does not help from being
	// run again at every interval: see Status.RefreshArmed.
	refreshArmed bool
}

// NewPoller builds a Poller. Call Start to begin polling.
func NewPoller(cfg PollerConfig) *Poller {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	client := cfg.Client
	if client == nil {
		client = NewClient()
	}
	refreshTimeout := cfg.RefreshTimeout
	if refreshTimeout <= 0 {
		refreshTimeout = defaultRefreshTimeout
	}
	return &Poller{
		interval:       cfg.Interval,
		credPath:       ResolveCredentialsPath(cfg.CredentialsPath),
		client:         client,
		sink:           cfg.Sink,
		now:            now,
		refresh:        cfg.Refresh,
		refreshTimeout: refreshTimeout,
		refreshArmed:   true,
		stopChan:       make(chan struct{}),
		doneChan:       make(chan struct{}),
	}
}

// Start launches the poll loop.
func (p *Poller) Start() {
	p.started = true
	go p.run()
}

// Stop signals the loop and waits for it to exit. Safe to call more than
// once, and safe to call on a Poller that was never started.
func (p *Poller) Stop() {
	p.stopOnce.Do(func() { close(p.stopChan) })
	if p.started {
		<-p.doneChan
	}
}

// Status returns a copy of the current health.
func (p *Poller) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.status
	st.RefreshEnabled = p.refresh != nil
	st.RefreshArmed = p.refresh != nil && p.refreshArmed
	return st
}

func (p *Poller) run() {
	defer close(p.doneChan)

	// Poll immediately rather than withholding the first reading for a
	// whole interval after launch.
	p.tick()

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopChan:
			return
		case <-ticker.C:
			p.tick()
		}
	}
}

// tick runs one poll bounded by the stop signal, so a hung request cannot
// outlive shutdown.
func (p *Poller) tick() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-p.stopChan:
			cancel()
		case <-ctx.Done():
		}
	}()

	if err := p.poll(ctx); err != nil {
		p.logPollError(err)
	}
}

// poll is PollOnce plus the stale-credential refresh: when the credential
// is stale and the refresh is armed, it disarms, runs the refresh once, and
// retries the poll straight away.
//
// Disarming before the attempt is what keeps a refresh that does not help
// -- the command is missing, the user is logged out -- from being run again
// at every interval. Only a successful poll re-arms it (see PollOnce),
// whether this refresh or Claude Code running on its own brought the
// credential back, so the next stale episode gets its own single attempt.
func (p *Poller) poll(ctx context.Context) error {
	err := p.PollOnce(ctx)
	if err == nil || p.refresh == nil || !errors.Is(err, ErrCredentialStale) {
		return err
	}

	p.mu.Lock()
	if !p.refreshArmed {
		p.mu.Unlock()
		return err
	}
	p.refreshArmed = false
	p.status.LastRefreshAttempt = p.now()
	p.mu.Unlock()

	slog.Info("oauth credential stale; running refresh command", "err", err)
	rctx, cancel := context.WithTimeout(ctx, p.refreshTimeout)
	rerr := p.refresh(rctx)
	cancel()
	if rerr != nil {
		p.setRefreshError(rerr.Error())
		slog.Warn("oauth credential refresh failed; not retrying until the credential recovers",
			"err", rerr)
		return err
	}

	err = p.PollOnce(ctx)
	if err == nil {
		p.setRefreshError("")
		slog.Info("oauth credential refreshed")
		return nil
	}
	msg := err.Error()
	if errors.Is(err, ErrCredentialStale) {
		msg = "credential still stale after refresh: " + msg
	}
	p.setRefreshError(msg)
	slog.Warn("oauth credential refresh did not help; not retrying until the credential recovers",
		"err", err)
	return err
}

func (p *Poller) setRefreshError(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status.LastRefreshError = msg
}

// logPollError keeps a routine, self-healing condition from being shouted
// about once per interval while still reporting it the first time.
func (p *Poller) logPollError(err error) {
	if errors.Is(err, ErrCredentialStale) {
		p.mu.Lock()
		alreadyLogged := p.staleLogged
		p.staleLogged = true
		p.mu.Unlock()
		if alreadyLogged {
			slog.Debug("oauth usage still unavailable", "err", err)
			return
		}
		slog.Warn("oauth usage unavailable: credential needs a refresh from Claude Code",
			"err", err, "credentials_path", p.credPath)
		return
	}
	slog.Warn("oauth usage poll failed", "err", err)
}

// PollOnce performs one read and records it.
//
// Exported so a caller can force an immediate refresh -- an integration
// test driving exactly one deterministic poll, or a future "refresh now"
// action -- without waiting for the ticker.
//
// The credential is re-read from disk every time: Claude Code rewrites the
// file when it rotates the token, and a cached copy goes stale within the
// day. When the token is already past its expiry the request is skipped
// entirely rather than sent to be refused.
func (p *Poller) PollOnce(ctx context.Context) error {
	now := p.now()

	p.mu.Lock()
	p.status.LastAttempt = now
	p.mu.Unlock()

	cred, err := LoadCredential(p.credPath)
	if err != nil {
		p.recordFailure(err)
		return err
	}
	if !cred.UsableAt(now) {
		err := errors.New("access token expired at " + cred.ExpiresAt.Format(time.RFC3339) +
			"; it refreshes when Claude Code next runs")
		wrapped := wrapStale(err)
		p.recordFailure(wrapped)
		return wrapped
	}

	reading, err := p.client.Fetch(ctx, cred.AccessToken)
	if err != nil {
		p.recordFailure(err)
		return err
	}

	p.mu.Lock()
	continuous := decideContinuity(p.prev, p.prevAt, reading, now)
	p.mu.Unlock()

	snap := Snapshot{
		Reading:            *reading,
		ObservedAt:         now,
		ContinuousWithPrev: &continuous,
	}

	if err := p.sink(snap); err != nil {
		// A failed write must not advance the continuity anchor, or the
		// next successful snapshot would claim to chain off a reading
		// that was never recorded.
		p.recordFailure(err)
		return err
	}

	p.mu.Lock()
	p.prev = reading
	p.prevAt = now
	p.status.Available = true
	p.status.CredentialStale = false
	p.status.LastSuccess = now
	p.status.LastError = ""
	wasStale := p.staleLogged
	p.staleLogged = false
	p.refreshArmed = true
	p.mu.Unlock()

	if wasStale {
		slog.Info("oauth usage recovered")
	}
	return nil
}

func (p *Poller) recordFailure(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status.Available = false
	p.status.CredentialStale = errors.Is(err, ErrCredentialStale)
	p.status.LastError = err.Error()
}

func wrapStale(err error) error {
	return &staleError{err: err}
}

// staleError attaches ErrCredentialStale to a condition detected locally,
// so callers can treat a pre-flight skip and a server 401 identically.
type staleError struct{ err error }

func (e *staleError) Error() string { return ErrCredentialStale.Error() + ": " + e.err.Error() }
func (e *staleError) Is(target error) bool {
	return target == ErrCredentialStale
}
func (e *staleError) Unwrap() error { return e.err }
