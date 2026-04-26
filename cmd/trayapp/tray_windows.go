//go:build windows

package main

import (
	"context"
	"log/slog"

	"fyne.io/systray"

	"github.com/vector76/cc_usage_dashboard/internal/server"
)

// StartTray runs the Windows systray UI. It blocks until ctx is cancelled
// or the user picks Quit, at which point it tears down the tray and
// returns. The skeleton wires the documented v1 menu (Open dashboard,
// Status, Pause slack signal, About, Quit) — handler bodies are TODOs so
// that the cross-compiled binary builds and exposes the correct menu
// surface while the real behaviors are filled in later.
func StartTray(ctx context.Context, srv *server.Server, paused interface{ Toggle() }) {
	onReady := func() {
		systray.SetTitle("Claude Usage")
		systray.SetTooltip("Claude Usage Dashboard")

		mOpen := systray.AddMenuItem("Open dashboard", "Open the dashboard in the default browser")
		// Status is a placeholder submenu host; populate dynamically once
		// the snapshot loop is wired in a later bead.
		systray.AddMenuItem("Status", "Current burn and slack")
		mPause := systray.AddMenuItemCheckbox("Pause slack signal", "Suppress release recommendations", false)
		systray.AddSeparator()
		mAbout := systray.AddMenuItem("About", "Version and build info")
		mQuit := systray.AddMenuItem("Quit", "Shut down the trayapp")

		go func() {
			for {
				select {
				case <-ctx.Done():
					systray.Quit()
					return
				case <-mOpen.ClickedCh:
					// TODO: open http://localhost:<port> via browser.
					slog.Info("tray: open dashboard clicked")
				case <-mPause.ClickedCh:
					if paused != nil {
						paused.Toggle()
					}
					if mPause.Checked() {
						mPause.Uncheck()
					} else {
						mPause.Check()
					}
					slog.Info("tray: pause toggled", "checked", mPause.Checked())
				case <-mAbout.ClickedCh:
					// TODO: surface a real about dialog; for now log version.
					slog.Info("tray: about", "version", Version)
				case <-mQuit.ClickedCh:
					slog.Info("tray: quit clicked")
					systray.Quit()
					return
				}
			}
		}()
	}

	onExit := func() {
		slog.Info("tray exited")
	}

	systray.Run(onReady, onExit)
}
