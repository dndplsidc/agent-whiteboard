package common

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"
)

func pathsForHome(home string) servicePaths {
	return servicePaths{
		Plist:     filepath.Join(home, "Library", "LaunchAgents", LaunchAgentLabel+".plist"),
		StdoutLog: filepath.Join(home, ".agent-whiteboard", "logs", "agent.stdout.log"),
		StderrLog: filepath.Join(home, ".agent-whiteboard", "logs", "agent.stderr.log"),
	}
}

func normalizeHome(home string) (string, error) {
	if !validAbsolutePath(home) {
		return "", errors.New("home directory must be an absolute clean path")
	}
	real, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("inspect home directory: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("home directory must be a directory")
	}
	return filepath.Clean(real), nil
}

type normalizedConfig struct {
	Executable          string
	ConfigPath          string
	ProviderExecutables map[string]string
}

// GenerateLaunchAgentPlist validates and resolves all configured paths, then returns the
// deterministic plist for the supplied real user home.
func GenerateLaunchAgentPlist(config LaunchAgentConfig, home string) ([]byte, error) {
	realHome, err := normalizeHome(home)
	if err != nil {
		return nil, err
	}
	normalized, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	return marshalPlist(normalized, pathsForHome(realHome))
}

func normalizeConfig(config LaunchAgentConfig) (normalizedConfig, error) {
	executable, err := resolveRegularPath(config.Executable, true, false)
	if err != nil {
		return normalizedConfig{}, fmt.Errorf("validate agent-whiteboard executable: %w", err)
	}
	configuration, err := resolveConfigPath(config.ConfigPath)
	if err != nil {
		return normalizedConfig{}, fmt.Errorf("validate configuration path: %w", err)
	}
	providers, err := resolveProviderExecutables(config.Providers, config.ExecutableResolver)
	if err != nil {
		return normalizedConfig{}, err
	}
	return normalizedConfig{
		Executable:          executable,
		ConfigPath:          configuration,
		ProviderExecutables: providers,
	}, nil
}

func resolveProviderExecutables(descriptors []LaunchAgentProviderDescriptor, resolver LaunchAgentExecutableResolver) (map[string]string, error) {
	providers := make(map[string]string, len(descriptors))
	seen := make(map[string]struct{}, len(descriptors))
	if len(descriptors) == 0 {
		return providers, nil
	}
	if resolver == nil {
		resolver = pathResolver{}
	}
	for _, descriptor := range descriptors {
		if isNilInterface(descriptor) {
			return nil, errors.New("provider descriptor is required")
		}
		name, executableName := descriptor.ProviderName(), descriptor.ExecutableName()
		if (name != LaunchAgentProviderPi && name != LaunchAgentProviderCodex) || executableName != name {
			return nil, fmt.Errorf("unsupported provider executable descriptor %q", name)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("duplicate provider executable descriptor %q", name)
		}
		seen[name] = struct{}{}
		path, err := resolver.LookPath(executableName)
		if err != nil {
			if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("resolve %s provider executable: %w", name, err)
		}
		if !filepath.IsAbs(path) {
			path, err = filepath.Abs(path)
			if err != nil {
				return nil, fmt.Errorf("make %s provider executable absolute: %w", name, err)
			}
		}
		resolved, err := resolveRegularPath(filepath.Clean(path), true, false)
		if err != nil {
			return nil, fmt.Errorf("validate %s provider executable: %w", name, err)
		}
		providers[name] = resolved
	}
	return providers, nil
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func resolveConfigPath(path string) (string, error) {
	if !validAbsolutePath(path) {
		return "", errors.New("path must be absolute and clean")
	}
	if _, err := os.Lstat(path); err == nil {
		return resolveRegularPath(path, false, true)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	ancestor := filepath.Dir(path)
	missing := []string{filepath.Base(path)}
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			real, err := filepath.EvalSymlinks(ancestor)
			if err != nil {
				return "", err
			}
			canonicalAncestor, err := resolveTopLevelAlias(ancestor)
			if err != nil {
				return "", err
			}
			if filepath.Clean(real) != canonicalAncestor {
				return "", errors.New("missing configuration path must not traverse symlinked ancestors")
			}
			info, err := os.Stat(real)
			if err != nil {
				return "", err
			}
			if !info.IsDir() {
				return "", errors.New("existing configuration parent must be a directory")
			}
			resolved := filepath.Join(append([]string{filepath.Clean(real)}, missing...)...)
			if !validAbsolutePath(resolved) {
				return "", errors.New("resolved path must be absolute and clean")
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", err
		}
		missing = append([]string{filepath.Base(ancestor)}, missing...)
		ancestor = parent
	}
}

func resolveTopLevelAlias(path string) (string, error) {
	volume := filepath.VolumeName(path)
	relative := strings.TrimPrefix(path, volume+string(filepath.Separator))
	first, remainder, _ := strings.Cut(relative, string(filepath.Separator))
	top := filepath.Join(volume+string(filepath.Separator), first)
	resolvedTop, err := filepath.EvalSymlinks(top)
	if err != nil {
		return "", err
	}
	if remainder == "" {
		return filepath.Clean(resolvedTop), nil
	}
	return filepath.Clean(filepath.Join(resolvedTop, remainder)), nil
}

func resolveRegularPath(path string, executable, private bool) (string, error) {
	if !validAbsolutePath(path) {
		return "", errors.New("path must be absolute and clean")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	real = filepath.Clean(real)
	if !validAbsolutePath(real) {
		return "", errors.New("resolved path must be absolute and clean")
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("path must identify a regular file")
	}
	if executable {
		if err := currentUserExecutable(real); err != nil {
			return "", err
		}
	}
	if private && info.Mode().Perm()&0o022 != 0 {
		return "", errors.New("path must not be writable by group or others")
	}
	return real, nil
}

func validAbsolutePath(path string) bool {
	if path == "" || !utf8.ValidString(path) || strings.ContainsRune(path, '\x00') || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	for _, character := range path {
		if character < 0x20 {
			return false
		}
	}
	return true
}

func marshalPlist(config normalizedConfig, paths servicePaths) ([]byte, error) {
	for _, path := range []string{config.Executable, config.ConfigPath, paths.Plist, paths.StdoutLog, paths.StderrLog} {
		if !validAbsolutePath(path) {
			return nil, errors.New("plist contains an invalid path")
		}
	}

	arguments := []string{config.Executable, "--config", config.ConfigPath, "agent", "serve"}
	environment := make(map[string]string, len(config.ProviderExecutables))
	for provider, path := range config.ProviderExecutables {
		if (provider != LaunchAgentProviderPi && provider != LaunchAgentProviderCodex) || !validAbsolutePath(path) {
			return nil, errors.New("plist contains an invalid provider executable")
		}
		key := LaunchAgentPiExecutableEnvironment
		if provider == LaunchAgentProviderCodex {
			key = LaunchAgentCodexExecutableEnvironment
		}
		environment[key] = path
	}

	var output bytes.Buffer
	output.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	output.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	output.WriteString("<plist version=\"1.0\">\n<dict>\n")
	writePlistString(&output, "LaunchAgentLabel", LaunchAgentLabel, 1)
	writePlistArray(&output, "ProgramArguments", arguments, 1)
	writePlistBool(&output, "RunAtLoad", true, 1)
	writePlistBool(&output, "KeepAlive", true, 1)
	writePlistString(&output, "StandardOutPath", paths.StdoutLog, 1)
	writePlistString(&output, "StandardErrorPath", paths.StderrLog, 1)
	if len(environment) > 0 {
		output.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		keys := make([]string, 0, len(environment))
		for key := range environment {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			writePlistString(&output, key, environment[key], 2)
		}
		output.WriteString("  </dict>\n")
	}
	output.WriteString("</dict>\n</plist>\n")
	return output.Bytes(), nil
}

func writePlistString(output *bytes.Buffer, key, value string, indentation int) {
	indent := strings.Repeat("  ", indentation)
	output.WriteString(indent + "<key>")
	_ = xml.EscapeText(output, []byte(key))
	output.WriteString("</key>\n" + indent + "<string>")
	_ = xml.EscapeText(output, []byte(value))
	output.WriteString("</string>\n")
}

func writePlistArray(output *bytes.Buffer, key string, values []string, indentation int) {
	indent := strings.Repeat("  ", indentation)
	output.WriteString(indent + "<key>")
	_ = xml.EscapeText(output, []byte(key))
	output.WriteString("</key>\n" + indent + "<array>\n")
	for _, value := range values {
		valueIndent := strings.Repeat("  ", indentation+1)
		output.WriteString(valueIndent + "<string>")
		_ = xml.EscapeText(output, []byte(value))
		output.WriteString("</string>\n")
	}
	output.WriteString(indent + "</array>\n")
}

func writePlistBool(output *bytes.Buffer, key string, value bool, indentation int) {
	indent := strings.Repeat("  ", indentation)
	output.WriteString(indent + "<key>")
	_ = xml.EscapeText(output, []byte(key))
	if value {
		output.WriteString("</key>\n" + indent + "<true/>\n")
	} else {
		output.WriteString("</key>\n" + indent + "<false/>\n")
	}
}
