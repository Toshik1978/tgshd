package zte

// DeviceVersionResponse define the device version response.
type DeviceVersionResponse struct {
	CrVersion      string `json:"cr_version"`
	WaInnerVersion string `json:"wa_inner_version"`
}

// LDResponse define the LD command response.
type LDResponse struct {
	LD string `json:"LD"`
}

// RDResponse define the RD command response.
type RDResponse struct {
	RD string `json:"RD"`
}

// Response is generic ZTE response.
type Response struct {
	Result string `json:"result"`
}

// Bearer is a type of network.
type Bearer string

//nolint:gosec // G101 false positive: these are network bearer names, not hardcoded credentials.
const (
	BearerWLAnd5G     Bearer = "WL_AND_5G"     // 5G/4G/3G
	BearerLTEAnd5G    Bearer = "LTE_AND_5G"    // 5G NSA
	BearerOnly5G      Bearer = "Only_5G"       // 5G SA
	BearerWCDMAAndLTE Bearer = "WCDMA_AND_LTE" // 4G/3G
	BearerOnlyLTE     Bearer = "Only_LTE"      // 4G Only
	BearerOnlyWCDMA   Bearer = "Only_WCDMA"    // 3G Only
)

// Sms is an SMS object.
type Sms struct {
	ID                   string `json:"id"`
	Number               string `json:"number"`
	Content              string `json:"content"`
	Tag                  string `json:"tag"`
	Date                 string `json:"date"`
	DraftGroupID         string `json:"draft_group_id"`
	ReceivedAllConcatSms string `json:"received_all_concat_sms"`
	ConcatSmsTotal       string `json:"concat_sms_total"`
	ConcatSmsReceived    string `json:"concat_sms_received"`
}

// SmsList is a list of SMS.
type SmsList struct {
	Messages []Sms `json:"messages"`
}
