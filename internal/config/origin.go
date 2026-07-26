package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

func CanonicalOrigin(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return "", errors.New("trusted origin must be an exact HTTPS origin")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil {
		return "", errors.New("trusted origin must be an exact HTTPS origin")
	}
	if !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" || parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("trusted origin must be an exact HTTPS origin")
	}

	host, portText, err := splitOriginHost(parsed.Host)
	if err != nil || host == "" || strings.ContainsAny(host, "*%") {
		return "", errors.New("trusted origin must be an exact HTTPS origin")
	}
	canonicalHost, ipv6, err := canonicalOriginHost(host)
	if err != nil {
		return "", errors.New("trusted origin must be an exact HTTPS origin")
	}

	port := 0
	if portText != "" {
		parsedPort, parseErr := strconv.ParseUint(portText, 10, 16)
		if parseErr != nil || parsedPort == 0 {
			return "", errors.New("trusted origin port must be between 1 and 65535")
		}
		port = int(parsedPort)
	}

	if ipv6 {
		canonicalHost = "[" + canonicalHost + "]"
	}
	canonical := "https://" + canonicalHost
	if port != 0 && port != 443 {
		canonical += ":" + strconv.Itoa(port)
	}
	return canonical, nil
}

func splitOriginHost(hostport string) (host, port string, err error) {
	if strings.HasPrefix(hostport, "[") {
		closing := strings.IndexByte(hostport, ']')
		if closing < 0 {
			return "", "", errors.New("unterminated IPv6 address")
		}
		host = hostport[1:closing]
		remainder := hostport[closing+1:]
		switch {
		case remainder == "":
			return host, "", nil
		case strings.HasPrefix(remainder, ":") && len(remainder) > 1:
			return host, remainder[1:], nil
		default:
			return "", "", errors.New("invalid IPv6 origin")
		}
	}
	if strings.Count(hostport, ":") > 1 {
		return "", "", errors.New("IPv6 origin must use brackets")
	}
	if separator := strings.LastIndexByte(hostport, ':'); separator >= 0 {
		if separator == len(hostport)-1 {
			return "", "", errors.New("empty port")
		}
		return hostport[:separator], hostport[separator+1:], nil
	}
	return hostport, "", nil
}

func canonicalOriginHost(host string) (canonical string, ipv6 bool, err error) {
	if address, parseErr := netip.ParseAddr(host); parseErr == nil {
		return strings.ToLower(address.String()), address.Is6(), nil
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil {
		return "", false, fmt.Errorf("invalid host: %w", err)
	}
	if ascii == "" || strings.Contains(ascii, ":") {
		return "", false, errors.New("invalid host")
	}
	return strings.ToLower(ascii), false, nil
}
