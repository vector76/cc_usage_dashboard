// Package remoteprices keeps a local copy of the price table published in the
// project's GitHub repository, so a model released after this binary was
// built can still be priced from its real rates instead of the ceiling.
//
// The copy is fetched only when an unknown model turns up, and at most once
// per Interval (a day by default), with the attempt time persisted so a
// restart does not fetch again. A fetched table is the lowest-priority layer:
// a custom prices.yaml and the built-in table both win over it per model, so
// it only adds models the build does not know. See docs/configuration.md
// "Price table resolution".
package remoteprices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/ingest"
)

const (
	// DefaultURL is prices.yaml on the main branch of the official
	// repository. It is built in deliberately: the table is only trusted
	// because it comes from the project's own source.
	DefaultURL = "https://raw.githubusercontent.com/vector76/cc_usage_dashboard/main/prices.yaml"

	// CacheFileName holds the last table fetched successfully, and
	// StateFileName the time of the last attempt, both in the user data dir.
	CacheFileName = "prices_remote.yaml"
	StateFileName = "prices_remote_state.json"

	// DefaultInterval is the minimum time between two fetch attempts.
	DefaultInterval = 24 * time.Hour

	// maxBodyBytes caps the download; the real table is a few kilobytes.
	maxBodyBytes = 1 << 20

	fetchTimeout = 30 * time.Second
	userAgent    = "cc-usage-dashboard-trayapp"
)

// Updater fetches the published price table on demand. The exported fields
// may be changed after New and before the first NoteUnknownModel.
type Updater struct {
	// URL is where the table is fetched from; DefaultURL unless a test
	// points it elsewhere.
	URL string
	// Interval is the minimum time between fetch attempts, failed ones
	// included.
	Interval time.Duration
	// Client performs the request.
	Client *http.Client
	// Now is the clock the interval is measured with.
	Now func() time.Time
	// OnUpdate receives each table fetched and parsed successfully. It runs
	// on the fetch goroutine.
	OnUpdate func(ingest.PriceTable)

	dir string

	mu          sync.Mutex
	stateLoaded bool
	lastAttempt time.Time
	running     bool
	closed      bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New returns an Updater that keeps its files in dir (the user data dir).
func New(dir string, onUpdate func(ingest.PriceTable)) *Updater {
	ctx, cancel := context.WithCancel(context.Background())
	return &Updater{
		URL:      DefaultURL,
		Interval: DefaultInterval,
		Client:   &http.Client{Timeout: fetchTimeout},
		Now:      time.Now,
		OnUpdate: onUpdate,
		dir:      dir,
		ctx:      ctx,
		cancel:   cancel,
	}
}

// Cached returns the last table fetched, or an empty table when there is
// none or the file cannot be parsed. A bad cache is not fatal: the next
// fetch replaces it.
func (u *Updater) Cached() ingest.PriceTable {
	path := filepath.Join(u.dir, CacheFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("could not read the cached GitHub price list", "path", path, "err", err)
		}
		return make(ingest.PriceTable)
	}
	pt, err := ingest.ParsePriceTable(data, path)
	if err != nil {
		slog.Warn("ignoring the cached GitHub price list", "err", err)
		return make(ingest.PriceTable)
	}
	return pt
}

// NoteUnknownModel reports that model could not be priced. It starts a
// background fetch when none ran within Interval and none is running, and
// reports whether it did. It is cheap to call once per event.
func (u *Updater) NoteUnknownModel(model string) bool {
	if model == "" {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.running {
		return false
	}
	if !u.stateLoaded {
		u.lastAttempt = u.readLastAttempt()
		u.stateLoaded = true
	}
	// An attempt time in the future (the clock was set back) does not block:
	// otherwise it could hold off fetching for as long as the clock was off.
	now := u.Now()
	if since := now.Sub(u.lastAttempt); !u.lastAttempt.IsZero() && since >= 0 && since < u.Interval {
		return false
	}
	// Record the attempt before making it, so a failure, a hang or a crash
	// mid-request still uses up the day's attempt.
	u.lastAttempt = now
	u.writeLastAttempt(now)
	u.running = true
	u.wg.Add(1)
	go u.run(model)
	return true
}

// Wait blocks until a fetch in progress has finished.
func (u *Updater) Wait() {
	u.wg.Wait()
}

// Close cancels a fetch in progress, waits for it, and stops new ones. Call
// it before closing the database OnUpdate writes to.
func (u *Updater) Close() {
	u.mu.Lock()
	u.closed = true
	u.mu.Unlock()
	u.cancel()
	u.wg.Wait()
}

func (u *Updater) run(model string) {
	defer u.wg.Done()
	defer func() {
		u.mu.Lock()
		u.running = false
		u.mu.Unlock()
	}()

	pt, err := u.fetch()
	if err != nil {
		if u.ctx.Err() == nil {
			slog.Warn("could not fetch the price list from GitHub; will try again in a day",
				"model", model, "url", u.URL, "err", err)
		}
		return
	}
	if ingest.HasPrice(pt, model) {
		slog.Info("fetched the price list from GitHub", "models", len(pt), "for", model)
	} else {
		slog.Warn("the price list on GitHub does not list this model yet; it stays priced at the ceiling",
			"model", model, "models", len(pt))
	}
	if u.OnUpdate != nil {
		u.OnUpdate(pt)
	}
}

// fetch downloads, validates and caches the table.
func (u *Updater) fetch() (ingest.PriceTable, error) {
	ctx, cancel := context.WithTimeout(u.ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := u.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBodyBytes {
		return nil, fmt.Errorf("response is over %d bytes", maxBodyBytes)
	}
	pt, err := ingest.ParsePriceTable(data, u.URL)
	if err != nil {
		return nil, err
	}
	if len(pt) == 0 {
		return nil, errors.New("response lists no models")
	}
	if err := u.writeCache(data); err != nil {
		// The table is still good for this run; only the restart copy is lost.
		slog.Warn("could not save the GitHub price list", "err", err)
	}
	return pt, nil
}

// writeCache replaces the cache file through a rename, so a crash mid-write
// cannot leave a truncated table behind.
func (u *Updater) writeCache(data []byte) error {
	if err := os.MkdirAll(u.dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(u.dir, CacheFileName+".*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return werr
	}
	if err := os.Rename(tmp.Name(), filepath.Join(u.dir, CacheFileName)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

type state struct {
	LastAttempt time.Time `json:"last_attempt"`
}

// readLastAttempt returns the persisted attempt time, or zero when there is
// none or it cannot be read (which allows one attempt now).
func (u *Updater) readLastAttempt() time.Time {
	data, err := os.ReadFile(filepath.Join(u.dir, StateFileName))
	if err != nil {
		return time.Time{}
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		return time.Time{}
	}
	return s.LastAttempt
}

// writeLastAttempt persists t. A failure is logged and otherwise ignored:
// the in-memory time still limits this process to one attempt per interval.
func (u *Updater) writeLastAttempt(t time.Time) {
	data, _ := json.Marshal(state{LastAttempt: t.UTC()})
	err := os.MkdirAll(u.dir, 0o755)
	if err == nil {
		err = os.WriteFile(filepath.Join(u.dir, StateFileName), data, 0o644)
	}
	if err != nil {
		slog.Warn("could not save the GitHub price list attempt time", "err", err)
	}
}
