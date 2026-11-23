package oauthusage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// planFile is the one field ReadPlan decodes. The access token is
// deliberately not a member, so the plan can be read without the token ever
// being parsed into memory.
type planFile struct {
	ClaudeAIOAuth struct {
		SubscriptionType string `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

// ReadPlan returns the subscription type Claude Code recorded at login
// ("pro", "max", "team", "enterprise"), or "" when the file is missing or
// states none.
//
// It needs no token and makes no network call: the field is plain data in the
// credentials file, present whether or not the access token has expired. That
// is what lets enterprise mode work with polling switched off. A missing file
// is not an error, since the plan simply is not known. Errors never quote the
// file's contents.
func ReadPlan(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("reading credentials: %w", err)
	}
	var f planFile
	if err := json.Unmarshal(data, &f); err != nil {
		// Not %w: the decode error can quote the offending value.
		return "", fmt.Errorf("credentials file at %s is not valid JSON", path)
	}
	return f.ClaudeAIOAuth.SubscriptionType, nil
}

// PlanDetector answers "is this an enterprise account?" from the credentials
// file, re-reading it only when its size or modification time changes, so it
// is cheap enough to call on every dashboard refresh.
type PlanDetector struct {
	path string

	mu         sync.Mutex
	size       int64
	modTime    time.Time
	checked    bool
	enterprise bool
}

// NewPlanDetector returns a detector for the credentials file at path.
func NewPlanDetector(path string) *PlanDetector {
	return &PlanDetector{path: path}
}

// Enterprise reports whether the plan is "enterprise".
//
// A file that cannot be read or decoded keeps the previous answer rather than
// flipping it: Claude Code rewrites this file when it rotates the token, and a
// poll landing mid-write must not bounce the dashboard between modes. A file
// that is gone for good (logged out) reads as not enterprise.
func (d *PlanDetector) Enterprise() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	fi, err := os.Stat(d.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			d.checked, d.enterprise = true, false
		}
		return d.enterprise
	}
	if d.checked && fi.Size() == d.size && fi.ModTime().Equal(d.modTime) {
		return d.enterprise
	}
	plan, err := ReadPlan(d.path)
	if err != nil {
		return d.enterprise
	}
	d.size, d.modTime, d.checked = fi.Size(), fi.ModTime(), true
	d.enterprise = strings.EqualFold(strings.TrimSpace(plan), "enterprise")
	return d.enterprise
}
