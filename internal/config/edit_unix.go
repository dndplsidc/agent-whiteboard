//go:build darwin || linux

package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type configDirectory struct {
	file *os.File
}

type fileIdentity struct {
	device uint64
	inode  uint64
}

type editLock struct {
	file *os.File
}

func openConfigDirectory(path string) (*configDirectory, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open configuration directory")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.IsDir() {
		_ = file.Close()
		return nil, errors.New("configuration parent must be a directory")
	}
	if info.Mode().Perm()&0o022 != 0 {
		_ = file.Close()
		return nil, errors.New("configuration parent must not be writable by group or others")
	}
	return &configDirectory{file: file}, nil
}

func (directory *configDirectory) close() error {
	if directory == nil || directory.file == nil {
		return nil
	}
	return directory.file.Close()
}

func (directory *configDirectory) fd() int {
	return int(directory.file.Fd())
}

func (directory *configDirectory) openTarget(name string) (*os.File, error) {
	fd, err := unix.Openat(directory.fd(), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open configuration file")
	}
	return file, nil
}

func (directory *configDirectory) targetIdentity(name string) (fileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(directory.fd(), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fileIdentity{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fileIdentity{}, errors.New("configuration must be a regular file")
	}
	if stat.Mode&0o022 != 0 {
		return fileIdentity{}, errors.New("configuration must not be writable by group or others")
	}
	return fileIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}, nil
}

func identityForFile(file *os.File) (fileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return fileIdentity{}, err
	}
	return fileIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}, nil
}

func (directory *configDirectory) createTemporary(target string) (*os.File, string, error) {
	for attempt := 0; attempt < 128; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", err
		}
		name := "." + target + ".tmp-" + hex.EncodeToString(random[:])
		fd, err := unix.Openat(directory.fd(), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
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
			return nil, "", errors.New("create temporary configuration")
		}
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			_ = unix.Unlinkat(directory.fd(), name, 0)
			return nil, "", err
		}
		return file, name, nil
	}
	return nil, "", errors.New("create unique temporary configuration")
}

func (directory *configDirectory) rename(oldName, newName string) error {
	return unix.Renameat(directory.fd(), oldName, directory.fd(), newName)
}

func (directory *configDirectory) unlink(name string) error {
	return unix.Unlinkat(directory.fd(), name, 0)
}

func (directory *configDirectory) sync() error {
	return unix.Fsync(directory.fd())
}

func acquireEditLock(directory *configDirectory, name string) (*editLock, error) {
	fd := -1
	var err error
	for attempt := 0; attempt < 128; attempt++ {
		fd, err = unix.Openat(directory.fd(), name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("open lock: %w", err)
		}
		fd, err = unix.Openat(directory.fd(), name, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EEXIST) && !errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("create lock: %w", err)
		}
	}
	if fd < 0 {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open lock file")
	}
	failed := true
	defer func() {
		if failed {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect lock: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("lock %q must be a regular file", name)
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("secure lock: %w", err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	failed = false
	return &editLock{file: file}, nil
}

func (lock *editLock) release() {
	if lock == nil || lock.file == nil {
		return
	}
	_ = unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	_ = lock.file.Close()
}
