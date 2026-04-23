package hap

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// advertiser runs Apple's `dns-sd -P` as a subprocess to register the HAP
// service via mDNSResponder. We use -P (Proxy) rather than -R so we can pin
// the advertised hostname/IP exactly — the machine's default `mini.local.`
// resolves to ~7 addresses here including invalid network-address bridges,
// which confuses iPhones during pair-setup.
type advertiser struct {
	name    string // HAP service name, e.g. "Front Entrance"
	port    int    // HAP TCP port
	host    string // advertised hostname, e.g. "ringbuzz.local"
	ip      string // advertised IP, e.g. "192.168.0.220"
	id      string // accessory ID, MAC-format "AA:BB:CC:DD:EE:FF"
	setupID string // HomeKit setup ID, 4 chars
	model   string // model string in TXT
	configN int    // c# (configuration number)
	cat     int    // ci= (HomeKit category)

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	paired bool
}

func newAdvertiser(name, id, setupID, model, host, ip string, port, configN, category int) *advertiser {
	return &advertiser{
		name:    name,
		port:    port,
		host:    host,
		ip:      ip,
		id:      id,
		setupID: setupID,
		model:   model,
		configN: configN,
		cat:     category,
	}
}

// Start launches the dns-sd subprocess. Call SetPaired(true) once HAP pairing
// completes so sf=0 gets advertised. Close the parent ctx to tear down.
func (a *advertiser) Start(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.restart(ctx)
}

func (a *advertiser) SetPaired(p bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.paired == p || a.cancel == nil {
		a.paired = p
		return
	}
	a.paired = p
	// Tear down the old subprocess and spawn a new one with updated sf=.
	// Release the lock while waiting for the goroutine to exit so its own
	// slog calls (which don't need a.mu) aren't blocked by us.
	oldCancel := a.cancel
	oldDone := a.done
	a.mu.Unlock()
	oldCancel()
	<-oldDone
	a.mu.Lock()
	a.restart(context.Background())
}

// restart spawns a fresh dns-sd subprocess reflecting the current `paired`
// state. Must be called with a.mu held.
func (a *advertiser) restart(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	a.cancel = cancel
	a.done = make(chan struct{})
	args := a.argsLocked()

	go func() {
		defer close(a.done)
		for ctx.Err() == nil {
			cmd := exec.CommandContext(ctx, "/usr/bin/dns-sd", args...)
			cmd.Stdout = nil
			cmd.Stderr = nil
			slog.Info("registering via dns-sd", "args", args)
			if err := cmd.Run(); err != nil && ctx.Err() == nil {
				slog.Warn("dns-sd exited unexpectedly, restarting", "error", err)
			}
		}
	}()
}

// argsLocked builds the dns-sd -R argv. Must be called with a.mu held.
func (a *advertiser) argsLocked() []string {
	sf := "1"
	if a.paired {
		sf = "0"
	}

	return []string{
		"-P",
		a.name,
		"_hap._tcp",
		"local",
		fmt.Sprintf("%d", a.port),
		a.host,
		a.ip,
		fmt.Sprintf("c#=%d", a.configN),
		"ff=0",
		"id=" + a.id,
		"md=" + a.model,
		"pv=1.1",
		"s#=1",
		"sf=" + sf,
		fmt.Sprintf("ci=%d", a.cat),
		"sh=" + setupHash(a.setupID, a.id),
	}
}

// primaryIPv4 returns the Mac's primary IPv4 address — the first non-loopback
// interface with a routable IPv4 that isn't a weird bridge network-address.
// We pin this into the dns-sd -P advertisement so the iPhone gets exactly
// one reachable IP to connect to.
func primaryIPv4() (string, error) {
	// Prefer en0 if present; fall back to first suitable iface.
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	var fallback string
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
			v4 := ipnet.IP.To4()
			if v4 == nil || v4.IsLinkLocalUnicast() {
				continue
			}
			// Skip network-address /24 /.0s (macOS advertises these for some
			// bridge interfaces and they break pair-setup discovery).
			if v4[3] == 0 {
				continue
			}
			if iface.Name == "en0" {
				return v4.String(), nil
			}
			if fallback == "" {
				fallback = v4.String()
			}
		}
	}
	if fallback == "" {
		return "", fmt.Errorf("no routable IPv4 interface found")
	}
	return fallback, nil
}

// setupHash = base64(first 4 bytes of SHA-512(setupID + accessoryID)),
// per HAP R17 §5.4 ("Accessory Setup Hash"). The accessory ID must be
// formatted with uppercase colon-separated bytes (e.g. "AA:BB:CC:DD:EE:FF").
func setupHash(setupID, accessoryID string) string {
	h := sha512.Sum512([]byte(setupID + accessoryID))
	return base64.StdEncoding.EncodeToString(h[:4])
}

// readAccessoryID reads the MAC-format ID brutella/hap persists as `uuid`
// on first launch. That file is authoritative for the accessory identity.
func readAccessoryID(hapDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(hapDir, "uuid"))
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if id == "" {
		return "", fmt.Errorf("empty uuid file")
	}
	return id, nil
}
