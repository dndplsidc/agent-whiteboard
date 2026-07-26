//go:build darwin || linux

package config

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

type editLock struct {
	file *os.File
}

func acquireEditLock(path string) (*editLock, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
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
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("lock %q must be a regular file", path)
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, err
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return nil, err
	}
	failed = false
	return &editLock{file: file}, nil
}

func (lock *editLock) release() {
	if lock == nil || lock.file == nil {
		return
	}
	_ = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	_ = lock.file.Close()
}

func openRegularNoFollow(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("open configuration file")
	}
	return file, nil
}
