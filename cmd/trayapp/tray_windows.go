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
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"

	"fyne.io/systray"
	"golang.org/x/sys/windows"

	"github.com/vector76/cc_usage_dashboard/internal/authtoken"
	"github.com/vector76/cc_usage_dashboard/internal/icon"
	"github.com/vector76/cc_usage_dashboard/internal/server"
)

// StartTray runs the Windows systray UI. It blocks until ctx is cancelled
// or the user picks Quit, at which point it tears down the tray and
// returns. dashboardURL is the http://host:port the "Open dashboard" item
// should launch in the user's default browser. tokens backs the access
// token menu items.
func StartTray(ctx context.Context, srv *server.Server, paused interface{ Toggle() }, tokens *authtoken.Store, dashboardURL string) {
	// Pin this goroutine to a single OS thread for the lifetime of the
	// systray. fyne.io/systray creates its hidden window with CreateWindowEx
	// inside Run; Win32 delivers WM_NOTIFY (tray clicks) and TaskbarCreated
	// only to the queue of the thread that created the window. The library's
	// own LockOSThread runs in package init on the main goroutine, which
	// doesn't help us because StartTray is invoked from a goroutine spawned
	// in main.main. Without this lock, Go's scheduler eventually migrates
	// the pump goroutine off the window-owning thread and GetMessage starts
	// draining a queue that never receives any tray messages — the icon
	// goes silent while the rest of trayapp keeps running.
	runtime.LockOSThread()

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
		mPause := systray.AddMenuItemCheckbox("Pause slack signal", "Suppress release recommendations", false)
		systray.AddSeparator()
		mCopyToken := systray.AddMenuItem("Copy access token", "Copy the token VMs and containers must present")
		mRotateToken := systray.AddMenuItem("Rotate access token…", "Issue a new token and refuse the old one")
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
				case <-mCopyToken.ClickedCh:
				// Dialogs run in their own goroutines, like About, so
				// the dispatcher stays free while one is open.
				go copyAccessToken(tokens)
			case <-mRotateToken.ClickedCh:
				go rotateAccessToken(tokens)
			case <-mAbout.ClickedCh:
					// Run the modal MessageBox in a fresh goroutine so the
					// menu dispatcher is free to handle other clicks while
					// the dialog is open.
					go showAboutDialog()
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

// MessageBox flags and return values used by the tray dialogs.
const (
	mbOK          = 0x00000000
	mbYesNo       = 0x00000004
	mbIconError   = 0x00000010
	mbIconWarning = 0x00000030
	mbIconInfo    = 0x00000040
	mbDefButton2  = 0x00000100
	mbTopmost     = 0x00040000
	idYes         = 6
)

// messageBox shows a modal dialog and returns the button pressed, or 0 when
// the dialog could not be shown. MB_TOPMOST keeps a dialog launched from the
// tray from opening behind other windows.
func messageBox(body, caption string, flags uint32) int32 {
	bodyPtr, err := windows.UTF16PtrFromString(body)
	if err != nil {
		slog.Warn("tray: encode dialog body failed", "err", err)
		return 0
	}
	captionPtr, err := windows.UTF16PtrFromString(caption)
	if err != nil {
		slog.Warn("tray: encode dialog caption failed", "err", err)
		return 0
	}
	ret, err := windows.MessageBox(0, bodyPtr, captionPtr, flags|mbTopmost)
	if err != nil {
		slog.Warn("tray: MessageBox failed", "err", err)
		return 0
	}
	return ret
}

// copyAccessToken puts the current token on the clipboard. It goes to the
// clipboard rather than into the dialog so it is not left on screen.
func copyAccessToken(tokens *authtoken.Store) {
	const caption = "Access token"
	tok := tokens.Token()
	if tok == "" {
		messageBox("No access token is loaded, so every caller not on this machine is being refused. "+
			"See the log for why the token file could not be read.", caption, mbOK|mbIconError)
		return
	}
	if err := copyToClipboard(tok); err != nil {
		slog.Error("tray: copy access token failed", "err", err)
		messageBox("Could not copy the token to the clipboard:\n"+err.Error()+
			"\n\nIt is stored in:\n"+tokens.Path(), caption, mbOK|mbIconError)
		return
	}
	messageBox("Access token copied to the clipboard.\n\n"+
		"On a VM, paste it into config.yaml as uplink.token.\n"+
		"In a container, set it as CLUSAGE_TOKEN.\n\n"+
		"Stored in:\n"+tokens.Path(), caption, mbOK|mbIconInfo)
}

// rotateAccessToken replaces the token after confirmation and copies the new
// one. The default button is No: this is the one tray action that breaks
// other machines.
func rotateAccessToken(tokens *authtoken.Store) {
	const caption = "Rotate access token"
	answer := messageBox("Issue a new access token?\n\n"+
		"Every VM and container using the current token will be refused until it is given the new one. "+
		"No usage is lost: an uplink keeps its backlog and resends it once the token is updated.",
		caption, mbYesNo|mbIconWarning|mbDefButton2)
	if answer != idYes {
		return
	}
	tok, err := tokens.Rotate()
	if err != nil {
		slog.Error("tray: rotate access token failed", "err", err)
		messageBox("Could not rotate the token; the current one still works.\n\n"+err.Error(), caption, mbOK|mbIconError)
		return
	}
	slog.Info("tray: access token rotated")
	if err := copyToClipboard(tok); err != nil {
		slog.Error("tray: copy access token failed", "err", err)
		messageBox("The token was rotated, but could not be copied to the clipboard:\n"+err.Error()+
			"\n\nIt is stored in:\n"+tokens.Path(), caption, mbOK|mbIconWarning)
		return
	}
	messageBox("New access token copied to the clipboard. Update each VM's uplink.token and each container's CLUSAGE_TOKEN.",
		caption, mbOK|mbIconInfo)
}

// copyToClipboard sets the clipboard text via clip.exe. CREATE_NO_WINDOW
// keeps the console program from flashing a window out of this windowsgui
// process. The token is plain ASCII, so clip's code-page handling of stdin
// cannot garble it.
func copyToClipboard(text string) error {
	cmd := exec.Command("clip")
	cmd.Stdin = strings.NewReader(text)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.Run()
}

// showAboutDialog pops a modal Windows MessageBox with version + build info.
// VCS revision comes from debug.ReadBuildInfo, which Go populates automatically
// when building inside a git checkout — no -ldflags injection required.
func showAboutDialog() {
	body := fmt.Sprintf(
		"Claude Usage Dashboard\nVersion: %s\nCommit:  %s\n\n%s",
		Version, buildRevision(), runtime.Version(),
	)
	messageBox(body, "About Claude Usage Dashboard", mbOK|mbIconInfo)
}

// buildRevision returns the git commit hash baked in by `go build`, or
// "(unknown)" when build info is unavailable (e.g. tests, GOFLAGS overrides).
func buildRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(unknown)"
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			if len(s.Value) >= 12 {
				return s.Value[:12]
			}
			return s.Value
		}
	}
	return "(unknown)"
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
