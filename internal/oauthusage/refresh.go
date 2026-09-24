package oauthusage

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// RefreshFunc tries to get a stale credential rewritten on disk. It returns
// once the attempt has finished; whether it worked is judged by re-reading
// the credential, not by the error, which only reports that the attempt
// itself failed.
type RefreshFunc func(ctx context.Context) error

// ClaudeRefreshArgs are the arguments NewClaudeRefresher passes to Claude
// Code. Launching it refreshes the OAuth token as a side effect, and
// /usage keeps the run to reading the account's usage.
var ClaudeRefreshArgs = []string{"-p", "/usage"}

// defaultRefreshTimeout bounds one refresh attempt. Claude Code's startup
// takes seconds; a run still going after two minutes is hung, and the poll
// loop must not wait on it.
const defaultRefreshTimeout = 2 * time.Minute

// refreshOutputLimit caps how much of a failing command's output is kept in
// its error. The tail is kept, since that is where the reason usually is.
const refreshOutputLimit = 500

// NewClaudeRefresher returns a RefreshFunc that runs `claude -p /usage`
// using the executable at path, with dir as the working directory.
//
// The working directory matters because Claude Code files a transcript
// under the project named after it. A fixed, app-owned directory keeps
// these runs out of the user's real projects.
func NewClaudeRefresher(path, dir string) RefreshFunc {
	return newCommandRefresher(path, ClaudeRefreshArgs, dir)
}

func newCommandRefresher(name string, args []string, dir string) RefreshFunc {
	return func(ctx context.Context) error {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		// If Claude Code leaves a child holding the output pipes after it
		// is killed on timeout, stop waiting for them rather than hang.
		cmd.WaitDelay = 5 * time.Second
		hideWindow(cmd)

		if err := cmd.Run(); err != nil {
			if tail := outputTail(out.String()); tail != "" {
				return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, tail)
			}
			return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return nil
	}
}

func outputTail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > refreshOutputLimit {
		s = "..." + s[len(s)-refreshOutputLimit:]
	}
	return s
}
