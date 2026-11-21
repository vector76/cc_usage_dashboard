package remoteprices

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/ingest"
)

const newModelYAML = `models:
  claude-new-9:
    input_rate_usd_per_m: 7.00
    output_rate_usd_per_m: 35.00
    cache_creation_rate_usd_per_m: 8.75
    cache_creation_1h_rate_usd_per_m: 14.00
    cache_read_rate_usd_per_m: 0.70
`

// fakeClock is a settable clock shared by an Updater and its test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// priceServer serves body with status and counts requests.
type priceServer struct {
	*httptest.Server
	hits   atomic.Int32
	status atomic.Int32
	body   atomic.Value // string
}

func newPriceServer(t *testing.T, status int, body string) *priceServer {
	t.Helper()
	ps := &priceServer{}
	ps.status.Store(int32(status))
	ps.body.Store(body)
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps.hits.Add(1)
		w.WriteHeader(int(ps.status.Load()))
		w.Write([]byte(ps.body.Load().(string)))
	}))
	t.Cleanup(ps.Close)
	return ps
}

// newTestUpdater builds an Updater against ps, storing its files in dir and
// recording every table it hands to OnUpdate.
func newTestUpdater(t *testing.T, ps *priceServer, dir string, clock *fakeClock) (*Updater, *[]ingest.PriceTable) {
	t.Helper()
	var mu sync.Mutex
	var updates []ingest.PriceTable
	u := New(dir, func(pt ingest.PriceTable) {
		mu.Lock()
		defer mu.Unlock()
		updates = append(updates, pt)
	})
	u.URL = ps.URL
	u.Now = clock.Now
	t.Cleanup(u.Close)
	return u, &updates
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
}

func TestUnknownModelFetchesAndCaches(t *testing.T) {
	ps := newPriceServer(t, http.StatusOK, newModelYAML)
	dir := t.TempDir()
	u, updates := newTestUpdater(t, ps, dir, newClock())

	if !u.NoteUnknownModel("claude-new-9") {
		t.Fatal("NoteUnknownModel did not start a fetch")
	}
	u.Wait()

	if got := ps.hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1", got)
	}
	if len(*updates) != 1 {
		t.Fatalf("OnUpdate calls = %d, want 1", len(*updates))
	}
	if p := (*updates)[0]["claude-new-9"]; p == nil || p.InputRate != 7.00 || p.CacheReadRate != 0.70 {
		t.Errorf("OnUpdate table claude-new-9 = %+v, want the served rates", p)
	}

	// The fetched table survives a restart through the cache file.
	cached := New(dir, nil).Cached()
	if p := cached["claude-new-9"]; p == nil || p.OutputRate != 35.00 {
		t.Errorf("Cached() claude-new-9 = %+v, want the fetched rates", p)
	}
}

func TestAtMostOneAttemptPerInterval(t *testing.T) {
	ps := newPriceServer(t, http.StatusOK, newModelYAML)
	clock := newClock()
	u, _ := newTestUpdater(t, ps, t.TempDir(), clock)

	u.NoteUnknownModel("claude-new-9")
	u.Wait()
	clock.Advance(23 * time.Hour)
	if u.NoteUnknownModel("claude-other-1") {
		t.Error("second fetch started 23h after the first")
	}
	u.Wait()
	if got := ps.hits.Load(); got != 1 {
		t.Errorf("server hits after 23h = %d, want 1", got)
	}

	clock.Advance(2 * time.Hour)
	if !u.NoteUnknownModel("claude-other-1") {
		t.Error("no fetch 25h after the first")
	}
	u.Wait()
	if got := ps.hits.Load(); got != 2 {
		t.Errorf("server hits after 25h = %d, want 2", got)
	}
}

// The daily limit is persisted: a restart within the interval must not fetch
// again, or a crash loop would hit GitHub on every start.
func TestAttemptTimePersistsAcrossRestarts(t *testing.T) {
	ps := newPriceServer(t, http.StatusOK, newModelYAML)
	dir := t.TempDir()
	clock := newClock()

	first, _ := newTestUpdater(t, ps, dir, clock)
	first.NoteUnknownModel("claude-new-9")
	first.Wait()
	first.Close()

	clock.Advance(time.Hour)
	second, _ := newTestUpdater(t, ps, dir, clock)
	if second.NoteUnknownModel("claude-new-9") {
		t.Error("restarted updater fetched again within the interval")
	}
	second.Wait()
	if got := ps.hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1", got)
	}
}

// An attempt time later than now (the clock was set back) must not block
// fetching until the clock catches up.
func TestFutureAttemptTimeDoesNotBlock(t *testing.T) {
	ps := newPriceServer(t, http.StatusOK, newModelYAML)
	dir := t.TempDir()
	clock := newClock()
	first, _ := newTestUpdater(t, ps, dir, clock)
	first.NoteUnknownModel("claude-new-9")
	first.Wait()
	first.Close()

	clock.Advance(-72 * time.Hour)
	second, _ := newTestUpdater(t, ps, dir, clock)
	if !second.NoteUnknownModel("claude-new-9") {
		t.Error("a future attempt time blocked the fetch")
	}
	second.Wait()
}

// A failed fetch still counts as the day's attempt, and leaves the previous
// cache in place.
func TestFailedFetchCountsAndKeepsCache(t *testing.T) {
	ps := newPriceServer(t, http.StatusOK, newModelYAML)
	dir := t.TempDir()
	clock := newClock()
	u, updates := newTestUpdater(t, ps, dir, clock)
	u.NoteUnknownModel("claude-new-9")
	u.Wait()

	ps.status.Store(http.StatusInternalServerError)
	ps.body.Store("oops")
	clock.Advance(25 * time.Hour)
	u.NoteUnknownModel("claude-other-1")
	u.Wait()

	if len(*updates) != 1 {
		t.Errorf("OnUpdate calls = %d, want 1 (the failed fetch must not call it)", len(*updates))
	}
	if New(dir, nil).Cached()["claude-new-9"] == nil {
		t.Error("failed fetch removed the previously cached table")
	}

	clock.Advance(time.Hour)
	if u.NoteUnknownModel("claude-other-1") {
		t.Error("retried within the interval after a failure")
	}
}

func TestRejectsBadResponses(t *testing.T) {
	cases := map[string]string{
		"malformed": "models:\n  x:\n    input_rate_usd_per_m: [unterminated\n",
		"no models": "models: {}\n",
		"html":      "<html>not yaml</html>",
		"oversized": "models:\n" + strings.Repeat("#", maxBodyBytes+1) + "\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			ps := newPriceServer(t, http.StatusOK, body)
			dir := t.TempDir()
			u, updates := newTestUpdater(t, ps, dir, newClock())
			u.NoteUnknownModel("claude-new-9")
			u.Wait()
			if len(*updates) != 0 {
				t.Errorf("OnUpdate called for a %s response", name)
			}
			if _, err := os.Stat(filepath.Join(dir, CacheFileName)); !os.IsNotExist(err) {
				t.Errorf("cache file written for a %s response (stat err %v)", name, err)
			}
		})
	}
}

func TestConcurrentNotesStartOneFetch(t *testing.T) {
	ps := newPriceServer(t, http.StatusOK, newModelYAML)
	u, _ := newTestUpdater(t, ps, t.TempDir(), newClock())

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u.NoteUnknownModel("claude-new-9")
		}()
	}
	wg.Wait()
	u.Wait()
	if got := ps.hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1", got)
	}
}

func TestIgnoresEmptyModelAndClosed(t *testing.T) {
	ps := newPriceServer(t, http.StatusOK, newModelYAML)
	u, _ := newTestUpdater(t, ps, t.TempDir(), newClock())
	if u.NoteUnknownModel("") {
		t.Error("empty model started a fetch")
	}
	u.Close()
	if u.NoteUnknownModel("claude-new-9") {
		t.Error("closed updater started a fetch")
	}
	if got := ps.hits.Load(); got != 0 {
		t.Errorf("server hits = %d, want 0", got)
	}
}

func TestCachedMissingOrMalformed(t *testing.T) {
	dir := t.TempDir()
	if pt := New(dir, nil).Cached(); len(pt) != 0 {
		t.Errorf("Cached() with no file = %v, want empty", pt)
	}
	if err := os.WriteFile(filepath.Join(dir, CacheFileName), []byte("models: [bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if pt := New(dir, nil).Cached(); len(pt) != 0 {
		t.Errorf("Cached() with a malformed file = %v, want empty", pt)
	}
}

func TestDefaultURLIsTheOfficialRepo(t *testing.T) {
	const want = "https://raw.githubusercontent.com/vector76/cc_usage_dashboard/main/prices.yaml"
	if DefaultURL != want {
		t.Errorf("DefaultURL = %q, want %q", DefaultURL, want)
	}
	if u := New(t.TempDir(), nil); u.URL != DefaultURL {
		t.Errorf("New().URL = %q, want DefaultURL", u.URL)
	}
}
