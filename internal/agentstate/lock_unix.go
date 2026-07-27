//go:build darwin || linux

package agentstate

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type secureDirectory struct {
	file          *os.File
	mutationGuard func() error
}

type stateLayout struct {
	rootPath      string
	homePath      string
	home          *secureDirectory
	agent         *secureDirectory
	state         *secureDirectory
	conversations *secureDirectory
	workspaces    *secureDirectory
	providers     *secureDirectory
	lock          *os.File
}

func openStateLayout(homePath string) (*stateLayout, error) {
	homeDir, err := openAbsoluteDirectory(homePath, false)
	if err != nil {
		return nil, fmt.Errorf("open home directory: %w", err)
	}
	layout := &stateLayout{homePath: homePath, rootPath: filepath.Join(homePath, ".agent-whiteboard", "state"), home: homeDir}
	failed := true
	defer func() {
		if failed {
			_ = layout.close()
		}
	}()
	if layout.agent, _, err = homeDir.ensureDirectory(".agent-whiteboard", false); err != nil {
		return nil, fmt.Errorf("open agent-whiteboard directory: %w", err)
	}
	if layout.state, _, err = layout.agent.ensureDirectory("state", true); err != nil {
		return nil, fmt.Errorf("open state directory: %w", err)
	}
	if layout.conversations, _, err = layout.state.ensureDirectory("conversations", true); err != nil {
		return nil, fmt.Errorf("open conversations directory: %w", err)
	}
	if layout.workspaces, _, err = layout.state.ensureDirectory("workspaces", true); err != nil {
		return nil, fmt.Errorf("open workspaces directory: %w", err)
	}
	if layout.providers, _, err = layout.state.ensureDirectory("providers", true); err != nil {
		return nil, fmt.Errorf("open providers directory: %w", err)
	}
	layout.lock, err = acquireBrokerLock(layout.state)
	if err != nil {
		return nil, err
	}
	if err := layout.verify(); err != nil {
		return nil, err
	}
	failed = false
	return layout, nil
}

func (layout *stateLayout) verify() error {
	if layout == nil || layout.home == nil {
		return errors.New("state layout is unavailable")
	}
	if err := verifyAbsoluteDirectory(layout.homePath, layout.home, false); err != nil {
		return fmt.Errorf("canonical home changed: %w", err)
	}
	for _, child := range []struct {
		parent  *secureDirectory
		name    string
		opened  *secureDirectory
		private bool
	}{
		{layout.home, ".agent-whiteboard", layout.agent, false},
		{layout.agent, "state", layout.state, true},
		{layout.state, "conversations", layout.conversations, true},
		{layout.state, "workspaces", layout.workspaces, true},
		{layout.state, "providers", layout.providers, true},
	} {
		if err := child.parent.verifyChild(child.name, child.opened, child.private); err != nil {
			return fmt.Errorf("canonical state path %q changed: %w", child.name, err)
		}
	}
	if layout.lock != nil {
		if _, err := verifiedFileStat(layout.state, "broker.lock", layout.lock, 0o600); err != nil {
			return fmt.Errorf("canonical broker lock changed: %w", err)
		}
	}
	return nil
}

func (layout *stateLayout) close() error {
	if layout == nil {
		return nil
	}
	var errs []error
	if layout.lock != nil {
		_ = unix.Flock(int(layout.lock.Fd()), unix.LOCK_UN)
		errs = append(errs, layout.lock.Close())
		layout.lock = nil
	}
	for _, directory := range []*secureDirectory{layout.providers, layout.workspaces, layout.conversations, layout.state, layout.agent, layout.home} {
		if directory != nil {
			errs = append(errs, directory.close())
		}
	}
	return errors.Join(errs...)
}

func openAbsoluteDirectory(path string, private bool) (*secureDirectory, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	directory := &secureDirectory{file: os.NewFile(uintptr(fd), path)}
	if directory.file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open directory")
	}
	if err := directory.validate(private); err != nil {
		_ = directory.close()
		return nil, err
	}
	return directory, nil
}

func openDirectoryAt(parent *secureDirectory, name string, private bool) (*secureDirectory, error) {
	if !validName(name) {
		return nil, errors.New("invalid state path name")
	}
	fd, err := unix.Openat(parent.fd(), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	directory := &secureDirectory{file: os.NewFile(uintptr(fd), name), mutationGuard: parent.mutationGuard}
	if directory.file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open state directory")
	}
	if err := directory.validate(private); err != nil {
		_ = directory.close()
		return nil, err
	}
	return directory, nil
}

func (directory *secureDirectory) validate(private bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(directory.fd(), &stat); err != nil {
		return err
	}
	return validateDirectoryStat(&stat, private)
}

func validateDirectoryStat(stat *unix.Stat_t, private bool) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("state directory must be an owner-controlled directory")
	}
	permissions := os.FileMode(stat.Mode & 0o777)
	if private {
		if permissions != 0o700 {
			return errors.New("state directory must have mode 0700")
		}
	} else if permissions&0o022 != 0 {
		return errors.New("state parent must not be writable by group or others")
	}
	return nil
}

func verifyAbsoluteDirectory(path string, opened *secureDirectory, private bool) error {
	var current, actual unix.Stat_t
	if err := unix.Lstat(path, &current); err != nil {
		return err
	}
	if err := validateDirectoryStat(&current, private); err != nil {
		return err
	}
	if err := unix.Fstat(opened.fd(), &actual); err != nil {
		return err
	}
	if err := validateDirectoryStat(&actual, private); err != nil || identityFromStat(&current) != identityFromStat(&actual) {
		return errors.New("directory identity changed")
	}
	return nil
}

func (directory *secureDirectory) verifyChild(name string, opened *secureDirectory, private bool) error {
	if opened == nil || !validName(name) {
		return errors.New("invalid state directory")
	}
	var current, actual unix.Stat_t
	if err := unix.Fstatat(directory.fd(), name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if err := validateDirectoryStat(&current, private); err != nil {
		return err
	}
	if err := unix.Fstat(opened.fd(), &actual); err != nil {
		return err
	}
	if err := validateDirectoryStat(&actual, private); err != nil || identityFromStat(&current) != identityFromStat(&actual) {
		return errors.New("directory identity changed")
	}
	return nil
}

func (directory *secureDirectory) ensureDirectory(name string, private bool) (*secureDirectory, bool, error) {
	created := false
	if err := directory.verifyMutationAllowed(); err != nil {
		return nil, false, err
	}
	if err := unix.Mkdirat(directory.fd(), name, 0o700); err == nil {
		created = true
	} else if !errors.Is(err, unix.EEXIST) {
		return nil, false, err
	}
	child, err := openDirectoryAt(directory, name, private)
	if err != nil {
		return nil, false, err
	}
	if err := directory.verifyChild(name, child, private); err != nil {
		_ = child.close()
		return nil, false, err
	}
	if created {
		if err := child.verifyMutationAllowed(); err != nil {
			_ = child.close()
			return nil, false, err
		}
		if err := unix.Fchmod(child.fd(), 0o700); err != nil {
			_ = child.close()
			return nil, false, err
		}
		if err := directory.sync(); err != nil {
			_ = child.close()
			return nil, false, err
		}
	}
	return child, created, nil
}

func acquireBrokerLock(directory *secureDirectory) (*os.File, error) {
	fd, created, err := -1, false, error(nil)
	for attempt := 0; attempt < 128; attempt++ {
		fd, created, err = openOrCreateFile(directory, "broker.lock", 0o600)
		if !errors.Is(err, unix.EEXIST) && !errors.Is(err, unix.ENOENT) {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("open broker lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), "broker.lock")
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open broker lock")
	}
	failed := true
	defer func() {
		if failed {
			_ = file.Close()
		}
	}()
	if created {
		if err := file.Chmod(0o600); err != nil {
			return nil, err
		}
		if err := directory.sync(); err != nil {
			return nil, err
		}
	}
	if _, err := verifiedFileStat(directory, "broker.lock", file, 0o600); err != nil {
		return nil, fmt.Errorf("inspect broker lock: %w", err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("acquire broker lock: %w", err)
	}
	failed = false
	return file, nil
}

func openOrCreateFile(directory *secureDirectory, name string, mode uint32) (fd int, created bool, err error) {
	fd, err = unix.Openat(directory.fd(), name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err == nil {
		return fd, false, nil
	}
	if !errors.Is(err, unix.ENOENT) {
		return -1, false, err
	}
	fd, err = unix.Openat(directory.fd(), name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
	if err != nil {
		return -1, false, err
	}
	return fd, true, nil
}

func randomStateName(prefix string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random[:]), nil
}

func (directory *secureDirectory) createTemporary() (*os.File, string, error) {
	for attempt := 0; attempt < 128; attempt++ {
		if err := directory.verifyMutationAllowed(); err != nil {
			return nil, "", err
		}
		name, err := randomStateName(".mapping.tmp-")
		if err != nil {
			return nil, "", err
		}
		fd, err := unix.Openat(directory.fd(), name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		file := os.NewFile(uintptr(fd), name)
		if file == nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(directory.fd(), name, 0)
			return nil, "", errors.New("create temporary mapping")
		}
		if err := directory.verifyMutationAllowed(); err != nil {
			_ = file.Close()
			return nil, "", err
		}
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			_ = unix.Unlinkat(directory.fd(), name, 0)
			return nil, "", err
		}
		return file, name, nil
	}
	return nil, "", errors.New("temporary mapping collision limit exceeded")
}

func (directory *secureDirectory) readVerified(name string, limit int64) ([]byte, fileIdentity, error) {
	if !validName(name) {
		return nil, fileIdentity{}, errors.New("invalid state filename")
	}
	var before unix.Stat_t
	if err := unix.Fstatat(directory.fd(), name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, fileIdentity{}, err
	}
	if err := validateRegularStat(&before, 0o600); err != nil {
		return nil, fileIdentity{}, err
	}
	fd, err := unix.Openat(directory.fd(), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fileIdentity{}, errors.New("open state file")
	}
	defer file.Close()
	openedIdentity, err := verifiedFileStat(directory, name, file, 0o600)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	if openedIdentity != identityFromStat(&before) {
		return nil, fileIdentity{}, errors.New("state file identity changed while opening")
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fileIdentity{}, err
	}
	if int64(len(content)) > limit {
		return nil, fileIdentity{}, errors.New("conversation mapping exceeds size limit")
	}
	return content, identityFromStat(&before), nil
}

func verifiedFileStat(directory *secureDirectory, name string, file *os.File, mode os.FileMode) (fileIdentity, error) {
	opened, err := fileIdentityForFile(file, mode)
	if err != nil {
		return fileIdentity{}, err
	}
	current, err := directory.targetIdentity(name)
	if err != nil || current != opened {
		return fileIdentity{}, errors.New("state file identity changed while opening")
	}
	return opened, nil
}

func fileIdentityForFile(file *os.File, mode os.FileMode) (fileIdentity, error) {
	var opened unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &opened); err != nil {
		return fileIdentity{}, err
	}
	if err := validateRegularStat(&opened, mode); err != nil {
		return fileIdentity{}, err
	}
	return identityFromStat(&opened), nil
}

func validateRegularStat(stat *unix.Stat_t, mode os.FileMode) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) || uint64(stat.Nlink) != 1 {
		return errors.New("state file must be an owner-controlled, singly-linked regular file")
	}
	if os.FileMode(stat.Mode&0o777) != mode {
		return fmt.Errorf("state file must have mode %04o", mode)
	}
	return nil
}

func (directory *secureDirectory) targetIdentity(name string) (fileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(directory.fd(), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fileIdentity{}, err
	}
	if err := validateRegularStat(&stat, 0o600); err != nil {
		return fileIdentity{}, err
	}
	return identityFromStat(&stat), nil
}

func (directory *secureDirectory) childIdentity(name string) (fileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(directory.fd(), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fileIdentity{}, err
	}
	if err := validateDirectoryStat(&stat, true); err != nil {
		return fileIdentity{}, err
	}
	return identityFromStat(&stat), nil
}

func (directory *secureDirectory) openedIdentity() (fileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(directory.fd(), &stat); err != nil {
		return fileIdentity{}, err
	}
	if err := validateDirectoryStat(&stat, true); err != nil {
		return fileIdentity{}, err
	}
	return identityFromStat(&stat), nil
}

func identityFromStat(stat *unix.Stat_t) fileIdentity {
	return fileIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}
}

func (directory *secureDirectory) names() ([]string, error) {
	duplicate, err := unix.Dup(directory.fd())
	if err != nil {
		return nil, err
	}
	if _, err := unix.Seek(duplicate, 0, 0); err != nil {
		_ = unix.Close(duplicate)
		return nil, err
	}
	file := os.NewFile(uintptr(duplicate), "state-directory")
	if file == nil {
		_ = unix.Close(duplicate)
		return nil, errors.New("read state directory")
	}
	names, readErr := file.Readdirnames(-1)
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	return names, closeErr
}

func (directory *secureDirectory) publish(oldName, newName string, original *fileIdentity, replacement fileIdentity) error {
	if !validName(oldName) || !validName(newName) {
		return errors.New("invalid state filename")
	}
	if original == nil {
		if err := directory.verifyMutationAllowed(); err != nil {
			return err
		}
		if err := atomicRenameNoReplace(directory.fd(), oldName, directory.fd(), newName); err != nil {
			return err
		}
		current, err := directory.targetIdentity(newName)
		if err != nil || current != replacement {
			return errors.New("published mapping identity changed")
		}
		return nil
	}
	if err := directory.verifyMutationAllowed(); err != nil {
		return err
	}
	if err := atomicRenameExchange(directory.fd(), oldName, directory.fd(), newName); err != nil {
		return err
	}
	oldCurrent, oldErr := directory.targetIdentity(oldName)
	newCurrent, newErr := directory.targetIdentity(newName)
	if oldErr != nil || newErr != nil || oldCurrent != *original || newCurrent != replacement {
		if err := directory.verifyMutationAllowed(); err != nil {
			return fmt.Errorf("mapping target changed and rollback was blocked: %w", err)
		}
		rollbackErr := atomicRenameExchange(directory.fd(), oldName, directory.fd(), newName)
		if rollbackErr != nil {
			return fmt.Errorf("mapping target changed and rollback failed: %w", rollbackErr)
		}
		return errors.New("conversation mapping changed during update")
	}
	if err := directory.removeExpected(oldName, *original); err != nil {
		return fmt.Errorf("remove replaced mapping: %w", err)
	}
	return nil
}

func (directory *secureDirectory) removeExpected(name string, expected fileIdentity) error {
	return directory.removeExpectedWithUnlink(name, expected, func(parent *secureDirectory, tombstone string) error {
		return parent.unlinkFile(tombstone)
	})
}

func (directory *secureDirectory) removeExpectedWithUnlink(name string, expected fileIdentity, unlink func(*secureDirectory, string) error) error {
	if !validName(name) {
		return errors.New("invalid state filename")
	}
	tombstone := mappingTombstoneName(name)
	if !validName(tombstone) {
		return errors.New("invalid mapping tombstone name")
	}
	if err := directory.verifyMutationAllowed(); err != nil {
		return err
	}
	if err := atomicRenameNoReplace(directory.fd(), name, directory.fd(), tombstone); err != nil {
		return err
	}
	current, inspectErr := directory.targetIdentity(tombstone)
	if inspectErr != nil || current != expected {
		restoreErr := directory.restoreTombstone(tombstone, name)
		if restoreErr != nil {
			return fmt.Errorf("target identity changed and restore failed: %w", restoreErr)
		}
		return errors.New("target identity changed before removal")
	}
	if err := directory.verifyMutationAllowed(); err != nil {
		return err
	}
	if err := unlink(directory, tombstone); err != nil {
		restoreErr := directory.restoreTombstone(tombstone, name)
		if restoreErr != nil {
			return errors.Join(err, fmt.Errorf("restore mapping after failed unlink: %w", restoreErr))
		}
		return err
	}
	return nil
}

func (directory *secureDirectory) restoreTombstone(tombstone, name string) error {
	if err := directory.verifyMutationAllowed(); err != nil {
		return err
	}
	if err := atomicRenameNoReplace(directory.fd(), tombstone, directory.fd(), name); err != nil {
		return err
	}
	return directory.sync()
}

func (directory *secureDirectory) remove(name string) error {
	identity, err := directory.targetIdentity(name)
	if err != nil {
		return err
	}
	return directory.removeExpected(name, identity)
}

func (directory *secureDirectory) removeDirectory(name string) error {
	return directory.removeDirectoryWithHook(
		name,
		func() {},
		func(workspace *secureDirectory) error { return workspace.close() },
		func(parent *secureDirectory, tombstone string) error { return parent.unlinkDirectory(tombstone) },
	)
}

func (directory *secureDirectory) removeDirectoryWithHook(
	name string,
	beforeTombstone func(),
	closeWorkspace func(*secureDirectory) error,
	unlinkWorkspace func(*secureDirectory, string) error,
) error {
	if !validName(name) {
		return errors.New("invalid state directory name")
	}
	tombstone := workspaceTombstoneName(name)
	if !validName(tombstone) {
		return errors.New("invalid workspace tombstone name")
	}

	child, err := openDirectoryAt(directory, name, true)
	fromCanonical := err == nil
	if errors.Is(err, unix.ENOENT) {
		child, err = openDirectoryAt(directory, tombstone, true)
		if errors.Is(err, unix.ENOENT) {
			return os.ErrNotExist
		}
	}
	if err != nil {
		return err
	}
	expected, err := child.openedIdentity()
	if err != nil {
		_ = child.close()
		return err
	}

	if fromCanonical {
		if _, tombErr := directory.childIdentity(tombstone); tombErr == nil {
			_ = child.close()
			return errors.New("workspace has a pending tombstone")
		} else if !errors.Is(tombErr, unix.ENOENT) {
			_ = child.close()
			return tombErr
		}
		beforeTombstone()
		if err := directory.verifyMutationAllowed(); err != nil {
			_ = child.close()
			return err
		}
		if err := atomicRenameNoReplace(directory.fd(), name, directory.fd(), tombstone); err != nil {
			_ = child.close()
			return err
		}
	}

	current, inspectErr := directory.childIdentity(tombstone)
	if inspectErr != nil || current != expected {
		restoreErr := directory.restoreWorkspaceTombstone(tombstone, name)
		_ = child.close()
		if restoreErr != nil {
			return fmt.Errorf("workspace identity changed and restore failed: %w", restoreErr)
		}
		return errors.New("workspace identity changed before removal")
	}
	if err := child.removeContents(); err != nil {
		restoreErr := directory.restoreWorkspaceTombstone(tombstone, name)
		_ = child.close()
		if restoreErr != nil {
			return errors.Join(err, fmt.Errorf("restore workspace after failed cleanup: %w", restoreErr))
		}
		return err
	}
	if err := closeWorkspace(child); err != nil {
		restoreErr := directory.restoreWorkspaceTombstone(tombstone, name)
		if restoreErr != nil {
			return errors.Join(err, fmt.Errorf("restore workspace after failed close: %w", restoreErr))
		}
		return err
	}
	if err := directory.verifyMutationAllowed(); err != nil {
		return err
	}
	if err := unlinkWorkspace(directory, tombstone); err != nil {
		restoreErr := directory.restoreWorkspaceTombstone(tombstone, name)
		if restoreErr != nil {
			return errors.Join(err, fmt.Errorf("restore workspace after failed unlink: %w", restoreErr))
		}
		return err
	}
	return directory.sync()
}

func (directory *secureDirectory) restoreWorkspaceTombstone(tombstone, name string) error {
	if err := directory.verifyMutationAllowed(); err != nil {
		return err
	}
	if err := atomicRenameNoReplace(directory.fd(), tombstone, directory.fd(), name); err != nil {
		return err
	}
	return directory.sync()
}

func (directory *secureDirectory) removeContents() error {
	names, err := directory.names()
	if err != nil {
		return err
	}
	for _, name := range names {
		if !validName(name) {
			return errors.New("invalid workspace entry")
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(directory.fd(), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			child, err := openDirectoryAt(directory, name, true)
			if err != nil {
				return err
			}
			if err := child.removeContents(); err != nil {
				_ = child.close()
				return err
			}
			if err := child.close(); err != nil {
				return err
			}
			if err := directory.unlinkDirectory(name); err != nil {
				return err
			}
		case unix.S_IFREG:
			if err := validateRegularStat(&stat, 0o600); err != nil {
				return err
			}
			if err := directory.unlinkFile(name); err != nil {
				return err
			}
		default:
			return errors.New("workspace entry must be a real regular file or directory")
		}
	}
	return directory.sync()
}

func (directory *secureDirectory) setMutationGuard(guard func() error) {
	directory.mutationGuard = guard
}

func (directory *secureDirectory) verifyMutationAllowed() error {
	if directory.mutationGuard == nil {
		return nil
	}
	return directory.mutationGuard()
}

func (directory *secureDirectory) unlinkFile(name string) error {
	if err := directory.verifyMutationAllowed(); err != nil {
		return err
	}
	return unix.Unlinkat(directory.fd(), name, 0)
}

func (directory *secureDirectory) unlinkDirectory(name string) error {
	if err := directory.verifyMutationAllowed(); err != nil {
		return err
	}
	return unix.Unlinkat(directory.fd(), name, unix.AT_REMOVEDIR)
}

func (directory *secureDirectory) sync() error  { return unix.Fsync(directory.fd()) }
func (directory *secureDirectory) fd() int      { return int(directory.file.Fd()) }
func (directory *secureDirectory) close() error { return directory.file.Close() }

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name
}
