//go:build darwin

package common

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type fileOps struct {
	syncFile          func(*os.File) error
	syncDir           func(*os.File) error
	exchangeAt        func(int, string, int, string) error
	renameNoReplaceAt func(int, string, int, string) error
	unlinkAt          func(int, string, int) error
}

func defaultFileOps() fileOps {
	return fileOps{
		syncFile: func(file *os.File) error { return file.Sync() },
		syncDir:  func(directory *os.File) error { return directory.Sync() },
		exchangeAt: func(fromFD int, from string, toFD int, to string) error {
			return unix.RenameatxNp(fromFD, from, toFD, to, unix.RENAME_SWAP)
		},
		renameNoReplaceAt: func(fromFD int, from string, toFD int, to string) error {
			return unix.RenameatxNp(fromFD, from, toFD, to, unix.RENAME_EXCL)
		},
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

type boundFile struct {
	file      *os.File
	directory *secureDir
	name      string
}

func (binding *boundFile) close() { _ = binding.file.Close() }

func (binding *boundFile) verify() error {
	current, err := openOwnedRegularAt(binding.directory, binding.name)
	if err != nil {
		return err
	}
	defer current.Close()
	expectedInfo, err := binding.file.Stat()
	if err != nil {
		return err
	}
	currentInfo, err := current.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(expectedInfo, currentInfo) {
		return errors.New("file changed during operation")
	}
	return nil
}

func ensurePrivateFile(directory *secureDir, name string, ops fileOps) (*boundFile, error) {
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
	fd, err := unix.Openat(directory.fd(), name, flags|unix.O_CREAT|unix.O_EXCL, 0o600)
	created := err == nil
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(directory.fd(), name, flags, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("open private file %q: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	if err := verifyOwnedRegular(file, directory.uid); err != nil {
		file.Close()
		return nil, fmt.Errorf("validate private file %q: %w", name, err)
	}
	if err := ensurePrivateMode(file, ops); err != nil {
		file.Close()
		return nil, fmt.Errorf("set private file permissions %q: %w", name, err)
	}
	if created {
		if err := ops.syncFile(file); err != nil {
			file.Close()
			return nil, fmt.Errorf("sync private file %q: %w", name, err)
		}
		if err := ops.syncDir(directory.file); err != nil {
			file.Close()
			return nil, fmt.Errorf("sync private file directory: %w", err)
		}
	}
	return &boundFile{file: file, directory: directory, name: name}, nil
}

func ensurePrivateMode(file *os.File, ops fileOps) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Mode().Perm() == 0o600 {
		return nil
	}
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	return ops.syncFile(file)
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

// LaunchAgentCommitUncertainError reports that recovery after a publication failure could
// not itself be proven durable.
type LaunchAgentCommitUncertainError struct{ Err error }

func (err *LaunchAgentCommitUncertainError) Error() string {
	return fmt.Sprintf("LaunchAgent plist commit may have changed: %v", err.Err)
}

func (err *LaunchAgentCommitUncertainError) Unwrap() error { return err.Err }

type plistPublication struct {
	directory  *secureDir
	name       string
	ops        fileOps
	published  *os.File
	prior      *os.File
	priorName  string
	priorExist bool
	active     bool
}

func (publication *plistPublication) close() {
	if publication.published != nil {
		_ = publication.published.Close()
	}
	if publication.prior != nil {
		_ = publication.prior.Close()
	}
}

func (publication *plistPublication) verify() error {
	if err := verifyNameBinding(publication.directory, publication.name, publication.published); err != nil {
		return fmt.Errorf("published LaunchAgent plist changed during operation: %w", err)
	}
	return nil
}

func (publication *plistPublication) commit() error {
	if !publication.active {
		return nil
	}
	if !publication.priorExist {
		publication.active = false
		return nil
	}
	state, err := removeNameCAS(publication.directory, publication.priorName, publication.prior, publication.ops)
	if err != nil {
		publication.active = false
		return &LaunchAgentCommitUncertainError{Err: fmt.Errorf("remove prior LaunchAgent plist backup: %w", err)}
	}
	if state.sourcePresent {
		publication.active = false
		return &LaunchAgentCommitUncertainError{Err: errors.New("prior LaunchAgent plist backup removal did not complete")}
	}
	publication.active = false
	if err := publication.ops.syncDir(publication.directory.file); err != nil {
		return &LaunchAgentCommitUncertainError{Err: fmt.Errorf("sync LaunchAgents directory after backup removal: %w", err)}
	}
	return nil
}

func (publication *plistPublication) rollback() error {
	if !publication.active {
		return nil
	}
	if publication.priorExist {
		if err := publication.ops.exchangeAt(publication.directory.fd(), publication.priorName, publication.directory.fd(), publication.name); err != nil {
			return fmt.Errorf("restore prior LaunchAgent plist: %w", err)
		}
		restoredErr := verifyNameBinding(publication.directory, publication.name, publication.prior)
		displacedErr := verifyNameBinding(publication.directory, publication.priorName, publication.published)
		if restoredErr != nil || displacedErr != nil {
			if restoreErr := publication.ops.exchangeAt(publication.directory.fd(), publication.priorName, publication.directory.fd(), publication.name); restoreErr != nil {
				return &LaunchAgentCommitUncertainError{Err: errors.Join(errors.New("LaunchAgent plist changed before rollback"), restoredErr, displacedErr, restoreErr)}
			}
			return errors.Join(errors.New("LaunchAgent plist changed before rollback"), restoredErr, displacedErr)
		}
		publication.active = false
		if err := publication.ops.syncDir(publication.directory.file); err != nil {
			return &LaunchAgentCommitUncertainError{Err: fmt.Errorf("sync restored LaunchAgents directory: %w", err)}
		}
		if _, err := removeNameCAS(publication.directory, publication.priorName, publication.published, publication.ops); err != nil {
			return fmt.Errorf("remove rolled-back LaunchAgent plist: %w", err)
		}
		if err := publication.ops.syncDir(publication.directory.file); err != nil {
			return &LaunchAgentCommitUncertainError{Err: fmt.Errorf("sync LaunchAgents directory after rollback cleanup: %w", err)}
		}
		return nil
	}

	state, err := removeNameCAS(publication.directory, publication.name, publication.published, publication.ops)
	if err != nil {
		return fmt.Errorf("remove newly published LaunchAgent plist: %w", err)
	}
	if state.sourcePresent {
		return &LaunchAgentCommitUncertainError{Err: errors.New("newly published LaunchAgent plist removal did not complete")}
	}
	publication.active = false
	if err := publication.ops.syncDir(publication.directory.file); err != nil {
		return &LaunchAgentCommitUncertainError{Err: fmt.Errorf("sync restored LaunchAgents directory: %w", err)}
	}
	return nil
}

func writeAtomic(directory *secureDir, name string, contents []byte, ops fileOps) (*plistPublication, error) {
	prior, existed, err := openExistingRegular(directory, name, ops)
	if err != nil {
		return nil, err
	}
	temporary, temporaryNameValue, err := createTemporary(directory, name, contents, ops)
	if err != nil {
		if prior != nil {
			prior.Close()
		}
		return nil, err
	}
	publication := &plistPublication{
		directory: directory, name: name, ops: ops, published: temporary,
		prior: prior, priorName: temporaryNameValue, priorExist: existed,
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			publication.close()
		}
	}()
	cleanupTemporary := true
	defer func() {
		if cleanupTemporary {
			_, _ = removeNameCAS(directory, temporaryNameValue, temporary, ops)
		}
	}()

	if existed {
		if err := ops.exchangeAt(directory.fd(), temporaryNameValue, directory.fd(), name); err != nil {
			return nil, fmt.Errorf("exchange LaunchAgent plist: %w", err)
		}
		cleanupTemporary = false
		if err := verifyNameBinding(directory, temporaryNameValue, prior); err != nil {
			rollbackErr := ops.exchangeAt(directory.fd(), temporaryNameValue, directory.fd(), name)
			if rollbackErr == nil && verifyNameBinding(directory, temporaryNameValue, temporary) == nil {
				cleanupTemporary = true
			}
			if rollbackErr != nil {
				return nil, &LaunchAgentCommitUncertainError{Err: errors.Join(errors.New("LaunchAgent plist changed before publication"), err, rollbackErr)}
			}
			return nil, errors.Join(errors.New("LaunchAgent plist changed before publication"), err)
		}
	} else {
		if err := ops.renameNoReplaceAt(directory.fd(), temporaryNameValue, directory.fd(), name); err != nil {
			if errors.Is(err, unix.EEXIST) {
				return nil, errors.New("LaunchAgent plist appeared before publication")
			}
			return nil, fmt.Errorf("publish LaunchAgent plist without replacement: %w", err)
		}
		cleanupTemporary = false
	}
	publication.active = true
	if err := ops.syncDir(directory.file); err != nil {
		if rollbackErr := publication.rollback(); rollbackErr != nil {
			return nil, &LaunchAgentCommitUncertainError{Err: errors.Join(fmt.Errorf("sync LaunchAgents directory: %w", err), rollbackErr)}
		}
		return nil, fmt.Errorf("sync LaunchAgents directory: %w", err)
	}
	closeOnError = false
	return publication, nil
}

func temporaryName(name string) (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "." + name + ".tmp-" + hex.EncodeToString(random[:]), nil
}

func createTemporary(directory *secureDir, name string, contents []byte, ops fileOps) (*os.File, string, error) {
	var temporaryNameValue string
	var fd int
	var err error
	for attempts := 0; attempts < 10; attempts++ {
		temporaryNameValue, err = temporaryName(name)
		if err != nil {
			return nil, "", fmt.Errorf("name temporary plist: %w", err)
		}
		fd, err = unix.Openat(directory.fd(), temporaryNameValue, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if !errors.Is(err, unix.EEXIST) {
			break
		}
	}
	if err != nil {
		return nil, "", fmt.Errorf("create temporary plist: %w", err)
	}
	temporary := os.NewFile(uintptr(fd), temporaryNameValue)
	failed := true
	defer func() {
		if failed {
			_, _ = removeNameCAS(directory, temporaryNameValue, temporary, ops)
			temporary.Close()
		}
	}()
	if err := verifyOwnedRegular(temporary, directory.uid); err != nil {
		return nil, "", fmt.Errorf("validate temporary plist: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		return nil, "", fmt.Errorf("write temporary plist: %w", err)
	}
	if err := ops.syncFile(temporary); err != nil {
		return nil, "", fmt.Errorf("sync temporary plist: %w", err)
	}
	failed = false
	return temporary, temporaryNameValue, nil
}

func removeDurable(directory *secureDir, name string, ops fileOps) error {
	expected, existed, err := openExistingRegular(directory, name, ops)
	if err != nil {
		return err
	}
	if !existed {
		return nil
	}
	defer expected.Close()

	tombstone, tombstoneName, err := createTemporary(directory, name+".remove", nil, ops)
	if err != nil {
		return err
	}
	defer tombstone.Close()
	if err := ops.exchangeAt(directory.fd(), tombstoneName, directory.fd(), name); err != nil {
		_, cleanupErr := removeNameCAS(directory, tombstoneName, tombstone, ops)
		return errors.Join(fmt.Errorf("exchange LaunchAgent plist for removal: %w", err), cleanupErr)
	}
	if err := verifyNameBinding(directory, tombstoneName, expected); err != nil {
		rollbackErr := ops.exchangeAt(directory.fd(), tombstoneName, directory.fd(), name)
		if rollbackErr == nil && verifyNameBinding(directory, tombstoneName, tombstone) == nil {
			_, _ = removeNameCAS(directory, tombstoneName, tombstone, ops)
		}
		if rollbackErr != nil {
			return &LaunchAgentCommitUncertainError{Err: errors.Join(errors.New("LaunchAgent plist changed before removal"), err, rollbackErr)}
		}
		return errors.Join(errors.New("LaunchAgent plist changed before removal"), err)
	}

	state, removeErr := removeNameCAS(directory, name, tombstone, ops)
	if removeErr != nil {
		return restorePriorAfterRemovalError(directory, name, tombstoneName, expected, tombstone, state, removeErr, ops)
	}
	if state.sourcePresent {
		return restorePriorAfterRemovalError(directory, name, tombstoneName, expected, tombstone, state, errors.New("conditional removal did not complete"), ops)
	}
	if err := ops.syncDir(directory.file); err != nil {
		return restorePriorAfterRemovalError(directory, name, tombstoneName, expected, tombstone, state, fmt.Errorf("sync LaunchAgents directory after removal: %w", err), ops)
	}
	if _, err := removeNameCAS(directory, tombstoneName, expected, ops); err != nil {
		return fmt.Errorf("remove prior LaunchAgent plist backup: %w", err)
	}
	if err := ops.syncDir(directory.file); err != nil {
		return &LaunchAgentCommitUncertainError{Err: fmt.Errorf("sync LaunchAgents directory after backup removal: %w", err)}
	}
	return nil
}

type nameRemovalState struct {
	sourcePresent bool
}

func removeNameCAS(directory *secureDir, name string, expected *os.File, ops fileOps) (nameRemovalState, error) {
	destination, err := temporaryName(name + ".removed")
	if err != nil {
		return nameRemovalState{sourcePresent: true}, err
	}
	if err := ops.renameNoReplaceAt(directory.fd(), name, directory.fd(), destination); err != nil {
		return nameRemovalState{sourcePresent: true}, err
	}
	if err := verifyNameBinding(directory, destination, expected); err != nil {
		if restoreErr := ops.renameNoReplaceAt(directory.fd(), destination, directory.fd(), name); restoreErr != nil {
			return nameRemovalState{}, &LaunchAgentCommitUncertainError{Err: errors.Join(errors.New("file changed before conditional removal"), err, restoreErr)}
		}
		return nameRemovalState{sourcePresent: true}, errors.Join(errors.New("file changed before conditional removal"), err)
	}
	if err := ops.unlinkAt(directory.fd(), destination, 0); err != nil {
		if restoreErr := ops.renameNoReplaceAt(directory.fd(), destination, directory.fd(), name); restoreErr != nil {
			return nameRemovalState{}, &LaunchAgentCommitUncertainError{Err: errors.Join(err, restoreErr)}
		}
		return nameRemovalState{sourcePresent: true}, err
	}
	return nameRemovalState{}, nil
}

func restorePriorAfterRemovalError(directory *secureDir, name, priorName string, prior, tombstone *os.File, state nameRemovalState, cause error, ops fileOps) error {
	if err := verifyNameBinding(directory, priorName, prior); err != nil {
		return &LaunchAgentCommitUncertainError{Err: errors.Join(cause, errors.New("prior LaunchAgent plist backup changed during recovery"), err)}
	}
	if state.sourcePresent {
		if err := ops.exchangeAt(directory.fd(), priorName, directory.fd(), name); err != nil {
			return &LaunchAgentCommitUncertainError{Err: errors.Join(cause, fmt.Errorf("restore prior LaunchAgent plist by exchange: %w", err))}
		}
	} else {
		if err := ops.renameNoReplaceAt(directory.fd(), priorName, directory.fd(), name); err != nil {
			return &LaunchAgentCommitUncertainError{Err: errors.Join(cause, fmt.Errorf("restore prior LaunchAgent plist without replacement: %w", err))}
		}
	}
	if err := verifyNameBinding(directory, name, prior); err != nil {
		return &LaunchAgentCommitUncertainError{Err: errors.Join(cause, errors.New("restored LaunchAgent plist identity is uncertain"), err)}
	}
	if err := ops.syncDir(directory.file); err != nil {
		return &LaunchAgentCommitUncertainError{Err: errors.Join(cause, fmt.Errorf("sync restored LaunchAgent plist: %w", err))}
	}
	if state.sourcePresent && verifyNameBinding(directory, priorName, tombstone) == nil {
		if _, err := removeNameCAS(directory, priorName, tombstone, ops); err != nil {
			return &LaunchAgentCommitUncertainError{Err: errors.Join(cause, fmt.Errorf("remove restored tombstone: %w", err))}
		}
		if err := ops.syncDir(directory.file); err != nil {
			return &LaunchAgentCommitUncertainError{Err: errors.Join(cause, fmt.Errorf("sync restored tombstone cleanup: %w", err))}
		}
	}
	return cause
}

func verifyNameBinding(directory *secureDir, name string, expected *os.File) error {
	current, err := openOwnedRegularAt(directory, name)
	if err != nil {
		return err
	}
	defer current.Close()
	if !sameFile(current, expected) {
		return errors.New("file changed during operation")
	}
	return nil
}

func sameFile(first, second *os.File) bool {
	if first == nil || second == nil {
		return false
	}
	firstInfo, firstErr := first.Stat()
	secondInfo, secondErr := second.Stat()
	return firstErr == nil && secondErr == nil && os.SameFile(firstInfo, secondInfo)
}

func openExistingRegular(directory *secureDir, name string, ops fileOps) (*os.File, bool, error) {
	file, err := openOwnedRegularAt(directory, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open LaunchAgent plist: %w", err)
	}
	if err := ensurePrivateMode(file, ops); err != nil {
		file.Close()
		return nil, false, fmt.Errorf("set private LaunchAgent plist permissions: %w", err)
	}
	return file, true, nil
}

func openOwnedRegularAt(directory *secureDir, name string) (*os.File, error) {
	fd, err := unix.Openat(directory.fd(), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if err := verifyOwnedRegular(file, directory.uid); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
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
