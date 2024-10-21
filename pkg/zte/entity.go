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
