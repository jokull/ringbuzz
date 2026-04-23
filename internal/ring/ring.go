// Package ring is a tiny Ring-cloud API client supporting exactly what
// ringbuzz needs: OAuth login (with 2FA), device listing, and intercom unlock.
//
// Wire format mirrors dgreif/ring-client-api (MIT); see rest-client.ts and
// ring-intercom.ts in that repo for the authoritative shapes.
package ring

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jokull/ringbuzz/internal/config"
)

const (
	oauthURL      = "https://oauth.ring.com/oauth/token"
	devicesURL    = "https://api.ring.com/clients_api/ring_devices"
	unlockTmpl    = "https://api.ring.com/commands/v1/devices/%d/device_rpc"
	ringClientID  = "ring_official_android"
	ringScope     = "client"
	ringUserAgent = "android:com.ringapp"
)

// NewHardwareID returns a random UUIDv4 hex string. Ring just needs a stable
// identifier per client; the exact format isn't validated.
func NewHardwareID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to time-based — extremely unlikely to trigger.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

type Device struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

type Client struct {
	cfg  *config.Config
	http *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

func New(cfg *config.Config) *Client {
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// ---------- auth ----------

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
}

// Prompt2FA is called when Ring responds 412 on password login. Return the
// SMS/email/TOTP code the user just received.
type Prompt2FA func() (string, error)

// PasswordLogin performs the initial OAuth password grant, handling 2FA.
// Returns the refresh token (which caller should persist). Also populates
// the client's in-memory access token so subsequent calls work immediately.
func (c *Client) PasswordLogin(ctx context.Context, email, password string, prompt Prompt2FA) (string, error) {
	if c.cfg.HardwareID == "" {
		c.cfg.HardwareID = NewHardwareID()
	}

	body := map[string]any{
		"grant_type": "password",
		"username":   email,
		"password":   password,
		"client_id":  ringClientID,
		"scope":      ringScope,
	}

	tok, err := c.tokenRequest(ctx, body, "")
	if errors.Is(err, err2FARequired) {
		code, perr := prompt()
		if perr != nil {
			return "", perr
		}
		tok, err = c.tokenRequest(ctx, body, code)
	}
	if err != nil {
		return "", err
	}

	c.setToken(tok)
	return tok.RefreshToken, nil
}

// refresh exchanges the stored refresh token for a fresh access token.
// Rotates the stored refresh token if Ring returns a new one.
func (c *Client) refresh(ctx context.Context) error {
	if c.cfg.RefreshToken == "" {
		return fmt.Errorf("no refresh token")
	}
	body := map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": c.cfg.RefreshToken,
		"client_id":     ringClientID,
		"scope":         ringScope,
	}
	tok, err := c.tokenRequest(ctx, body, "")
	if err != nil {
		return err
	}
	c.setToken(tok)

	if tok.RefreshToken != "" && tok.RefreshToken != c.cfg.RefreshToken {
		c.cfg.RefreshToken = tok.RefreshToken
		if err := c.cfg.Save(); err != nil {
			slog.Warn("failed to persist rotated refresh token", "error", err)
		}
	}
	return nil
}

var err2FARequired = errors.New("2fa required")

func (c *Client) tokenRequest(ctx context.Context, body map[string]any, twoFACode string) (*tokenResponse, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthURL, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ringUserAgent)
	req.Header.Set("hardware_id", c.cfg.HardwareID)
	req.Header.Set("2fa-support", "true")
	req.Header.Set("2fa-code", twoFACode)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var t tokenResponse
		if err := json.Unmarshal(data, &t); err != nil {
			return nil, fmt.Errorf("decode token response: %w", err)
		}
		return &t, nil
	case http.StatusPreconditionFailed: // 412 — 2FA challenge
		return nil, err2FARequired
	case http.StatusBadRequest:
		// Usually wrong 2FA code or bad credentials.
		return nil, fmt.Errorf("ring auth rejected: %s", strings.TrimSpace(string(data)))
	default:
		return nil, fmt.Errorf("ring auth: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
}

func (c *Client) setToken(t *tokenResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accessToken = t.AccessToken
	// Refresh 60s early.
	c.expiresAt = time.Now().Add(time.Duration(t.ExpiresIn-60) * time.Second)
}

func (c *Client) ensureAccessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	tok := c.accessToken
	exp := c.expiresAt
	c.mu.Unlock()
	if tok != "" && time.Now().Before(exp) {
		return tok, nil
	}
	if err := c.refresh(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.accessToken, nil
}

// ---------- authenticated requests ----------

func (c *Client) do(ctx context.Context, method, url string, body any) (*http.Response, []byte, error) {
	tok, err := c.ensureAccessToken(ctx)
	if err != nil {
		return nil, nil, err
	}

	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("User-Agent", ringUserAgent)
	req.Header.Set("hardware_id", c.cfg.HardwareID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data, nil
}

// ---------- public API ----------

// ringDevicesResponse matches the `ring_devices` endpoint. Ring sorts devices
// by category; intercoms live in the "other" bucket.
type ringDevicesResponse struct {
	Doorbots           []Device `json:"doorbots"`
	AuthorizedDoorbots []Device `json:"authorized_doorbots"`
	StickupCams        []Device `json:"stickup_cams"`
	Chimes             []Device `json:"chimes"`
	Other              []Device `json:"other"`
}

// ListDevices returns every Ring device on the account, flattened. The CLI
// prints Kind so the user can identify the intercom (`intercom_handset_audio`
// or `intercom_handset_video`).
func (c *Client) ListDevices(ctx context.Context) ([]Device, error) {
	resp, data, err := c.do(ctx, http.MethodGet, devicesURL, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list devices: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var r ringDevicesResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode devices: %w", err)
	}
	var out []Device
	out = append(out, r.Doorbots...)
	out = append(out, r.AuthorizedDoorbots...)
	out = append(out, r.StickupCams...)
	out = append(out, r.Chimes...)
	out = append(out, r.Other...)
	return out, nil
}

// Unlock triggers the intercom unlock via the device_rpc command.
// Wire format mirrors ring-intercom.ts#unlock (PUT, JSON-RPC 2.0 body).
func (c *Client) Unlock(ctx context.Context, deviceID int64) error {
	body := map[string]any{
		"command_name": "device_rpc",
		"request": map[string]any{
			"jsonrpc": "2.0",
			"method":  "unlock_door",
			"params": map[string]any{
				"door_id": 0,
				"user_id": 0,
			},
		},
	}
	url := fmt.Sprintf(unlockTmpl, deviceID)
	resp, data, err := c.do(ctx, http.MethodPut, url, body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unlock: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}
