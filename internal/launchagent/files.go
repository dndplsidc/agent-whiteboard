package launchagent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type fileOps struct {
	syncFile func(*os.File) error
	rename   func(string, string) error
	syncDir  func(string) error
	remove   func(string) error
}

func defaultFileOps() fileOps {
	return fileOps{
		syncFile: func(file *os.File) error { return file.Sync() },
		rename:   os.Rename,
		syncDir:  syncDirectory,
		remove:   os.Remove,
	}
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// CommitUncertainError reports that recovery after a post-rename durability
// failure could not itself be proven durable.
type CommitUncertainError struct{ Err error }

func (err *CommitUncertainError) Error() string {
	return fmt.Sprintf("LaunchAgent plist commit may have changed: %v", err.Err)
}

func (err *CommitUncertainError) Unwrap() error { return err.Err }

func writeAtomic(path string, contents []byte, ops fileOps) error {
	oldContents, existed, err := readExistingRegular(path)
	if err != nil {
		return err
	}
	if err := writeReplacement(path, contents, ops); err != nil {
		var postRename *postRenameError
		if !errors.As(err, &postRename) {
			return err
		}
		if recoveryErr := restoreReplacement(path, oldContents, existed, ops); recoveryErr != nil {
			return &CommitUncertainError{Err: errors.Join(postRename.Err, recoveryErr)}
		}
		return postRename.Err
	}
	return nil
}

type postRenameError struct{ Err error }

func (err *postRenameError) Error() string { return err.Err.Error() }
func (err *postRenameError) Unwrap() error { return err.Err }

func writeReplacement(path string, contents []byte, ops fileOps) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("create temporary plist: %w", err)
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
		return fmt.Errorf("set temporary plist permissions: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		return fmt.Errorf("write temporary plist: %w", err)
	}
	if err := ops.syncFile(temporary); err != nil {
		return fmt.Errorf("sync temporary plist: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary plist: %w", err)
	}
	if err := ops.rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace LaunchAgent plist: %w", err)
	}
	committed = true
	if err := ops.syncDir(directory); err != nil {
		return &postRenameError{Err: fmt.Errorf("sync LaunchAgents directory: %w", err)}
	}
	return nil
}

func restoreReplacement(path string, oldContents []byte, existed bool, ops fileOps) error {
	if existed {
		if err := writeReplacement(path, oldContents, ops); err != nil {
			return fmt.Errorf("restore prior LaunchAgent plist: %w", err)
		}
		return nil
	}
	if err := ops.remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove uncommitted LaunchAgent plist: %w", err)
	}
	if err := ops.syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync restored LaunchAgents directory: %w", err)
	}
	return nil
}

func removeDurable(path string, ops fileOps) error {
	oldContents, existed, err := readExistingRegular(path)
	if err != nil {
		return err
	}
	if !existed {
		return nil
	}
	if err := ops.remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove LaunchAgent plist: %w", err)
	}
	if err := ops.syncDir(filepath.Dir(path)); err != nil {
		if recoveryErr := restoreReplacement(path, oldContents, true, ops); recoveryErr != nil {
			return &CommitUncertainError{Err: errors.Join(fmt.Errorf("sync LaunchAgents directory after removal: %w", err), recoveryErr)}
		}
		return fmt.Errorf("sync LaunchAgents directory after removal: %w", err)
	}
	return nil
}

func readExistingRegular(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect LaunchAgent plist: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, errors.New("LaunchAgent plist must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("open LaunchAgent plist: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("inspect opened LaunchAgent plist: %w", err)
	}
	if !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() {
		return nil, false, errors.New("LaunchAgent plist must be an unchanged regular file")
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		return nil, false, fmt.Errorf("read LaunchAgent plist: %w", err)
	}
	return contents, true, nil
}

func ensurePrivateDirectory(home, path string) error {
	relative, err := filepath.Rel(home, path)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) {
		return errors.New("private directory must be below the user home")
	}
	current := home
	for _, component := range splitPath(relative) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil {
				return fmt.Errorf("create private directory %q: %w", current, err)
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return fmt.Errorf("inspect private directory %q: %w", current, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("private directory %q must not be a symlink or non-directory", current)
		}
		if err := os.Chmod(current, 0o700); err != nil {
			return fmt.Errorf("set private directory permissions %q: %w", current, err)
		}
	}
	return nil
}

func splitPath(path string) []string {
	var components []string
	for path != "." && path != string(filepath.Separator) && path != "" {
		directory, base := filepath.Split(path)
		if base != "" {
			components = append([]string{base}, components...)
		}
		path = filepath.Clean(directory)
	}
	return components
}

func ensurePrivateFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if createErr != nil {
			return fmt.Errorf("create private file %q: %w", path, createErr)
		}
		if syncErr := file.Sync(); syncErr != nil {
			_ = file.Close()
			return fmt.Errorf("sync private file %q: %w", path, syncErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("close private file %q: %w", path, closeErr)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("inspect private file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("private file %q must not be a symlink or non-regular file", path)
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open private file %q: %w", path, err)
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return fmt.Errorf("private file %q changed while opening", path)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("set private file permissions %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close private file %q: %w", path, err)
	}
	return nil
}

func installedPlist(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect LaunchAgent plist: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("LaunchAgent plist must be a regular file")
	}
	return true, nil
}
