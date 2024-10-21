package zte

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-resty/resty/v2"
	"go.uber.org/zap"
)

const (
	APIBase = "/goform/"
	GetCmd  = "goform_get_cmd_process"
	SetCmd  = "goform_set_cmd_process"
)

// Connection manages a ZTE MC888 device connection.
type Connection struct {
	logger   *zap.Logger
	client   *resty.Client
	referer  string
	password string

	crVersion      string
	waInnerVersion string
	cookie         *http.Cookie
}

// NewConnection initializes a new Connection instance.
func NewConnection(logger *zap.Logger, host, password string) *Connection {
	return &Connection{
		logger:   logger,
		client:   resty.New().SetBaseURL("http://" + host),
		referer:  "http://" + host + "/",
		password: password,
	}
}

// Login logs in to the ZTE device.
func (c *Connection) Login() error {
	if err := c.parseDeviceVersion(); err != nil {
		return fmt.Errorf("failed to login: %w", err)
	}
	ld, err := c.getLD()
	if err != nil {
		return fmt.Errorf("failed to login: %w", err)
	}
	cookie, err := c.loginRequest(c.calculatePassword(ld))
	if err != nil {
		return fmt.Errorf("failed to login: %w", err)
	}
	c.cookie = cookie
	return nil
}

// Logout logs out from the ZTE device.
func (c *Connection) Logout() error {
	rd, err := c.getRD()
	if err != nil {
		return fmt.Errorf("failed to logout: %w", err)
	}
	return c.logoutRequest(c.calculateAD(rd))
}

// parseDeviceVersion parses the ZTE device version.
func (c *Connection) parseDeviceVersion() error {
	deviceVersion := &DeviceVersionResponse{}
	resp, err := c.client.R().
		SetHeader("Referer", c.referer).
		SetQueryParams(map[string]string{
			"isTest":     "false",
			"cmd":        "Language,cr_version,wa_inner_version",
			"multi_data": "1",
		}).
		SetResult(deviceVersion).
		ForceContentType("application/json").
		Get(APIBase + GetCmd)
	if err != nil {
		return fmt.Errorf("get device version failed: %w", err)
	}
	if resp.StatusCode() != http.StatusOK {
		return fmt.Errorf("get device version failed: %s", resp.Status())
	}

	c.crVersion = deviceVersion.CrVersion
	c.waInnerVersion = deviceVersion.WaInnerVersion
	return nil
}

// getLD retrieves the LD value from the ZTE device.
func (c *Connection) getLD() (string, error) {
	ld := &LDResponse{}
	if err := c.getXD(ld, "LD"); err != nil {
		return "", fmt.Errorf("get LD failed: %w", err)
	}
	return ld.LD, nil
}

// getXD is a generic logic to get LD or RD.
func (c *Connection) getXD(result interface{}, cmd string) error {
	resp, err := c.client.R().
		SetHeader("Referer", c.referer).
		SetQueryParams(map[string]string{
			"isTest": "false",
			"cmd":    cmd,
		}).
		SetResult(result).
		ForceContentType("application/json").
		Get(APIBase + GetCmd)
	if err != nil {
		return fmt.Errorf("get %s failed: %w", cmd, err)
	}
	if resp.StatusCode() != http.StatusOK {
		return fmt.Errorf("get %s failed: %s", cmd, resp.Status())
	}
	return nil
}

// getRD retrieves the RD value from the ZTE device.
func (c *Connection) getRD() (string, error) {
	rd := &RDResponse{}
	if err := c.getXD(rd, "RD"); err != nil {
		return "", fmt.Errorf("get RD failed: %w", err)
	}
	return rd.RD, nil
}

// calculatePassword generates the password hash based on the LD value.
func (c *Connection) calculatePassword(ld string) string {
	prefixHash := sha256.Sum256([]byte(c.password))
	prefixHashHex := strings.ToUpper(hex.EncodeToString(prefixHash[:]))
	finalHash := sha256.Sum256([]byte(prefixHashHex + strings.ToUpper(ld)))
	return strings.ToUpper(hex.EncodeToString(finalHash[:]))
}

// calculateAD generates the AD hash for login.
func (c *Connection) calculateAD(rd string) string {
	prefixHash := sha256.Sum256([]byte(c.waInnerVersion + c.crVersion))
	prefixHashHex := strings.ToUpper(hex.EncodeToString(prefixHash[:]))
	finalHash := sha256.Sum256([]byte(prefixHashHex + strings.ToUpper(rd)))
	return strings.ToUpper(hex.EncodeToString(finalHash[:]))
}

// loginRequest sends a login request to the ZTE device.
func (c *Connection) loginRequest(password string) (*http.Cookie, error) {
	result := &Response{}
	resp, err := c.client.R().
		SetHeaders(map[string]string{
			"Origin":  c.referer,
			"Referer": c.referer,
		}).
		SetFormData(map[string]string{
			"isTest":   "false",
			"goformId": "LOGIN",
			"password": password,
		}).
		SetResult(result).
		ForceContentType("application/json").
		Post(APIBase + SetCmd)
	if err != nil {
		return nil, fmt.Errorf("login failed: %w", err)
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("login failed: %s", resp.Status())
	}
	if result.Result != "0" {
		return nil, fmt.Errorf("login failed: %s", result.Result)
	}

	// Get auth cookie.
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "stok" {
			return cookie, nil
		}
	}
	return nil, fmt.Errorf("login failed: no cookies found")
}

// logoutRequest sends a logout request to the ZTE device.
func (c *Connection) logoutRequest(ad string) error {
	result := &Response{}
	resp, err := c.client.R().
		SetHeaders(map[string]string{
			"Origin":  c.referer,
			"Referer": c.referer,
		}).
		SetCookie(c.cookie).
		SetFormData(map[string]string{
			"isTest":   "false",
			"goformId": "LOGOUT",
			"AD":       ad,
		}).
		SetResult(result).
		ForceContentType("application/json").
		Post(APIBase + SetCmd)
	if err != nil {
		return fmt.Errorf("logout failed: %w", err)
	}
	if resp.StatusCode() != http.StatusOK {
		return fmt.Errorf("logout failed: %s", resp.Status())
	}
	if result.Result != "success" {
		return fmt.Errorf("logout failed: %s", result.Result)
	}
	return nil
}
