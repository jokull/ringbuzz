package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/jokull/ringbuzz/internal/config"
	haprun "github.com/jokull/ringbuzz/internal/hap"
	"github.com/jokull/ringbuzz/internal/ring"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// HomeKit setup PIN. Fixed so re-pairing after a reset doesn't need a fresh
// lookup. Change once before your first pair if you care.
const hapPIN = "01023456"

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	root := &cobra.Command{
		Use:   "ringbuzz",
		Short: "Ring Intercom → Apple HomeKit bridge (unlock only)",
	}
	root.AddCommand(
		loginCmd(),
		devicesCmd(),
		useCmd(),
		unlockCmd(),
		pairCmd(),
		daemonCmd(),
		versionCmd(),
	)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version info",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("ringbuzz %s (%s, built %s)\n", version, commit, date)
		},
	}
}

func loginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Log in to Ring (interactive, stores refresh token)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if cfg.HardwareID == "" {
				cfg.HardwareID = ring.NewHardwareID()
			}

			email, err := prompt("Ring email: ")
			if err != nil {
				return err
			}
			password, err := promptPassword("Ring password: ")
			if err != nil {
				return err
			}

			client := ring.New(cfg)
			token, err := client.PasswordLogin(cmd.Context(), email, password, func() (string, error) {
				return prompt("2FA code: ")
			})
			if err != nil {
				return fmt.Errorf("login: %w", err)
			}
			cfg.RefreshToken = token
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Println("OK — refresh token stored.")
			fmt.Println("Next: `ringbuzz devices` then `ringbuzz use <id>`.")
			return nil
		},
	}
}

func devicesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "devices",
		Short: "List Ring devices on the account",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, client, err := loadAuthed()
			if err != nil {
				return err
			}
			devs, err := client.ListDevices(cmd.Context())
			if err != nil {
				return err
			}
			if len(devs) == 0 {
				fmt.Println("no devices found")
				return nil
			}
			for _, d := range devs {
				marker := "  "
				if d.ID == cfg.DeviceID {
					marker = "* "
				}
				fmt.Printf("%s%d\t%s\t%s\n", marker, d.ID, d.Kind, d.Description)
			}
			return nil
		},
	}
}

func useCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <device-id>",
		Short: "Select which Ring device to control",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("device-id must be numeric: %w", err)
			}
			cfg, client, err := loadAuthed()
			if err != nil {
				return err
			}
			devs, err := client.ListDevices(cmd.Context())
			if err != nil {
				return err
			}
			for _, d := range devs {
				if d.ID == id {
					cfg.DeviceID = d.ID
					cfg.DeviceName = d.Description
					if err := cfg.Save(); err != nil {
						return err
					}
					fmt.Printf("using %d (%s)\n", d.ID, d.Description)
					return nil
				}
			}
			return fmt.Errorf("device %d not found on this account", id)
		},
	}
}

func unlockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlock",
		Short: "Fire the unlock once (test)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, client, err := loadAuthed()
			if err != nil {
				return err
			}
			if cfg.DeviceID == 0 {
				return fmt.Errorf("no device selected — run `ringbuzz use <id>` first")
			}
			if err := client.Unlock(cmd.Context(), cfg.DeviceID); err != nil {
				return err
			}
			fmt.Println("unlock sent")
			return nil
		},
	}
}

func pairCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pair",
		Short: "Print the HomeKit setup code",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("Setup code: %s-%s-%s\n", hapPIN[0:3], hapPIN[3:5], hapPIN[5:8])
			fmt.Println("Start `ringbuzz daemon` and add the accessory in the Home app.")
			return nil
		},
	}
}

func daemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "Run the HomeKit server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, client, err := loadAuthed()
			if err != nil {
				return err
			}
			if cfg.DeviceID == 0 {
				return fmt.Errorf("no device selected — run `ringbuzz use <id>` first")
			}

			hapDir, err := config.HAPDir()
			if err != nil {
				return err
			}

			name := cfg.DeviceName
			if name == "" {
				name = "Ring Intercom"
			}

			srv := haprun.New(hapDir, hapPIN, strconv.FormatInt(cfg.DeviceID, 10), name,
				func(ctx context.Context) error {
					return client.Unlock(ctx, cfg.DeviceID)
				})

			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			slog.Info("ringbuzz daemon starting",
				"device_id", cfg.DeviceID,
				"device_name", name,
				"hap_dir", hapDir)

			return srv.Run(ctx)
		},
	}
}

// loadAuthed loads config + constructs a Ring client, refusing to continue
// without a refresh token.
func loadAuthed() (*config.Config, *ring.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	if cfg.RefreshToken == "" {
		return nil, nil, fmt.Errorf("not logged in — run `ringbuzz login`")
	}
	return cfg, ring.New(cfg), nil
}

func prompt(msg string) (string, error) {
	fmt.Print(msg)
	var s string
	if _, err := fmt.Scanln(&s); err != nil {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

func promptPassword(msg string) (string, error) {
	fmt.Print(msg)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
