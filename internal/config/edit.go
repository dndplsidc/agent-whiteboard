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
	syncFile func(*os.File) error
	rename   func(string, string) error
	syncDir  func(*os.File) error
}

func defaultEditFileOps() editFileOps {
	return editFileOps{
		syncFile: func(file *os.File) error { return file.Sync() },
		rename:   os.Rename,
		syncDir:  func(directory *os.File) error { return directory.Sync() },
	}
}

func AddTrustedOrigin(selectedPath, origin string) error {
	return editTrustedOrigin(selectedPath, origin, true, defaultEditFileOps())
}

func RemoveTrustedOrigin(selectedPath, origin string) error {
	return editTrustedOrigin(selectedPath, origin, false, defaultEditFileOps())
}

func ListTrustedOrigins(selectedPath string) ([]string, error) {
	path, explicit, err := trustedOriginPath(selectedPath)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit {
			return []string{}, nil
		}
		return nil, fmt.Errorf("inspect configuration %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("configuration %q must be a regular file", path)
	}
	file, err := openRegularNoFollow(path)
	if err != nil {
		return nil, fmt.Errorf("open configuration %q: %w", path, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect open configuration %q: %w", path, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("configuration %q must be an unchanged regular file", path)
	}
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
	if explicit {
		if _, err := os.Lstat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("configuration %q: %w", path, os.ErrNotExist)
			}
			return fmt.Errorf("inspect configuration %q: %w", path, err)
		}
	} else if err := ensureDefaultConfigDirectory(filepath.Dir(path)); err != nil {
		return err
	}

	lock, err := acquireEditLock(path + ".lock")
	if err != nil {
		return fmt.Errorf("lock configuration %q: %w", path, err)
	}
	defer lock.release()

	root, loaded, originalInfo, err := readConfigForEdit(path, explicit)
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
	if err := commitConfig(path, serialized.Bytes(), originalInfo, ops); err != nil {
		return err
	}
	return nil
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
	if err := os.Mkdir(path, 0o700); err != nil {
		return fmt.Errorf("create default configuration directory: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("secure default configuration directory: %w", err)
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

func readConfigForEdit(path string, explicit bool) (*yaml.Node, Config, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, Config{}, nil, fmt.Errorf("inspect configuration %q: %w", path, err)
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
	if !info.Mode().IsRegular() {
		return nil, Config{}, nil, fmt.Errorf("configuration %q must be a regular file", path)
	}
	file, err := openRegularNoFollow(path)
	if err != nil {
		return nil, Config{}, nil, fmt.Errorf("open configuration %q: %w", path, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, Config{}, nil, fmt.Errorf("inspect open configuration %q: %w", path, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, Config{}, nil, fmt.Errorf("configuration %q must be an unchanged regular file", path)
	}
	root, loaded, err := parseConfigFile(file, path)
	if err != nil {
		return nil, Config{}, nil, err
	}
	return root, loaded, openedInfo, nil
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

func commitConfig(path string, contents []byte, originalInfo os.FileInfo, ops editFileOps) error {
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open configuration directory: %w", err)
	}
	defer directory.Close()
	directoryInfo, err := directory.Stat()
	if err != nil || !directoryInfo.IsDir() {
		if err == nil {
			err = errors.New("not a directory")
		}
		return fmt.Errorf("inspect configuration directory: %w", err)
	}

	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary configuration: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary configuration: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		return fmt.Errorf("write temporary configuration: %w", err)
	}
	if err := ops.syncFile(temporary); err != nil {
		return fmt.Errorf("sync temporary configuration: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary configuration: %w", err)
	}
	if err := verifyUnchangedTarget(path, originalInfo); err != nil {
		return err
	}
	if err := ops.rename(temporaryPath, path); err != nil {
		return fmt.Errorf("commit configuration: %w", err)
	}
	committed = true
	if err := ops.syncDir(directory); err != nil {
		return &CommitUncertainError{Err: fmt.Errorf("sync configuration directory: %w", err)}
	}
	return nil
}

func verifyUnchangedTarget(path string, originalInfo os.FileInfo) error {
	current, err := os.Lstat(path)
	if originalInfo == nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reinspect configuration %q: %w", path, err)
		}
		return fmt.Errorf("configuration %q changed while editing", path)
	}
	if err != nil {
		return fmt.Errorf("reinspect configuration %q: %w", path, err)
	}
	if !current.Mode().IsRegular() || !os.SameFile(originalInfo, current) {
		return fmt.Errorf("configuration %q changed while editing", path)
	}
	return nil
}
