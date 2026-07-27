//go:build darwin

package launchagent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

type fileOps struct {
	syncFile func(*os.File) error
	syncDir  func(*os.File) error
	renameAt func(int, string, int, string) error
	unlinkAt func(int, string, int) error
}

func defaultFileOps() fileOps {
	return fileOps{
		syncFile: func(file *os.File) error { return file.Sync() },
		syncDir:  func(directory *os.File) error { return directory.Sync() },
		renameAt: unix.Renameat,
		unlinkAt: unix.Unlinkat,
	}
}

type secureDir struct {
	file *os.File
	uid  uint32
}

func (directory *secureDir) close()  { _ = directory.file.Close() }
func (directory *secureDir) fd() int { return int(directory.file.Fd()) }

func openControlledDirectory(path string) (*secureDir, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	directory := &secureDir{file: os.NewFile(uintptr(fd), path), uid: uint32(os.Getuid())}
	if err := directory.verify(false); err != nil {
		directory.close()
		return nil, err
	}
	return directory, nil
}

func (directory *secureDir) verify(private bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(directory.fd(), &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("path must identify a directory")
	}
	if stat.Uid != directory.uid {
		return errors.New("directory must be owned by the current user")
	}
	if stat.Mode&0o022 != 0 {
		return errors.New("directory must not be writable by group or others")
	}
	if private && stat.Mode&0o777 != 0o700 {
		return errors.New("private directory has unsafe permissions")
	}
	return nil
}

func (directory *secureDir) openChild(name string, create, private bool) (*secureDir, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	fd, err := unix.Openat(directory.fd(), name, flags, 0)
	created := false
	if errors.Is(err, unix.ENOENT) && create {
		if err := unix.Mkdirat(directory.fd(), name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, err
		}
		created = true
		fd, err = unix.Openat(directory.fd(), name, flags, 0)
	}
	if err != nil {
		return nil, err
	}
	child := &secureDir{file: os.NewFile(uintptr(fd), name), uid: directory.uid}
	if err := child.verify(false); err != nil {
		child.close()
		return nil, err
	}
	if private {
		if err := unix.Fchmod(child.fd(), 0o700); err != nil {
			child.close()
			return nil, err
		}
		if err := child.verify(true); err != nil {
			child.close()
			return nil, err
		}
	}
	if created {
		if err := directory.file.Sync(); err != nil {
			child.close()
			return nil, err
		}
	}
	return child, nil
}

func openLaunchAgents(home string, create bool) (*secureDir, error) {
	homeDir, err := openControlledDirectory(home)
	if err != nil {
		return nil, fmt.Errorf("open user home securely: %w", err)
	}
	defer homeDir.close()
	library, err := homeDir.openChild("Library", false, false)
	if err != nil {
		return nil, fmt.Errorf("open Library securely: %w", err)
	}
	defer library.close()
	launchAgents, err := library.openChild("LaunchAgents", create, true)
	if err != nil {
		return nil, fmt.Errorf("open LaunchAgents securely: %w", err)
	}
	return launchAgents, nil
}

func verifyLaunchAgentsBinding(home string, expected *secureDir) error {
	current, err := openLaunchAgents(home, false)
	if err != nil {
		return err
	}
	defer current.close()
	expectedInfo, err := expected.file.Stat()
	if err != nil {
		return err
	}
	currentInfo, err := current.file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(expectedInfo, currentInfo) {
		return errors.New("LaunchAgents directory changed during operation")
	}
	return nil
}

func openLogDirectory(home string, create bool) (*secureDir, error) {
	homeDir, err := openControlledDirectory(home)
	if err != nil {
		return nil, fmt.Errorf("open user home securely: %w", err)
	}
	defer homeDir.close()
	private, err := homeDir.openChild(".agent-whiteboard", create, true)
	if err != nil {
		return nil, fmt.Errorf("open private application directory securely: %w", err)
	}
	defer private.close()
	logs, err := private.openChild("logs", create, true)
	if err != nil {
		return nil, fmt.Errorf("open log directory securely: %w", err)
	}
	return logs, nil
}

func verifyLogDirectoryBinding(home string, expected *secureDir) error {
	current, err := openLogDirectory(home, false)
	if err != nil {
		return err
	}
	defer current.close()
	expectedInfo, err := expected.file.Stat()
	if err != nil {
		return err
	}
	currentInfo, err := current.file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(expectedInfo, currentInfo) {
		return errors.New("log directory changed during operation")
	}
	return nil
}

func ensurePrivateFile(directory *secureDir, name string, ops fileOps) error {
	flags := unix.O_WRONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	fd, err := unix.Openat(directory.fd(), name, flags|unix.O_CREAT|unix.O_EXCL, 0o600)
	created := err == nil
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(directory.fd(), name, flags, 0)
	}
	if err != nil {
		return fmt.Errorf("open private file %q: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	if err := verifyOwnedRegular(file, directory.uid); err != nil {
		return fmt.Errorf("validate private file %q: %w", name, err)
	}
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("set private file permissions %q: %w", name, err)
	}
	if created {
		if err := ops.syncFile(file); err != nil {
			return fmt.Errorf("sync private file %q: %w", name, err)
		}
		if err := ops.syncDir(directory.file); err != nil {
			return fmt.Errorf("sync private file directory: %w", err)
		}
	}
	return nil
}

func verifyOwnedRegular(file *os.File, uid uint32) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("file must be regular")
	}
	if stat.Uid != uid {
		return errors.New("file must be owned by the current user")
	}
	if stat.Nlink != 1 {
		return errors.New("file must not have multiple hard links")
	}
	return nil
}

// CommitUncertainError reports that recovery after a post-rename durability
// failure could not itself be proven durable.
type CommitUncertainError struct{ Err error }

func (err *CommitUncertainError) Error() string {
	return fmt.Sprintf("LaunchAgent plist commit may have changed: %v", err.Err)
}

func (err *CommitUncertainError) Unwrap() error { return err.Err }

func writeAtomic(directory *secureDir, name string, contents []byte, ops fileOps) error {
	oldContents, existed, err := readExistingRegular(directory, name, true)
	if err != nil {
		return err
	}
	if err := writeReplacement(directory, name, contents, ops); err != nil {
		var postRename *postRenameError
		if !errors.As(err, &postRename) {
			return err
		}
		if recoveryErr := restoreReplacement(directory, name, oldContents, existed, ops); recoveryErr != nil {
			return &CommitUncertainError{Err: errors.Join(postRename.Err, recoveryErr)}
		}
		return postRename.Err
	}
	return nil
}

type postRenameError struct{ Err error }

func (err *postRenameError) Error() string { return err.Err.Error() }
func (err *postRenameError) Unwrap() error { return err.Err }

func temporaryName(name string) (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "." + name + ".tmp-" + hex.EncodeToString(random[:]), nil
}

func writeReplacement(directory *secureDir, name string, contents []byte, ops fileOps) error {
	var temporaryNameValue string
	var fd int
	var err error
	for attempts := 0; attempts < 10; attempts++ {
		temporaryNameValue, err = temporaryName(name)
		if err != nil {
			return fmt.Errorf("name temporary plist: %w", err)
		}
		fd, err = unix.Openat(directory.fd(), temporaryNameValue, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if !errors.Is(err, unix.EEXIST) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("create temporary plist: %w", err)
	}
	temporary := os.NewFile(uintptr(fd), temporaryNameValue)
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = ops.unlinkAt(directory.fd(), temporaryNameValue, 0)
		}
	}()
	if err := verifyOwnedRegular(temporary, directory.uid); err != nil {
		return fmt.Errorf("validate temporary plist: %w", err)
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
	if err := ops.renameAt(directory.fd(), temporaryNameValue, directory.fd(), name); err != nil {
		return fmt.Errorf("replace LaunchAgent plist: %w", err)
	}
	committed = true
	if err := ops.syncDir(directory.file); err != nil {
		return &postRenameError{Err: fmt.Errorf("sync LaunchAgents directory: %w", err)}
	}
	return nil
}

func restoreReplacement(directory *secureDir, name string, oldContents []byte, existed bool, ops fileOps) error {
	if existed {
		if err := writeReplacement(directory, name, oldContents, ops); err != nil {
			return fmt.Errorf("restore prior LaunchAgent plist: %w", err)
		}
		return nil
	}
	if err := ops.unlinkAt(directory.fd(), name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove uncommitted LaunchAgent plist: %w", err)
	}
	if err := ops.syncDir(directory.file); err != nil {
		return fmt.Errorf("sync restored LaunchAgents directory: %w", err)
	}
	return nil
}

func removeDurable(directory *secureDir, name string, ops fileOps) error {
	oldContents, existed, err := readExistingRegular(directory, name, true)
	if err != nil {
		return err
	}
	if !existed {
		return nil
	}
	if err := ops.unlinkAt(directory.fd(), name, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("remove LaunchAgent plist: %w", err)
	}
	if err := ops.syncDir(directory.file); err != nil {
		if recoveryErr := restoreReplacement(directory, name, oldContents, true, ops); recoveryErr != nil {
			return &CommitUncertainError{Err: errors.Join(fmt.Errorf("sync LaunchAgents directory after removal: %w", err), recoveryErr)}
		}
		return fmt.Errorf("sync LaunchAgents directory after removal: %w", err)
	}
	return nil
}

func readExistingRegular(directory *secureDir, name string, forcePrivate bool) ([]byte, bool, error) {
	fd, err := unix.Openat(directory.fd(), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open LaunchAgent plist: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	if err := verifyOwnedRegular(file, directory.uid); err != nil {
		return nil, false, fmt.Errorf("validate LaunchAgent plist: %w", err)
	}
	if forcePrivate {
		if err := file.Chmod(0o600); err != nil {
			return nil, false, fmt.Errorf("set LaunchAgent plist permissions: %w", err)
		}
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		return nil, false, fmt.Errorf("read LaunchAgent plist: %w", err)
	}
	return contents, true, nil
}

func installedPlist(directory *secureDir, name string) (bool, error) {
	fd, err := unix.Openat(directory.fd(), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open LaunchAgent plist: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	if err := verifyOwnedRegular(file, directory.uid); err != nil {
		return false, fmt.Errorf("validate LaunchAgent plist: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if info.Mode().Perm() != 0o600 {
		return false, errors.New("LaunchAgent plist has unsafe permissions")
	}
	return true, nil
}
