package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type CommitUncertainError struct {
	Err error
}

func (err *CommitUncertainError) Error() string {
	return fmt.Sprintf("trusted-origin edit may have committed: %v", err.Err)
}

func (err *CommitUncertainError) Unwrap() error { return err.Err }

type editFileOps struct {
	syncFile           func(*os.File) error
	rename             func(*configDirectory, string, string) error
	syncDir            func(*configDirectory) error
	afterDirectoryOpen func()
	beforeTargetOpen   func()
	beforeTargetCheck  func()
}

func defaultEditFileOps() editFileOps {
	return editFileOps{
		syncFile: func(file *os.File) error { return file.Sync() },
		rename: func(directory *configDirectory, oldName, newName string) error {
			return directory.rename(oldName, newName)
		},
		syncDir: func(directory *configDirectory) error { return directory.sync() },
	}
}

func AddTrustedOrigin(selectedPath, origin string) error {
	return editTrustedOrigin(selectedPath, origin, true, defaultEditFileOps())
}

func RemoveTrustedOrigin(selectedPath, origin string) error {
	return editTrustedOrigin(selectedPath, origin, false, defaultEditFileOps())
}

func ListTrustedOrigins(selectedPath string) ([]string, error) {
	return listTrustedOrigins(selectedPath, defaultEditFileOps())
}

func listTrustedOrigins(selectedPath string, ops editFileOps) ([]string, error) {
	path, explicit, err := trustedOriginPath(selectedPath)
	if err != nil {
		return nil, err
	}
	directory, err := openConfigDirectory(filepath.Dir(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit {
			return []string{}, nil
		}
		return nil, fmt.Errorf("open configuration directory %q: %w", filepath.Dir(path), err)
	}
	defer directory.close()
	if ops.afterDirectoryOpen != nil {
		ops.afterDirectoryOpen()
	}

	file, _, err := openConfigTarget(directory, filepath.Base(path), ops.beforeTargetOpen)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit {
			return []string{}, nil
		}
		return nil, fmt.Errorf("open configuration %q: %w", path, err)
	}
	defer file.Close()
	_, loaded, err := parseConfigFile(file, path)
	if err != nil {
		return nil, err
	}
	origins, set := loaded.Agent().TrustedOrigins()
	if !set {
		return []string{}, nil
	}
	return origins, nil
}

func editTrustedOrigin(selectedPath, origin string, add bool, ops editFileOps) error {
	canonical, err := CanonicalOrigin(origin)
	if err != nil {
		return err
	}
	path, explicit, err := trustedOriginPath(selectedPath)
	if err != nil {
		return err
	}
	if !explicit {
		if err := ensureDefaultConfigDirectory(filepath.Dir(path)); err != nil {
			return err
		}
	}

	directory, err := openConfigDirectory(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open configuration directory %q: %w", filepath.Dir(path), err)
	}
	defer directory.close()
	if ops.afterDirectoryOpen != nil {
		ops.afterDirectoryOpen()
	}

	targetName := filepath.Base(path)
	if explicit {
		if _, err := directory.targetIdentity(targetName); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("configuration %q: %w", path, os.ErrNotExist)
			}
			return fmt.Errorf("inspect configuration %q: %w", path, err)
		}
	}
	lock, err := acquireEditLock(directory, targetName+".lock")
	if err != nil {
		return fmt.Errorf("lock configuration %q: %w", path, err)
	}
	defer lock.release()

	root, loaded, originalIdentity, err := readConfigForEdit(directory, path, explicit, ops.beforeTargetOpen)
	if err != nil {
		return err
	}
	origins, set := loaded.Agent().TrustedOrigins()
	if !set {
		origins = []string{}
	}
	updated, changed := updateOriginList(origins, canonical, add)
	if !changed {
		return nil
	}
	setTrustedOrigins(root, updated)

	var document yaml.Node
	document.Kind = yaml.DocumentNode
	document.Content = []*yaml.Node{root}
	var serialized bytes.Buffer
	encoder := yaml.NewEncoder(&serialized)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	return commitConfig(directory, path, serialized.Bytes(), originalIdentity, ops)
}

func ensureDefaultConfigDirectory(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("default configuration parent %q must be a directory", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect default configuration directory: %w", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create default configuration directory: %w", err)
	}
	return nil
}

func trustedOriginPath(selectedPath string) (path string, explicit bool, err error) {
	explicit = selectedPath != ""
	if explicit {
		path, err = ResolvePath(selectedPath)
	} else {
		path, err = DefaultPath()
	}
	if err != nil {
		return "", explicit, fmt.Errorf("resolve configuration path: %w", err)
	}
	return path, explicit, nil
}

func openConfigTarget(directory *configDirectory, name string, beforeOpen func()) (*os.File, fileIdentity, error) {
	expected, err := directory.targetIdentity(name)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	file, err := directory.openTarget(name)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	failed := true
	defer func() {
		if failed {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, fileIdentity{}, err
	}
	if err := validateConfigurationInfo(info); err != nil {
		return nil, fileIdentity{}, err
	}
	opened, err := identityForFile(file)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	if opened != expected {
		return nil, fileIdentity{}, errors.New("configuration must be an unchanged regular file")
	}
	failed = false
	return file, opened, nil
}

func readConfigForEdit(directory *configDirectory, path string, explicit bool, beforeOpen func()) (*yaml.Node, Config, *fileIdentity, error) {
	file, identity, err := openConfigTarget(directory, filepath.Base(path), beforeOpen)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, Config{}, nil, fmt.Errorf("open configuration %q: %w", path, err)
		}
		if explicit {
			return nil, Config{}, nil, fmt.Errorf("configuration %q: %w", path, os.ErrNotExist)
		}
		root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "version"},
			{Kind: yaml.ScalarNode, Tag: "!!int", Value: "1"},
		}}
		return root, Config{path: path, version: Version1}, nil, nil
	}
	defer file.Close()
	root, loaded, err := parseConfigFile(file, path)
	if err != nil {
		return nil, Config{}, nil, err
	}
	return root, loaded, &identity, nil
}

func parseConfigFile(reader io.Reader, path string) (*yaml.Node, Config, error) {
	root, err := decodeDocument(reader)
	if err != nil {
		return nil, Config{}, fmt.Errorf("parse configuration %q: %w", path, err)
	}
	loaded := Config{path: path, exists: true, version: Version1}
	if err := parseConfig(root, filepath.Dir(path), &loaded); err != nil {
		return nil, Config{}, fmt.Errorf("parse configuration %q: %w", path, err)
	}
	return root, loaded, nil
}

func updateOriginList(origins []string, canonical string, add bool) ([]string, bool) {
	index := -1
	for current, origin := range origins {
		if origin == canonical {
			index = current
			break
		}
	}
	if add {
		if index >= 0 {
			return origins, false
		}
		return append(append([]string(nil), origins...), canonical), true
	}
	if index < 0 {
		return origins, false
	}
	updated := append([]string(nil), origins[:index]...)
	updated = append(updated, origins[index+1:]...)
	return updated, true
}

func setTrustedOrigins(root *yaml.Node, origins []string) {
	agent := mappingValue(root, "agent")
	if agent == nil {
		agent = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "agent"}, agent,
		)
	}
	sequence := mappingValue(agent, "trusted_origins")
	if sequence == nil {
		sequence = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		agent.Content = append(agent.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "trusted_origins"}, sequence,
		)
	}

	byCanonical := make(map[string][]*yaml.Node, len(sequence.Content))
	for _, node := range sequence.Content {
		canonical, err := CanonicalOrigin(node.Value)
		if err == nil {
			byCanonical[canonical] = append(byCanonical[canonical], node)
		}
	}
	content := make([]*yaml.Node, 0, len(origins))
	for _, origin := range origins {
		var node *yaml.Node
		if existing := byCanonical[origin]; len(existing) > 0 {
			node = existing[0]
			byCanonical[origin] = existing[1:]
			node.Value = origin
		} else {
			node = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: origin}
		}
		content = append(content, node)
	}
	sequence.Kind = yaml.SequenceNode
	sequence.Tag = "!!seq"
	sequence.Content = content
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

func commitConfig(directory *configDirectory, path string, contents []byte, originalIdentity *fileIdentity, ops editFileOps) error {
	targetName := filepath.Base(path)
	temporary, temporaryName, err := directory.createTemporary(targetName)
	if err != nil {
		return fmt.Errorf("create temporary configuration: %w", err)
	}
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = directory.unlink(temporaryName)
		}
	}()
	if _, err := temporary.Write(contents); err != nil {
		return fmt.Errorf("write temporary configuration: %w", err)
	}
	if err := ops.syncFile(temporary); err != nil {
		return fmt.Errorf("sync temporary configuration: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary configuration: %w", err)
	}
	if ops.beforeTargetCheck != nil {
		ops.beforeTargetCheck()
	}
	if err := verifyUnchangedTarget(directory, path, originalIdentity); err != nil {
		return err
	}
	if err := ops.rename(directory, temporaryName, targetName); err != nil {
		return fmt.Errorf("commit configuration: %w", err)
	}
	committed = true
	if err := ops.syncDir(directory); err != nil {
		return &CommitUncertainError{Err: fmt.Errorf("sync configuration directory: %w", err)}
	}
	return nil
}

func verifyUnchangedTarget(directory *configDirectory, path string, originalIdentity *fileIdentity) error {
	current, err := directory.targetIdentity(filepath.Base(path))
	if originalIdentity == nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reinspect configuration %q: %w", path, err)
		}
		return fmt.Errorf("configuration %q changed while editing", path)
	}
	if err != nil || current != *originalIdentity {
		return fmt.Errorf("configuration %q changed while editing", path)
	}
	// The advisory lock coordinates cooperative editors. A same-user process that
	// ignores the lock can still race this check and rename; cross-platform Unix
	// APIs do not provide an atomic conditional replacement by inode.
	return nil
}
