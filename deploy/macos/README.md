# Intercepting Claude Desktop on macOS

This is the supported way to run the interceptor against Claude Desktop: a background service
that starts at login, plus Claude Desktop's own `egressProxyUrl` setting pointed at it. It works
however the app is opened — Dock, Spotlight, or login — and it forwards Claude's Code tab and Chat
tab traffic to a Tuskira gateway. Part 7 covers how to undo everything.

Two pieces do the work:

- **The `com.tuskira.interceptor` LaunchAgent** starts the interceptor at login on `127.0.0.1:9090`,
  restarts it if it crashes, writes captured traffic to `~/claude-capture.jsonl`, and forwards
  Claude's Code/Chat tab traffic to your gateway. Gateway forwarding is not optional — the
  interceptor refuses to start without it.
- **Claude Desktop's `egressProxyUrl` setting** tells Claude to send all its traffic to
  `http://127.0.0.1:9090`. It applies however the app is opened, and also covers the Claude Code
  engine the app spawns. The setting fails closed: while it is set, Claude cannot connect unless the
  interceptor is running.

Tested on macOS with Claude Desktop 2.9939.4.

**You need:** Go, `jq`, and your Mac admin password (for trusting the CA). You also need a Tuskira
gateway URL and someone able to create a gateway API key for you (Part 3).

## Part 1: build

```sh
cd /path/to/interceptor && make build
```

You should see `bin/interceptor` created.

## Part 2: create and trust the CA

```sh
./bin/interceptor ca init
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ~/.interceptor/ca.pem
```

`ca init` writes `~/.interceptor/ca.pem` and `~/.interceptor/ca-key.pem` and prints the trust
command above. The `security add-trusted-cert` step asks for your admin password; it makes macOS
trust certificates the interceptor signs, system-wide.

## Part 3: create a gateway key

Ask a gateway admin to create a key with role `interceptor`, either from the gateway console's API
Keys page, or:

```sh
curl -s -X POST "$GATEWAY_URL/api/v1/api-keys" \
  -H 'Content-Type: application/json' \
  -d '{"name":"<your name> interceptor","role":"interceptor"}'
```

Save **only the key itself** (not the whole JSON response) to a mode-0600 file:

```sh
echo -n "gk_..." > ~/.interceptor/gateway.key
chmod 600 ~/.interceptor/gateway.key
```

The interceptor refuses to start if this file is not mode 0600.

## Part 4: install the background service

```sh
make install-agent GATEWAY_URL=https://your-gateway.example.com
```

This renders `deploy/macos/com.tuskira.interceptor.plist` (a template — launchd does not expand
`$HOME`) with your home directory, this repo's path, and the gateway URL you gave it; validates the
result with `plutil -lint`; and loads it with `launchctl bootstrap`. It is safe to re-run, including
after a rebuild or a gateway URL change — it unloads any previously-installed copy first.

Check it is running. You should see `proxy listening on 127.0.0.1:9090` and `forwarding Claude
Desktop traffic to https://your-gateway.example.com`:

```sh
tail -5 ~/Library/Logs/interceptor.log
```

Test it. This should print `200`:

```sh
curl -s --proxy http://127.0.0.1:9090 --cacert ~/.interceptor/ca.pem https://example.com -o /dev/null -w '%{http_code}\n'
```

Useful commands once it's installed:

```sh
launchctl kickstart -k gui/$(id -u)/com.tuskira.interceptor   # restart, e.g. after rebuilding
launchctl bootout gui/$(id -u)/com.tuskira.interceptor        # stop
make uninstall-agent                                          # stop and remove
```

<details>
<summary>What <code>make install-agent</code> runs, if you want to do it by hand or customize flags</summary>

```sh
sed -e "s#__HOME__#$HOME#g" -e "s#__REPO__#$(pwd)#g" -e "s#__GATEWAY_URL__#https://your-gateway.example.com#g" \
  deploy/macos/com.tuskira.interceptor.plist > ~/Library/LaunchAgents/com.tuskira.interceptor.plist
plutil -lint ~/Library/LaunchAgents/com.tuskira.interceptor.plist
launchctl bootout gui/$(id -u)/com.tuskira.interceptor 2>/dev/null
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.tuskira.interceptor.plist
```

Edit the rendered file's `ProgramArguments` for anything beyond the gateway URL — a different
`--proxy-addr`, `-v`, `--filter-host`, and so on — then reload with the `bootout`/`bootstrap` pair
above.

</details>

## Part 5: point Claude Desktop at it

Claude Desktop reads managed settings from a local configuration library in `~/Library/Application
Support/Claude-3p/configLibrary/`. `_meta.json` in that folder names the active configuration
(`appliedId`), and the settings live in `<appliedId>.json`. The ID is random and differs on every
Mac, so the commands below read it rather than hard-coding it.

1. Quit Claude Desktop completely with Cmd+Q.
2. Add the setting to the active configuration. This keeps any other settings already in the file:
   ```sh
   LIB="$HOME/Library/Application Support/Claude-3p/configLibrary"
   ID=$(jq -r .appliedId "$LIB/_meta.json")
   F="$LIB/$ID.json"
   [ -s "$F" ] || echo '{}' > "$F"
   jq '.egressProxyUrl = "http://127.0.0.1:9090"' "$F" > "$F.tmp" && mv "$F.tmp" "$F" && chmod 600 "$F"
   cat "$F"
   ```
   The last command should print a JSON object containing `"egressProxyUrl": "http://127.0.0.1:9090"`.

   If `_meta.json` does not exist yet, create the library first, then run step 2 again:
   ```sh
   LIB="$HOME/Library/Application Support/Claude-3p/configLibrary"
   ID=$(uuidgen | tr 'A-Z' 'a-z')
   mkdir -p "$LIB" && chmod 700 "$LIB"
   printf '{\n  "appliedId": "%s",\n  "entries": [{ "id": "%s", "name": "Default" }]\n}\n' "$ID" "$ID" > "$LIB/_meta.json"
   echo '{}' > "$LIB/$ID.json"
   chmod 600 "$LIB/_meta.json" "$LIB/$ID.json"
   ```
3. Open Claude from the Dock, not the terminal.

Claude's in-app settings screen shows this same field (**Network proxy → Proxy server URL**), but it
may not save it on its own, so edit the file as above. Claude reads the file only at startup, so
restart Claude after any change. A user can edit or clear this file at any time.

## Part 6: verify

1. The service log should show both listeners coming up at startup:
   ```sh
   tail -20 ~/Library/Logs/interceptor.log
   ```
   Look for `proxy listening on 127.0.0.1:9090` and `forwarding Claude Desktop traffic to
   https://your-gateway.example.com`.
2. Use Claude Desktop for a moment, then confirm traffic arrives. You should see `claude.ai` lines:
   ```sh
   tail -f ~/Library/Logs/interceptor.log
   ```
3. Wait up to 60 seconds and confirm the status line is moving:
   ```sh
   grep 'forward status' ~/Library/Logs/interceptor.log | tail -3
   ```
   `sent=` should increase as you use Claude. `rejected=` and `dropped=` should stay at 0.
4. In the gateway console, open **LLM Logs** and filter by source **Interceptor**. You should see
   rows tagged with the **Interceptor** badge, with the **User** column showing your Claude account
   email. That email is learned from traffic at app start, so if it's missing or wrong, restart
   Claude Desktop once and check again.

## Watching request and response bodies

The service log shows one summary line per request, with sizes only. Full bodies go to
`~/claude-capture.jsonl`.

To watch bodies live, with heartbeats, presence pings, and telemetry hidden:

```sh
tail -f ~/claude-capture.jsonl | jq --unbuffered -r '
  select((.url|test("presence|heartbeat|event_logging|datadog|branch-status|mark_read"))|not)
  | select(((.req_body+.resp_body)|test("\"type\":\"(hb|hb_ack|keep_alive)\""))|not)
  | "\n" + .ts[11:19] + "  " + .method + "  " + (.url|.[0:100]) + "  " + (.status|tostring)
    + "\n  REQUEST:  " + ((.req_body_decoded // .req_body)|tostring|.[0:1000])
    + "\n  RESPONSE: " + ((.resp_body_decoded // .resp_body)|tostring|.[0:1000])'
```

Each body is cut at 1,000 characters. Change `1000` to see more. Protobuf chat traffic is shown in
its decoded form.

To search past traffic, including decoded protobuf, for some text:

```sh
./bin/interceptor decode ~/claude-capture.jsonl --grep 'some text'
```

To print full bodies in the service log itself, add `-v` to the plist's `ProgramArguments` (after
`run`) in `~/Library/LaunchAgents/com.tuskira.interceptor.plist`, then reload:

```sh
launchctl bootout gui/$(id -u)/com.tuskira.interceptor
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.tuskira.interceptor.plist
```

The log grows quickly with this on. Re-running `make install-agent` overwrites this edit; make it
again after any reinstall if you want it to stick.

## Good to know

- **If the service stops, Claude cannot connect.** The proxy setting fails closed: while
  `egressProxyUrl` is set, Claude does not fall back to a direct connection. Check the service first:
  ```sh
  launchctl print gui/$(id -u)/com.tuskira.interceptor | grep -E 'state|pid'
  ```
- **If the service won't start,** check the log for a required-flag error — `interceptor run` refuses
  to start without a working `--gateway-url` and gateway key:
  ```sh
  tail -20 ~/Library/Logs/interceptor.log
  ```
- **After rebuilding the interceptor,** restart the service so it picks up the new binary:
  ```sh
  launchctl kickstart -k gui/$(id -u)/com.tuskira.interceptor
  ```
- **Disk use.** The capture file keeps growing. Delete or rotate it from time to time.
- **Sensitive data.** The capture file holds full chats and session tokens. Treat it like a password
  file. The gateway key is equally sensitive; both are mode 0600 by design.
- **If the gateway is unreachable,** records queue to disk under `~/.interceptor/spool` and are
  retried automatically; nothing is lost across a restart of this service.

## Part 7: undo everything

Run these in order to return to your normal setup.

1. **Quit Claude with Cmd+Q.**
2. **Remove the proxy setting.** Keeps any other settings in the file:
   ```sh
   LIB="$HOME/Library/Application Support/Claude-3p/configLibrary"
   F="$LIB/$(jq -r .appliedId "$LIB/_meta.json").json"
   jq 'del(.egressProxyUrl)' "$F" > "$F.tmp" && mv "$F.tmp" "$F" && chmod 600 "$F"
   cat "$F"
   ```
3. **Stop and remove the background service:**
   ```sh
   make uninstall-agent
   ```
   or, without the Makefile:
   ```sh
   launchctl bootout gui/$(id -u)/com.tuskira.interceptor
   rm ~/Library/LaunchAgents/com.tuskira.interceptor.plist
   ```
4. **Open Claude from the Dock.** It now connects directly, with no interceptor.
5. **Remove the CA from system trust.** Do not skip this. While the CA is trusted and its private key
   is on disk, anything holding that key can read your HTTPS traffic.
   ```sh
   sudo security delete-certificate -c "Interceptor Local CA" /Library/Keychains/System.keychain
   ```
6. **Delete the CA key, gateway key, and captured data:**
   ```sh
   rm -rf ~/.interceptor ~/claude-capture.jsonl ~/Library/Logs/interceptor.log
   ```

The interceptor code and the files in this folder stay in the repo. To start again later, begin at
Part 1.
