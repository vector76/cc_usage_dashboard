# Userscript

`claude-usage-snapshot.user.js` is a Tampermonkey/Violentmonkey userscript that reads
quota numbers from `claude.ai` and POSTs them to the local Claude Usage Dashboard
trayapp at `http://localhost:27812/snapshot`.

See `docs/userscript.md` for the rationale (mixed content, CORS, Private Network
Access) and the snapshot payload schema.

## Installation

1. Install [Tampermonkey](https://www.tampermonkey.net/) (Chrome/Edge/Firefox/Safari)
   or [Violentmonkey](https://violentmonkey.github.io/) (Chrome/Edge/Firefox).
2. Open `userscript/claude-usage-snapshot.user.js` in your browser, either:
   - drag the file into the userscript manager's dashboard, or
   - open the GitHub raw URL — your manager will offer to install it.
3. Confirm the install and grant `GM.xmlHttpRequest` plus `@connect localhost` /
   `@connect 127.0.0.1` when prompted.
4. Make sure the trayapp is running locally on port `27812`.
5. Open `https://claude.ai/` in any tab. Within ~30 seconds the script will detect
   the quota nodes and start sending one snapshot per minute.

## Updating

The script declares `@updateURL` and `@downloadURL` pointing at the GitHub raw URL,
so Tampermonkey/Violentmonkey will offer updates automatically (interval is
configurable inside the manager).

## Troubleshooting

- Open the page console and filter for `[claude-usage-snapshot]`. All transport
  errors are logged with `console.warn` and never thrown.
- If the trayapp's tray UI shows a "userscript broke" warning, the script has
  detected that the expected DOM nodes have been missing for longer than 5
  minutes and has posted a `parse_error` payload — the page DOM has likely
  changed and the script needs an update.
