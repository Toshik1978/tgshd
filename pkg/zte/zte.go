package zte

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-resty/resty/v2"
	"go.uber.org/zap"
)

const (
	GetCmd  = "/goform/goform_get_cmd_process"
	SetCmd  = "/goform/goform_set_cmd_process"
	Success = "success"
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
		client:   resty.New().SetBaseURL("http://" + host).SetLogger(newLogger(logger)),
		referer:  "http://" + host + "/",
		password: password,
	}
}

// Login logs in to the ZTE device.
func (c *Connection) Login() error {
	if err := c.parseDeviceVersion(); err != nil {
		return fmt.Errorf("failed to login: %w", err)
	}
	ld, err := c.ld()
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
	ad, err := c.ad()
	if err != nil {
		return fmt.Errorf("failed to logout: %w", err)
	}
	return c.logoutRequest(ad)
}

// SetBearer sets the bearer preferences for the network.
func (c *Connection) SetBearer(pref Bearer) error {
	ad, err := c.ad()
	if err != nil {
		return fmt.Errorf("failed to set bearer: %w", err)
	}
	return c.bearerRequest(pref, ad)
}

// AllSms gets all SMS on the device.
func (c *Connection) AllSms(onlyUnread bool) ([]Sms, error) {
	messages, err := c.smsRequest(0, 500)
	if err != nil {
		return nil, fmt.Errorf("failed to get all sms: %w", err)
	}
	if len(messages.Messages) == 0 {
		return nil, nil
	}

	l := make([]Sms, 0, len(messages.Messages))
	for _, m := range messages.Messages { //nolint:gocritic
		if m.ReceivedAllConcatSms == "1" && (!onlyUnread || m.Tag == "1") {
			l = append(l, m)
		}
	}
	return l, nil
}

// ReadSms gets all unread SMS on the device and mark read/delete them.
//
// Method always returns the list of SMS if we could read them.
// But it can have an error at the same time if we didn't read/delete successfully.
func (c *Connection) ReadSms(del bool) ([]Sms, error) {
	messages, err := c.smsRequest(0, 500)
	if err != nil {
		return nil, fmt.Errorf("failed to get all sms: %w", err)
	}
	if len(messages.Messages) == 0 {
		return nil, nil
	}

	// Collect all unread SMSs.
	ids := make([]string, 0, len(messages.Messages))
	l := make([]Sms, 0, len(messages.Messages))
	for _, m := range messages.Messages { //nolint:gocritic
		if m.ReceivedAllConcatSms == "1" && m.Tag == "1" {
			ids = append(ids, m.ID)
			l = append(l, m)
		}
	}

	// Mark them read or delete.
	ad, err := c.ad()
	if err != nil {
		return nil, fmt.Errorf("failed to read/delete sms: %w", err)
	}
	if del {
		return l, c.smsDeleteRequest(ids, ad)
	}
	return l, c.smsReadRequest(ids, ad)
}

// parseDeviceVersion parses the ZTE device version.
func (c *Connection) parseDeviceVersion() error {
	deviceVersion := &DeviceVersionResponse{}
	r, err := c.request(false).
		SetQueryParams(map[string]string{
			"cmd":        "Language,cr_version,wa_inner_version",
			"multi_data": "1",
		}).
		SetResult(deviceVersion).
		Get(GetCmd)
	if err1 := c.checkError(err, r, "get device version"); err1 != nil {
		return err1
	}

	c.crVersion = deviceVersion.CrVersion
	c.waInnerVersion = deviceVersion.WaInnerVersion
	return nil
}

// ad calculates the current AD value.
func (c *Connection) ad() (string, error) {
	if c.crVersion == "" && c.waInnerVersion == "" {
		return "", fmt.Errorf("not logged in")
	}
	rd, err := c.rd()
	if err != nil {
		return "", fmt.Errorf("failed to get RD value: %w", err)
	}
	return c.calculateAD(rd), nil
}

// ld retrieves the LD value from the ZTE device.
func (c *Connection) ld() (string, error) {
	ld := &LDResponse{}
	if err := c.xd(ld, "LD"); err != nil {
		return "", fmt.Errorf("get LD failed: %w", err)
	}
	return ld.LD, nil
}

// rd retrieves the RD value from the ZTE device.
func (c *Connection) rd() (string, error) {
	rd := &RDResponse{}
	if err := c.xd(rd, "RD"); err != nil {
		return "", fmt.Errorf("get RD failed: %w", err)
	}
	return rd.RD, nil
}

// xd is a generic logic to get LD or RD.
func (c *Connection) xd(result interface{}, cmd string) error {
	r, err := c.request(false).
		SetQueryParam("cmd", cmd).
		SetResult(result).
		Get(GetCmd)
	if err1 := c.checkError(err, r, fmt.Sprintf("get %s", cmd)); err1 != nil {
		return err1
	}
	return nil
}

// calculatePassword generates the password hash based on the LD value.
func (c *Connection) calculatePassword(ld string) string {
	prefixHash := sha256.Sum256([]byte(c.password))
	prefixHashHex := strings.ToUpper(hex.EncodeToString(prefixHash[:]))
	finalHash := sha256.Sum256([]byte(prefixHashHex + strings.ToUpper(ld)))
	return strings.ToUpper(hex.EncodeToString(finalHash[:]))
}

// calculateAD generates the AD hash for API based on the RD value.
func (c *Connection) calculateAD(rd string) string {
	prefixHash := sha256.Sum256([]byte(c.waInnerVersion + c.crVersion))
	prefixHashHex := strings.ToUpper(hex.EncodeToString(prefixHash[:]))
	finalHash := sha256.Sum256([]byte(prefixHashHex + strings.ToUpper(rd)))
	return strings.ToUpper(hex.EncodeToString(finalHash[:]))
}

// loginRequest sends a login request to the ZTE device.
func (c *Connection) loginRequest(hash string) (*http.Cookie, error) {
	result := &Response{}
	r, err := c.request(true).
		SetFormData(map[string]string{
			"goformId": "LOGIN",
			"password": hash,
		}).
		SetResult(result).
		Post(SetCmd)
	if err1 := c.checkError(err, r, "login"); err1 != nil {
		return nil, err1
	}
	if result.Result != "0" {
		return nil, fmt.Errorf("login failed: %s", result.Result)
	}

	// Get auth cookie.
	for _, cookie := range r.Cookies() {
		if cookie.Name == "stok" {
			return cookie, nil
		}
	}
	return nil, fmt.Errorf("login failed: no cookies found")
}

// logoutRequest sends a logout request to the ZTE device.
func (c *Connection) logoutRequest(ad string) error {
	result := &Response{}
	r, err := c.request(true).
		SetFormData(map[string]string{
			"goformId": "LOGOUT",
			"AD":       ad,
		}).
		SetResult(result).
		Post(SetCmd)
	if err1 := c.checkError(err, r, "logout"); err1 != nil {
		return err1
	}
	if result.Result != Success {
		return fmt.Errorf("logout failed: %s", result.Result)
	}
	return nil
}

// bearerRequest sends a bearer request to the ZTE device.
func (c *Connection) bearerRequest(pref Bearer, ad string) error {
	result := &Response{}
	r, err := c.request(true).
		SetFormData(map[string]string{
			"goformId":         "SET_BEARER_PREFERENCE",
			"BearerPreference": string(pref),
			"AD":               ad,
		}).
		SetResult(result).
		Post(SetCmd)
	if err1 := c.checkError(err, r, "bearer"); err1 != nil {
		return err1
	}
	if result.Result != Success {
		return fmt.Errorf("bearer failed: %s", result.Result)
	}
	return nil
}

// smsRequest gets all SMSs.
func (c *Connection) smsRequest(page, size int) (*SmsList, error) {
	result := &SmsList{}
	r, err := c.request(false).
		SetQueryParam("cmd", "sms_data_total").
		SetQueryParam("page", strconv.Itoa(page)).
		SetQueryParam("data_per_page", strconv.Itoa(size)).
		SetQueryParam("mem_store", "1").
		SetQueryParam("tags", "10").
		SetQueryParam("order_by", "order+by+id+desc").
		SetResult(result).
		Get(GetCmd)
	if err1 := c.checkError(err, r, "get sms list"); err1 != nil {
		return nil, err1
	}
	return result, nil
}

// smsReadRequest marks SMSs as READ.
func (c *Connection) smsReadRequest(ids []string, ad string) error {
	if len(ids) == 0 {
		return nil
	}

	result := &Response{}
	r, err := c.request(true).
		SetFormData(map[string]string{
			"goformId": "SET_MSG_READ",
			"msg_id":   strings.Join(ids, ";") + ";",
			"tag":      "0",
			"AD":       ad,
		}).
		SetResult(result).
		Post(SetCmd)
	if err1 := c.checkError(err, r, "read sms"); err1 != nil {
		return err1
	}
	if result.Result != Success {
		return fmt.Errorf("read sms failed: %s", result.Result)
	}
	return nil
}

// smsDeleteRequest delete SMSs.
func (c *Connection) smsDeleteRequest(ids []string, ad string) error {
	if len(ids) == 0 {
		return nil
	}

	result := &Response{}
	r, err := c.request(true).
		SetFormData(map[string]string{
			"goformId": "DELETE_SMS",
			"msg_id":   strings.Join(ids, ";") + ";",
			"AD":       ad,
		}).
		SetResult(result).
		Post(SetCmd)
	if err1 := c.checkError(err, r, "delete sms"); err1 != nil {
		return err1
	}
	if result.Result != Success {
		return fmt.Errorf("delete sms failed: %s", result.Result)
	}
	return nil
}

// request generates the basic resty request object.
func (c *Connection) request(post bool) *resty.Request {
	r := c.client.R().
		ForceContentType("application/json").
		SetHeader("Origin", c.referer).
		SetHeader("Referer", c.referer)

	if post {
		r = r.SetFormData(map[string]string{"isTest": "false"})
	} else {
		r = r.SetQueryParam("isTest", "false")
	}

	if c.cookie != nil {
		r = r.SetCookie(c.cookie)
	}

	return r
}

func (c *Connection) checkError(err error, r *resty.Response, prefix string) error {
	if err != nil {
		return fmt.Errorf("%s failed: %w", prefix, err)
	}
	if r.StatusCode() != http.StatusOK {
		return fmt.Errorf("%s failed: %s", prefix, r.Status())
	}
	return nil
}
