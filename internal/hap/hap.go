// Package hap runs the HomeKit accessory server exposing a single Lock.
package hap

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"
	"github.com/mdp/qrterminal/v3"
)

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
	a := newLockAccessory(accessory.Info{
		Name:         s.name,
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

	printSetupInfo(s.pin, a.A.Info.Name.Value())

	return server.ListenAndServe(ctx)
}

// printSetupInfo writes the HomeKit pairing code and an ASCII QR to stdout.
// brutella/hap also logs the setup URI, but this makes it easy to read.
func printSetupInfo(pin, name string) {
	formatted := fmt.Sprintf("%s-%s-%s", pin[0:3], pin[3:5], pin[5:8])
	// Setup payload: "X-HM://" + base36-encoded flags/category/setupcode.
	// For simplicity we just QR-encode the PIN — the Home app accepts raw
	// 8-digit codes via "Enter Code Manually" and via QR when flagged as HAP.
	fmt.Fprintln(os.Stdout)
	fmt.Fprintln(os.Stdout, "HomeKit pairing for:", name)
	fmt.Fprintln(os.Stdout, "Setup code:", formatted)
	fmt.Fprintln(os.Stdout)
	qrterminal.GenerateHalfBlock(formatted, qrterminal.L, os.Stdout)
	fmt.Fprintln(os.Stdout, "In Home app: Add Accessory → More Options → pick this device → enter code.")
	fmt.Fprintln(os.Stdout)
}
