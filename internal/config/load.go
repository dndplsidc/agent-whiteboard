package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

func Load(selectedPath string) (Config, error) {
	explicit := selectedPath != ""
	path := selectedPath
	var err error
	if explicit {
		path, err = ResolvePath(path)
	} else {
		path, err = DefaultPath()
	}
	if err != nil {
		return Config{}, fmt.Errorf("resolve configuration path: %w", err)
	}

	file, err := os.Open(path)
	if err != nil {
		if !explicit && errors.Is(err, os.ErrNotExist) {
			return Config{path: path, version: Version1}, nil
		}
		return Config{}, fmt.Errorf("read configuration %q: %w", path, err)
	}
	defer file.Close()

	_, loaded, err := parseConfigFile(file, path)
	if err != nil {
		return Config{}, err
	}
	return loaded, nil
}

func decodeDocument(reader io.Reader) (*yaml.Node, error) {
	decoder := yaml.NewDecoder(reader)
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("configuration must contain one mapping document")
		}
		return nil, err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err == nil {
		return nil, errors.New("configuration must contain exactly one document")
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("configuration document must be a mapping")
	}
	root := document.Content[0]
	if err := validateYAMLStructure(root); err != nil {
		return nil, err
	}
	return root, nil
}

func validateYAMLStructure(node *yaml.Node) error {
	if node == nil {
		return errors.New("invalid empty YAML node")
	}
	if node.Kind == yaml.AliasNode {
		return errors.New("YAML aliases are not supported")
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]struct{}, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return errors.New("mapping keys must be strings")
			}
			if key.Tag == "!!merge" || key.Value == "<<" {
				return errors.New("YAML merge keys are not supported")
			}
			if _, duplicate := seen[key.Value]; duplicate {
				return fmt.Errorf("duplicate field %q", key.Value)
			}
			seen[key.Value] = struct{}{}
		}
	}
	for _, child := range node.Content {
		if err := validateYAMLStructure(child); err != nil {
			return err
		}
	}
	return nil
}

func parseConfig(root *yaml.Node, configDir string, loaded *Config) error {
	fields, err := mappingFields(root, "configuration")
	if err != nil {
		return err
	}
	if err := rejectUnknown(fields, "configuration", "version", "client", "server", "viewer", "agent"); err != nil {
		return err
	}
	versionNode, found := fields["version"]
	if !found {
		return errors.New("version is required")
	}
	version, err := integer(versionNode, "version")
	if err != nil {
		return err
	}
	if version != Version1 {
		return fmt.Errorf("unsupported configuration version %d", version)
	}
	if node := fields["client"]; node != nil {
		if err := parseClient(node, &loaded.client); err != nil {
			return err
		}
	}
	if node := fields["server"]; node != nil {
		if err := parseServer(node, configDir, &loaded.server); err != nil {
			return err
		}
	}
	if node := fields["viewer"]; node != nil {
		if err := parseViewer(node, &loaded.viewer); err != nil {
			return err
		}
	}
	if node := fields["agent"]; node != nil {
		if err := parseAgent(node, &loaded.agent); err != nil {
			return err
		}
	}
	return validateLimitRelationship(loaded.server)
}

func parseClient(node *yaml.Node, client *Client) error {
	fields, err := mappingFields(node, "client")
	if err != nil {
		return err
	}
	if err := rejectUnknown(fields, "client", "server", "timeout"); err != nil {
		return err
	}
	if node := fields["server"]; node != nil {
		value, err := nonemptyString(node, "client.server")
		if err != nil {
			return err
		}
		client.server = present(value)
	}
	if node := fields["timeout"]; node != nil {
		value, err := positiveDuration(node, "client.timeout")
		if err != nil {
			return err
		}
		client.timeout = present(value)
	}
	return nil
}

func parseServer(node *yaml.Node, configDir string, server *Server) error {
	fields, err := mappingFields(node, "server")
	if err != nil {
		return err
	}
	if err := rejectUnknown(fields, "server", "host", "port", "storage", "cleanup_interval", "default_expires_in", "shutdown_timeout", "log_mode", "max_whiteboard_bytes", "max_context_bytes", "max_image_bytes", "max_image_request_bytes"); err != nil {
		return err
	}
	if node := fields["host"]; node != nil {
		value, err := nonemptyString(node, "server.host")
		if err != nil {
			return err
		}
		server.host = present(value)
	}
	if node := fields["port"]; node != nil {
		value, err := port(node, "server.port", true)
		if err != nil {
			return err
		}
		server.port = present(value)
	}
	if node := fields["storage"]; node != nil {
		value, err := nonemptyString(node, "server.storage")
		if err != nil {
			return err
		}
		resolved, err := resolvePathFrom(value, configDir)
		if err != nil {
			return fmt.Errorf("server.storage: %w", err)
		}
		server.storage = present(resolved)
	}
	if node := fields["cleanup_interval"]; node != nil {
		value, err := positiveDuration(node, "server.cleanup_interval")
		if err != nil {
			return err
		}
		server.cleanupInterval = present(value)
	}
	if node := fields["default_expires_in"]; node != nil {
		value, err := nonnegativeInteger(node, "server.default_expires_in")
		if err != nil {
			return err
		}
		server.defaultExpiresIn = present(value)
	}
	if node := fields["shutdown_timeout"]; node != nil {
		value, err := positiveDuration(node, "server.shutdown_timeout")
		if err != nil {
			return err
		}
		server.shutdownTimeout = present(value)
	}
	if node := fields["log_mode"]; node != nil {
		value, err := scalarString(node, "server.log_mode")
		if err != nil {
			return err
		}
		if value != "console" && value != "json" {
			return errors.New("server.log_mode must be console or json")
		}
		server.logMode = present(value)
	}
	limits := []struct {
		name   string
		target *optional[int64]
	}{
		{name: "max_whiteboard_bytes", target: &server.maxWhiteboardBytes},
		{name: "max_context_bytes", target: &server.maxContextBytes},
		{name: "max_image_bytes", target: &server.maxImageBytes},
		{name: "max_image_request_bytes", target: &server.maxImageRequestBytes},
	}
	for _, limit := range limits {
		if node := fields[limit.name]; node != nil {
			value, err := nonnegativeInteger(node, "server."+limit.name)
			if err != nil {
				return err
			}
			*limit.target = present(value)
		}
	}
	return nil
}

func parseViewer(node *yaml.Node, viewer *Viewer) error {
	fields, err := mappingFields(node, "viewer")
	if err != nil {
		return err
	}
	if err := rejectUnknown(fields, "viewer", "local_agent"); err != nil {
		return err
	}
	if localAgentNode := fields["local_agent"]; localAgentNode != nil {
		localFields, err := mappingFields(localAgentNode, "viewer.local_agent")
		if err != nil {
			return err
		}
		if err := rejectUnknown(localFields, "viewer.local_agent", "enabled"); err != nil {
			return err
		}
		if enabledNode := localFields["enabled"]; enabledNode != nil {
			enabled, err := boolean(enabledNode, "viewer.local_agent.enabled")
			if err != nil {
				return err
			}
			viewer.localAgent.enabled = present(enabled)
		}
	}
	return nil
}

func parseAgent(node *yaml.Node, agent *Agent) error {
	fields, err := mappingFields(node, "agent")
	if err != nil {
		return err
	}
	if err := rejectUnknown(fields, "agent", "port", "trusted_origins", "provider_idle_timeout", "shutdown_timeout", "default_access"); err != nil {
		return err
	}
	if node := fields["port"]; node != nil {
		value, err := port(node, "agent.port", false)
		if err != nil {
			return err
		}
		agent.port = present(value)
	}
	if node := fields["trusted_origins"]; node != nil {
		if node.Kind != yaml.SequenceNode || node.Tag != "!!seq" {
			return errors.New("agent.trusted_origins must be a sequence")
		}
		origins := make([]string, 0, len(node.Content))
		seen := make(map[string]struct{}, len(node.Content))
		for _, originNode := range node.Content {
			value, err := scalarString(originNode, "agent.trusted_origins entry")
			if err != nil {
				return err
			}
			canonical, err := CanonicalOrigin(value)
			if err != nil {
				return fmt.Errorf("agent.trusted_origins: %w", err)
			}
			if _, duplicate := seen[canonical]; duplicate {
				return fmt.Errorf("duplicate trusted origin %q", canonical)
			}
			seen[canonical] = struct{}{}
			origins = append(origins, canonical)
		}
		agent.trustedOrigins = present(origins)
	}
	if node := fields["provider_idle_timeout"]; node != nil {
		value, err := positiveDuration(node, "agent.provider_idle_timeout")
		if err != nil {
			return err
		}
		agent.providerIdleTimeout = present(value)
	}
	if node := fields["shutdown_timeout"]; node != nil {
		value, err := positiveDuration(node, "agent.shutdown_timeout")
		if err != nil {
			return err
		}
		agent.shutdownTimeout = present(value)
	}
	if node := fields["default_access"]; node != nil {
		value, err := scalarString(node, "agent.default_access")
		if err != nil {
			return err
		}
		if value != AccessContentOnly {
			return fmt.Errorf("agent.default_access must be %q", AccessContentOnly)
		}
		agent.defaultAccess = present(value)
	}
	return nil
}

func mappingFields(node *yaml.Node, name string) (map[string]*yaml.Node, error) {
	if node.Kind != yaml.MappingNode || node.Tag != "!!map" {
		return nil, fmt.Errorf("%s must be a mapping", name)
	}
	fields := make(map[string]*yaml.Node, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		fields[node.Content[index].Value] = node.Content[index+1]
	}
	return fields, nil
}

func rejectUnknown(fields map[string]*yaml.Node, section string, allowed ...string) error {
	known := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		known[name] = struct{}{}
	}
	for name := range fields {
		if _, found := known[name]; !found {
			return fmt.Errorf("unknown field %s.%s", section, name)
		}
	}
	return nil
}

func scalarString(node *yaml.Node, name string) (string, error) {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return node.Value, nil
}

func nonemptyString(node *yaml.Node, name string) (string, error) {
	value, err := scalarString(node, name)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func boolean(node *yaml.Node, name string) (bool, error) {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!bool" {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	var value bool
	if err := node.Decode(&value); err != nil {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return value, nil
}

func integer(node *yaml.Node, name string) (int64, error) {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	var value int64
	if err := node.Decode(&value); err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return value, nil
}

func nonnegativeInteger(node *yaml.Node, name string) (int64, error) {
	value, err := integer(node, name)
	if err != nil {
		return 0, err
	}
	if value < 0 {
		return 0, fmt.Errorf("%s must not be negative", name)
	}
	return value, nil
}

func port(node *yaml.Node, name string, allowZero bool) (int, error) {
	value, err := integer(node, name)
	if err != nil {
		return 0, err
	}
	minimum := int64(1)
	if allowZero {
		minimum = 0
	}
	if value < minimum || value > 65535 {
		if allowZero {
			return 0, fmt.Errorf("%s must be between 0 and 65535", name)
		}
		return 0, fmt.Errorf("%s must be between 1 and 65535", name)
	}
	return int(value), nil
}

func positiveDuration(node *yaml.Node, name string) (time.Duration, error) {
	value, err := scalarString(node, name)
	if err != nil {
		return 0, err
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return duration, nil
}

func validateLimitRelationship(server Server) error {
	image := int64(25 << 20)
	if server.maxImageBytes.set && server.maxImageBytes.value != 0 {
		image = server.maxImageBytes.value
	}
	request := int64(100 << 20)
	if server.maxImageRequestBytes.set && server.maxImageRequestBytes.value != 0 {
		request = server.maxImageRequestBytes.value
	}
	if request < image {
		return errors.New("server.max_image_request_bytes must not be less than server.max_image_bytes")
	}
	return nil
}
