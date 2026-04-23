# ringbuzz

A minimal Ring Intercom → Apple HomeKit bridge. Exposes a single Lock accessory so you can buzz visitors in from your iPhone without opening the Ring app.

One Go binary, no Docker, no Homebridge, no Node. Runs under `launchctl` on macOS.

## What it does

- Exposes **one** HomeKit Lock accessory.
- On **Unlock** from Apple Home → calls Ring's cloud API to buzz the door open.
- Auto-returns to **Locked** after 5 seconds (intercom unlock is momentary).

## What it does not do

Doorbell events, video, audio, multiple intercoms, a config UI, or anything else. That's what [homebridge-ring](https://github.com/dgreif/ring) is for. If you want the kitchen sink, use that.

## Install

```sh
git clone https://github.com/jokull/ringbuzz ~/Code/ringbuzz
cd ~/Code/ringbuzz
make install          # → ~/bin/ringbuzz
```

## Setup

```sh
ringbuzz login                 # prompts for Ring email, password, 2FA code
ringbuzz devices               # prints numeric IDs for every Ring device
ringbuzz use <device-id>       # pick the intercom
ringbuzz unlock                # smoke test — does the door actually buzz?
```

Once `ringbuzz unlock` works, start the daemon:

```sh
ringbuzz daemon                # foreground; prints QR + setup code
```

Open Home app → **+** → **Add Accessory** → scan the QR, or enter the 8-digit code manually. Home will show "Uncertified Accessory" — tap **Add Anyway**.

To run at login via launchd:

```sh
cp scripts/com.ringbuzz.daemon.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.ringbuzz.daemon.plist
```

## CLI reference

```
ringbuzz login                 # OAuth + 2FA, stores refresh token
ringbuzz devices               # list Ring devices on the account
ringbuzz use <device-id>       # select which intercom to control
ringbuzz unlock                # fire the unlock once (test)
ringbuzz pair                  # print HomeKit setup code
ringbuzz daemon                # run the HomeKit server
```

## Files

| Path | Purpose |
|---|---|
| `~/bin/ringbuzz` | binary (via `make install`) |
| `~/.config/ringbuzz/config.toml` | refresh token + selected device |
| `~/.config/ringbuzz/hap/` | HomeKit pairing state (keypair, accessory UUID) |
| `~/Library/LaunchAgents/com.ringbuzz.daemon.plist` | launchd config |
| `~/Library/Logs/ringbuzz.log` | stdout + stderr when running under launchd |

## Platform

macOS only, currently. Pairing on macOS required working around two quirks:

- **mDNS announcements from `brutella/dnssd` never leave loopback when Apple's mDNSResponder is running.** Ringbuzz bypasses it by shelling out to `dns-sd -P` for service registration. Apple's native Bonjour is the authoritative mDNS responder on every macOS box anyway.
- **Go's `net/http` rejects Host headers containing backslashes** with "malformed Host header". iPhones URL-encode spaces in the mDNS service instance name as `\032`, so a service named "Front Entrance" becomes `Front\032Entrance._hap._tcp.local` — which Go treats as malformed. Ringbuzz strips spaces from the mDNS service name (Home displays the user-facing name via the HAP `Name` characteristic, not mDNS).

These could be fixed upstream; they are not.

## Debugging

Set `RINGBUZZ_DEBUG=1` in the launchd plist environment or shell to enable verbose `brutella/hap` and `brutella/dnssd` logs. Off by default — the chatter is overwhelming in normal operation.

## Credits

- Ring wire format ported from the MIT-licensed [dgreif/ring-client-api](https://github.com/dgreif/ring). The three endpoints used — OAuth, device list, and `device_rpc` unlock — are cribbed verbatim from `rest-client.ts` and `ring-intercom.ts`.
- HAP server courtesy of [brutella/hap](https://github.com/brutella/hap).
- QR code rendering via [mdp/qrterminal](https://github.com/mdp/qrterminal).

## License

MIT.
