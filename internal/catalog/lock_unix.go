//go:build darwin || linux

package catalog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type fileLock struct{ file *os.File }

func openRecordFile(root recordRoot, name string) (*os.File, error) {
	// Atomic replacement may change the inode after Lstat. Reject symlinks at
	// open itself, then validate the opened file rather than the previous inode.
	// Nonblocking open also prevents a raced-in FIFO from blocking the reader.
	// os.Root resolves in-root symlinks itself, even with O_NOFOLLOW. Use
	// openat relative to its directory descriptor and only a single filename.
	if filepath.Base(name) != name || name == "." || name == ".." {
		return nil, errors.New("invalid catalog record filename")
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func acquireFileLock(ctx context.Context, root *os.Root, name string) (*fileLock, error) {
	file, err := openLockFile(root, name)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return &fileLock{file: file}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func openLockFile(root *os.Root, name string) (*os.File, error) {
	created := false
	file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, fileMode)
	if errors.Is(err, os.ErrExist) {
		before, statErr := root.Lstat(name)
		if statErr != nil || !privateRegular(before) {
			return nil, errors.New("catalog lock must be private, regular, and not a symbolic link")
		}
		file, err = root.OpenFile(name, os.O_RDWR, fileMode)
		if err == nil {
			opened, inspectErr := file.Stat()
			if inspectErr != nil || !privateRegular(opened) || !os.SameFile(before, opened) {
				_ = file.Close()
				return nil, errors.New("catalog lock changed while opening")
			}
		}
	} else if err == nil {
		created = true
		if chmodErr := file.Chmod(fileMode); chmodErr != nil {
			_ = file.Close()
			return nil, chmodErr
		}
		opened, inspectErr := file.Stat()
		if inspectErr != nil || !privateRegular(opened) || opened.Mode().Perm() != fileMode {
			_ = file.Close()
			return nil, errors.New("catalog lock must be private and regular")
		}
	}
	if created {
		if syncErr := syncDirectory(root); syncErr != nil {
			_ = file.Close()
			return nil, syncErr
		}
	}
	return file, err
}

func (lock *fileLock) release() {
	if lock == nil || lock.file == nil {
		return
	}
	_ = unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	_ = lock.file.Close()
}

func syncDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}
