# ringbuzz

Minimal Ring Intercom → Apple HomeKit bridge. Exposes a single Lock accessory so you can buzz visitors in from your iPhone without opening the Ring app.

Single Go binary, runs under `launchd` on macOS. No Docker, no Homebridge, no Node.

## What it does

- Exposes one HomeKit Lock accessory named **"Ring Intercom"**.
- On "Unlock" from Apple Home → calls Ring's cloud API to buzz the door open.
- Auto-returns to "Locked" after 5 seconds (intercom unlock is momentary).

**What it does not do:** doorbell events, video, audio, multiple intercoms, anything else. That's homebridge-ring's job.

## Install

```bash
git clone https://github.com/jokull/ringbuzz ~/Code/ringbuzz
cd ~/Code/ringbuzz
make install                       # builds and installs to ~/bin/ringbuzz
```

## One-time setup

1. **Log in to Ring** (does 2FA, stores a refresh token at `~/.config/ringbuzz/config.toml`):
   ```bash
   ringbuzz login
   ```

2. **Pick your intercom** (lists all devices on the account):
   ```bash
   ringbuzz devices
   ringbuzz use <device-id>
   ```

3. **Start the daemon** and pair with Apple Home:
   ```bash
   ringbuzz daemon
   ```
   A setup code and QR will print. In the Home app → Add Accessory → scan.

4. **Run at login** via launchd:
   ```bash
   cp scripts/com.ringbuzz.daemon.plist ~/Library/LaunchAgents/
   launchctl load ~/Library/LaunchAgents/com.ringbuzz.daemon.plist
   ```

## CLI

```
ringbuzz login                 # OAuth + 2FA, writes refresh token
ringbuzz devices               # list Ring devices on account
ringbuzz use <device-id>       # select which intercom to control
ringbuzz unlock                # fire the unlock from the CLI (test)
ringbuzz pair                  # print HomeKit setup code + QR
ringbuzz daemon                # run the HomeKit server
```

## Files

- Binary: `~/bin/ringbuzz`
- Config + refresh token: `~/.config/ringbuzz/config.toml`
- HomeKit pairing state: `~/.config/ringbuzz/hap/`
- Launchd plist: `~/Library/LaunchAgents/com.ringbuzz.daemon.plist`
- Logs: `~/Library/Logs/ringbuzz.log`

## Why this exists

Homebridge-ring is the mature option if you want everything (doorbell chimes, video, cameras, multi-device). If you only want to unlock your Ring Intercom from Siri, this is 200 LOC of Go you can read in 10 minutes.

## Acknowledgements

Ring wire-format details are cribbed from the MIT-licensed [dgreif/ring-client-api](https://github.com/dgreif/ring). HomeKit server is [brutella/hap](https://github.com/brutella/hap).
