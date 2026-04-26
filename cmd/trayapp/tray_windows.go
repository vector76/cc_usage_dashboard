//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image/png"
	"log/slog"
	"os/exec"

	"fyne.io/systray"

	"github.com/vector76/cc_usage_dashboard/internal/icon"
	"github.com/vector76/cc_usage_dashboard/internal/server"
)

// StartTray runs the Windows systray UI. It blocks until ctx is cancelled
// or the user picks Quit, at which point it tears down the tray and
// returns. dashboardURL is the http://host:port the "Open dashboard" item
// should launch in the user's default browser.
func StartTray(ctx context.Context, srv *server.Server, paused interface{ Toggle() }, dashboardURL string) {
	onReady := func() {
		ico, err := buildTrayIcon(icon.PNG)
		if err != nil {
			slog.Warn("tray: icon build failed; tray will appear without an icon", "err", err)
		} else {
			systray.SetIcon(ico)
		}
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
					if err := openURL(dashboardURL); err != nil {
						slog.Error("tray: open dashboard failed", "url", dashboardURL, "err", err)
					}
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

// openURL launches the user's default browser at url. rundll32 keeps the
// trayapp's windowsgui mode console-free (cmd /c start would briefly flash
// a console window even with -H=windowsgui).
func openURL(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// buildTrayIcon wraps the embedded PNG in a single-image Windows ICO
// container. Vista and later accept PNG-in-ICO directly, so we don't
// have to decode and re-encode as a BMP. We still call png.DecodeConfig
// to discover the source dimensions: ICONDIRENTRY width/height fields
// are 8-bit (0 means "256 or larger").
func buildTrayIcon(pngBytes []byte) ([]byte, error) {
	if len(pngBytes) == 0 {
		return nil, fmt.Errorf("icon PNG is empty")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("decode PNG config: %w", err)
	}

	widthByte := byte(cfg.Width)
	if cfg.Width >= 256 {
		widthByte = 0
	}
	heightByte := byte(cfg.Height)
	if cfg.Height >= 256 {
		heightByte = 0
	}

	var buf bytes.Buffer
	// ICONDIR: reserved=0, type=1 (icon), count=1
	buf.Write([]byte{0, 0, 1, 0, 1, 0})
	// ICONDIRENTRY
	binary.Write(&buf, binary.LittleEndian, struct {
		Width, Height, Colors, Reserved byte
		Planes, BitCount                uint16
		BytesInRes, ImageOffset         uint32
	}{widthByte, heightByte, 0, 0, 1, 32, uint32(len(pngBytes)), 22})
	buf.Write(pngBytes)
	return buf.Bytes(), nil
}
