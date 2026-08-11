package protocol

// StatusResponse is the complete disclosure permitted by the unauthenticated
// local status endpoint.
type StatusResponse struct {
	Available     bool   `json:"available"`
	APIVersion    string `json:"api_version"`
	OriginTrusted bool   `json:"origin_trusted"`
}

func NewStatusResponse(originTrusted bool) StatusResponse {
	return StatusResponse{Available: true, APIVersion: APIVersion, OriginTrusted: originTrusted}
}
