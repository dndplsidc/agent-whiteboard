package common

import (
	"net/url"
	"strconv"
	"strings"
)

// ValidPageURL accepts ordinary HTTPS page URLs and canonical HTTP URLs served
// from literal IPv4 loopback with no explicit port or a non-default port.
func ValidPageURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	if parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" {
		return false
	}
	if parsed.Host == "127.0.0.1" {
		return true
	}
	const prefix = "127.0.0.1:"
	if !strings.HasPrefix(parsed.Host, prefix) {
		return false
	}
	portText := strings.TrimPrefix(parsed.Host, prefix)
	port, err := strconv.ParseUint(portText, 10, 16)
	return err == nil && port != 0 && port != 80 && strconv.FormatUint(port, 10) == portText
}
