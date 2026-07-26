package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultConfigSuffix = ".agent-whiteboard/config.yaml"

func DefaultPath() (string, error) {
	return ResolvePath("~/" + defaultConfigSuffix)
}

func ResolvePath(path string) (string, error) {
	return resolvePathFrom(path, "")
}

func resolvePathFrom(path, relativeTo string) (string, error) {
	if path == "" {
		return "", errors.New("path is required")
	}
	if strings.HasPrefix(path, "~") {
		if path != "~" && !strings.HasPrefix(path, "~/") {
			return "", errors.New("named-user home expansion is not supported")
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if home == "" {
			return "", errors.New("resolve home directory: empty path")
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, filepath.FromSlash(strings.TrimPrefix(path, "~/")))
		}
	} else if !filepath.IsAbs(path) && relativeTo != "" {
		path = filepath.Join(relativeTo, path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("make path absolute: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func Builtins() (BuiltinValues, error) {
	home, err := ResolvePath("~")
	if err != nil {
		return BuiltinValues{}, err
	}
	return BuiltinValues{
		Client: ClientValues{
			Server:  "http://127.0.0.1:8567",
			Timeout: 30 * time.Second,
		},
		Server: ServerValues{
			Host:                 "127.0.0.1",
			Port:                 8567,
			Storage:              filepath.Join(home, ".agent-whiteboard"),
			CleanupInterval:      15 * time.Minute,
			DefaultExpiresIn:     86400,
			ShutdownTimeout:      10 * time.Second,
			LogMode:              "console",
			MaxWhiteboardBytes:   10 << 20,
			MaxContextBytes:      DefaultMaxContextBytes,
			MaxImageBytes:        25 << 20,
			MaxImageRequestBytes: 100 << 20,
		},
		Viewer: ViewerValues{LocalAgent: LocalAgentValues{Enabled: false}},
		Agent: AgentValues{
			Port:                8568,
			TrustedOrigins:      []string{},
			ProviderIdleTimeout: 60 * time.Minute,
			ShutdownTimeout:     10 * time.Second,
			DefaultAccess:       AccessContentOnly,
		},
	}, nil
}
