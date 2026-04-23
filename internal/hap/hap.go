// Package hap runs the HomeKit accessory server exposing a single Lock.
package hap

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	dnssdlog "github.com/brutella/dnssd/log"
	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	hlog "github.com/brutella/hap/log"
	"github.com/brutella/hap/service"
	"github.com/mdp/qrterminal/v3"
)

func init() {
	// Set RINGBUZZ_DEBUG=1 to route brutella/hap and brutella/dnssd debug
	// logs to stdout. Off by default — the dnssd probe/announcement chatter
	// is overwhelming in normal operation.
	if os.Getenv("RINGBUZZ_DEBUG") == "1" {
		hlog.Debug.Enable()
		dnssdlog.Debug.Enable()
	}
}

// Fixed HomeKit setup ID (4 uppercase alphanumeric chars). Must stay stable —
// changing it invalidates the mDNS pairing hash and forces re-pairing.
const setupID = "RING"

// lockAccessory composes a HomeKit DoorLock-category accessory with a single
// LockMechanism service. brutella/hap v0.0.35 doesn't ship a NewLock helper.
type lockAccessory struct {
	*accessory.A
	LockMechanism *service.LockMechanism
}

func newLockAccessory(info accessory.Info) *lockAccessory {
	a := &lockAccessory{A: accessory.New(info, accessory.TypeDoorLock)}
	a.LockMechanism = service.NewLockMechanism()
	a.AddS(a.LockMechanism.S)
	return a
}

// Unlocker is called when HomeKit asks to unlock. Return nil on success.
type Unlocker func(ctx context.Context) error

type Server struct {
	dir      string
	pin      string
	deviceID string
	name     string
	unlock   Unlocker

	// AutoRelockAfter is how long to keep the lock showing "Unlocked" in Home
	// before auto-returning to "Locked". Ring intercom unlock is momentary.
	AutoRelockAfter time.Duration
}

func New(dir, pin, deviceID, name string, unlock Unlocker) *Server {
	return &Server{
		dir:             dir,
		pin:             pin,
		deviceID:        deviceID,
		name:            name,
		unlock:          unlock,
		AutoRelockAfter: 5 * time.Second,
	}
}

func (s *Server) Run(ctx context.Context) error {
	// Give brutella a unique, obscure mDNS service name so its query-
	// responder doesn't pollute Apple's mDNSResponder cache with a
	// duplicate "Front Entrance" that points at an unreachable hostname.
	// We flip the HAP Name characteristic to the real name after the
	// server boots — brutella caches the mDNS name from accessory.Info.Name
	// at NewService() time and doesn't re-read it on query responses.
	brutellaName := fmt.Sprintf("rbz-%s", strings.ReplaceAll(s.deviceID, ":", ""))
	a := newLockAccessory(accessory.Info{
		Name:         brutellaName,
		Manufacturer: "Ring",
		Model:        "Intercom",
		SerialNumber: s.deviceID,
		Firmware:     "1.0.0",
	})

	// Start locked.
	a.LockMechanism.LockCurrentState.SetValue(characteristic.LockCurrentStateSecured)
	a.LockMechanism.LockTargetState.SetValue(characteristic.LockTargetStateSecured)

	a.LockMechanism.LockTargetState.OnValueRemoteUpdate(func(v int) {
		if v != characteristic.LockTargetStateUnsecured {
			// Home asked to re-lock. We always show locked, nothing to do.
			a.LockMechanism.LockCurrentState.SetValue(characteristic.LockCurrentStateSecured)
			return
		}

		slog.Info("unlock requested from HomeKit")
		// Fire Ring unlock with a bounded timeout so the HAP write returns quickly.
		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		if err := s.unlock(reqCtx); err != nil {
			slog.Error("ring unlock failed", "error", err)
			// Snap back to locked state so Home reflects reality.
			a.LockMechanism.LockCurrentState.SetValue(characteristic.LockCurrentStateSecured)
			a.LockMechanism.LockTargetState.SetValue(characteristic.LockTargetStateSecured)
			return
		}

		a.LockMechanism.LockCurrentState.SetValue(characteristic.LockCurrentStateUnsecured)

		// Auto-relock after the configured delay.
		go func() {
			select {
			case <-time.After(s.AutoRelockAfter):
			case <-ctx.Done():
				return
			}
			a.LockMechanism.LockCurrentState.SetValue(characteristic.LockCurrentStateSecured)
			a.LockMechanism.LockTargetState.SetValue(characteristic.LockTargetStateSecured)
		}()
	})

	fs := hap.NewFsStore(s.dir)
	server, err := hap.NewServer(fs, a.A)
	if err != nil {
		return fmt.Errorf("new hap server: %w", err)
	}
	server.Pin = s.pin
	server.SetupId = setupID
	// Pin to a known port so we can tell dns-sd about it up front.
	server.Addr = fmt.Sprintf(":%d", hapPort)
	// Suppress brutella's mDNS entirely by targeting a non-existent iface.
	// Its announcements would otherwise land on loopback, mDNSResponder
	// would cache them alongside our dns-sd -R registration, and the iPhone
	// would see two competing services for the same name.
	server.Ifaces = []string{"ringbuzz-noop"}

	id, err := readAccessoryID(s.dir)
	if err != nil {
		return fmt.Errorf("read hap uuid: %w", err)
	}

	ip, err := primaryIPv4()
	if err != nil {
		return fmt.Errorf("detect primary ipv4: %w", err)
	}

	// Sanitize the mDNS service name: Go's net/http rejects Host headers
	// containing backslashes, which is what iPhones send when the service
	// name contains spaces (iPhone uses the escaped form "Front\032Entrance"
	// as the Host header). Strip spaces but preserve the display name
	// separately via the HAP Name characteristic.
	mdnsName := strings.ReplaceAll(s.name, " ", "-")

	adv := newAdvertiser(
		mdnsName,
		id,
		setupID,
		a.A.Info.Model.Value(),
		"ringbuzz.local",              // dedicated hostname (A record)
		ip,
		hapPort,
		1, // initial configuration number
		int(accessory.TypeDoorLock),
	)
	slog.Info("advertising HAP via dns-sd",
		"service", mdnsName, "host", "ringbuzz.local", "ip", ip, "port", hapPort, "id", id)
	adv.Start(ctx)

	// Poll pairing state so we flip sf=1 → sf=0 when a controller pairs.
	// brutella/hap doesn't expose pairing-event hooks.
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		prev := server.IsPaired()
		adv.SetPaired(prev)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cur := server.IsPaired()
				if cur != prev {
					slog.Info("hap pairing state changed", "paired", cur)
					adv.SetPaired(cur)
					prev = cur
				}
			}
		}
	}()

	// After brutella's mDNS service name is locked in, flip the HAP
	// Name characteristic to the user-facing name. Home pulls this via
	// HAP /accessories, not from mDNS.
	go func() {
		time.Sleep(100 * time.Millisecond)
		a.A.Info.Name.SetValue(s.name)
	}()

	printSetupInfo(s.pin, s.name, accessory.TypeDoorLock)
	return server.ListenAndServe(ctx)
}

// hapPort is a fixed TCP port for the HAP server. Pinned so dns-sd -R can be
// told the port up front, and so pairings survive daemon restarts.
const hapPort = 51827

// printSetupInfo writes the HomeKit pairing code plus an ASCII QR encoding
// the proper X-HM:// setup URI so iOS's camera scanner recognises it.
func printSetupInfo(pin, name string, category byte) {
	formatted := fmt.Sprintf("%s-%s-%s", pin[0:3], pin[3:5], pin[5:8])
	uri := setupURI(pin, setupID, category)
	fmt.Fprintln(os.Stdout)
	fmt.Fprintln(os.Stdout, "HomeKit pairing for:", name)
	fmt.Fprintln(os.Stdout, "Setup code:", formatted)
	fmt.Fprintln(os.Stdout, "Setup URI: ", uri)
	fmt.Fprintln(os.Stdout)
	qrterminal.GenerateHalfBlock(uri, qrterminal.L, os.Stdout)
	fmt.Fprintln(os.Stdout, "In Home app: Add Accessory → scan this QR, or enter code manually.")
	fmt.Fprintln(os.Stdout)
}

// primaryIfaces returns interface names that have a non-loopback IPv4
// address — typically `en0`. An empty slice lets brutella/dnssd pick,
// which is unreliable on macOS.
func primaryIfaces() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if v4 := ipnet.IP.To4(); v4 != nil && !v4.IsLinkLocalUnicast() {
				out = append(out, iface.Name)
				break
			}
		}
	}
	return out
}

// setupURI builds the HomeKit X-HM:// payload per HAP R17 §4.2.1.8.
// Bit layout (MSB → LSB): 3 version | 4 reserved | 8 category | 4 flags | 27 setupCode.
// Encoded as zero-padded 9-char base36 uppercase, suffixed with the 4-char setupID.
func setupURI(pin, setupID string, category byte) string {
	code, _ := strconv.ParseUint(strings.ReplaceAll(pin, "-", ""), 10, 32)
	const flagsIP = uint64(2) // IP transport
	var payload uint64
	payload = (payload << 3) | 0 // version
	payload = (payload << 4) | 0 // reserved
	payload = (payload << 8) | uint64(category)
	payload = (payload << 4) | flagsIP
	payload = (payload << 27) | code

	encoded := strings.ToUpper(strconv.FormatUint(payload, 36))
	if len(encoded) < 9 {
		encoded = strings.Repeat("0", 9-len(encoded)) + encoded
	}
	return "X-HM://" + encoded + setupID
}
