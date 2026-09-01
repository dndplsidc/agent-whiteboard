//go:build unix

package cursor

import (
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"golang.org/x/sys/unix"
)

const (
	imageDirectoryOpenFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	imageFileOpenFlags      = unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
)

// imageFilesystem is deliberately an instance dependency. Besides keeping the
// production implementation small, this lets security tests arrange a
// deterministic substitution immediately after an open without a mutable
// process-global hook.
type imageFilesystem interface {
	open(path string, flags int) (*os.File, error)
	openAt(parent *os.File, name string, flags int) (*os.File, error)
	lstat(path string, stat *unix.Stat_t) error
	fstat(file *os.File, stat *unix.Stat_t) error
	fstatAt(parent *os.File, name string, stat *unix.Stat_t, flags int) error
	read(file *os.File, buffer []byte) (int, error)
}

type unixImageFilesystem struct{}

func (unixImageFilesystem) open(path string, flags int) (*os.File, error) {
	fd, err := unix.Open(path, flags, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open Cursor image descriptor")
	}
	return file, nil
}

func (unixImageFilesystem) openAt(parent *os.File, name string, flags int) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, flags, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open Cursor image descriptor")
	}
	return file, nil
}

func (unixImageFilesystem) lstat(path string, stat *unix.Stat_t) error {
	return unix.Lstat(path, stat)
}
func (unixImageFilesystem) fstat(file *os.File, stat *unix.Stat_t) error {
	return unix.Fstat(int(file.Fd()), stat)
}
func (unixImageFilesystem) fstatAt(parent *os.File, name string, stat *unix.Stat_t, flags int) error {
	return unix.Fstatat(int(parent.Fd()), name, stat, flags)
}
func (unixImageFilesystem) read(file *os.File, buffer []byte) (int, error) {
	return file.Read(buffer)
}

type imageBinding struct {
	parent  *os.File
	name    string
	opened  *os.File
	initial unix.Stat_t
	isFinal bool
}

func buildPromptBlocks(workspace string, envelope []byte, images []provider.ImageInput) ([]any, error) {
	blocks := make([]any, 0, len(images)+1)
	blocks = append(blocks, map[string]any{"type": "text", "text": string(envelope)})
	for _, image := range images {
		content, err := readImageInput(workspace, image)
		if err != nil {
			return nil, err
		}
		encoded := base64.StdEncoding.EncodeToString(content)
		wipe(content)
		blocks = append(blocks, map[string]any{"type": "image", "mimeType": image.MediaType, "data": encoded})
	}
	return blocks, nil
}

func readImageInput(workspace string, image provider.ImageInput) ([]byte, error) {
	return readImageInputWithFS(workspace, image, unixImageFilesystem{})
}

func readImageInputWithFS(workspace string, image provider.ImageInput, fs imageFilesystem) ([]byte, error) {
	if image.Validate() != nil || !absolute(workspace) || fs == nil {
		return nil, errors.New("invalid Cursor image input")
	}
	relative, err := filepath.Rel(workspace, image.Path)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, errors.New("Cursor image is outside workspace")
	}
	parts := splitPath(relative)
	if len(parts) == 0 {
		return nil, errors.New("invalid Cursor image input")
	}

	// Open the workspace's parent and retain its name binding for the entire
	// read. This closes the gap where the workspace pathname itself is replaced
	// after its descriptor is opened.
	workspaceParent, err := fs.open(filepath.Dir(workspace), imageDirectoryOpenFlags)
	if err != nil {
		return nil, errors.New("unsafe Cursor workspace")
	}
	defer workspaceParent.Close()
	workspaceName := filepath.Base(workspace)
	workspaceDir, err := fs.openAt(workspaceParent, workspaceName, imageDirectoryOpenFlags)
	if err != nil {
		return nil, errors.New("unsafe Cursor workspace")
	}
	defer workspaceDir.Close()
	var workspaceStat, workspaceBindingStat unix.Stat_t
	if err := fs.fstat(workspaceDir, &workspaceStat); err != nil || !secureDirectoryStat(&workspaceStat) || fs.fstatAt(workspaceParent, workspaceName, &workspaceBindingStat, unix.AT_SYMLINK_NOFOLLOW) != nil || !secureDirectoryStat(&workspaceBindingStat) || !sameCursorStat(&workspaceBindingStat, &workspaceStat) {
		return nil, errors.New("unsafe Cursor workspace")
	}
	// Opening with O_NOFOLLOW protects the descriptor. Pairing it with lstat
	// protects the configured workspace name from being replaced while opening.
	var workspacePathStat unix.Stat_t
	if err := fs.lstat(workspace, &workspacePathStat); err != nil || !secureDirectoryStat(&workspacePathStat) || !sameCursorStat(&workspacePathStat, &workspaceStat) {
		return nil, errors.New("unsafe Cursor workspace")
	}

	bindings := make([]imageBinding, 0, len(parts))
	parent := workspaceDir
	for _, name := range parts[:len(parts)-1] {
		var before unix.Stat_t
		if err := fs.fstatAt(parent, name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil || !secureDirectoryStat(&before) {
			closeImageBindings(bindings)
			return nil, errors.New("unsafe Cursor image ancestor")
		}
		child, openErr := fs.openAt(parent, name, imageDirectoryOpenFlags)
		if openErr != nil {
			closeImageBindings(bindings)
			return nil, errors.New("open Cursor image ancestor")
		}
		var opened unix.Stat_t
		if err := fs.fstat(child, &opened); err != nil || !secureDirectoryStat(&opened) || !sameCursorStat(&before, &opened) {
			_ = child.Close()
			closeImageBindings(bindings)
			return nil, errors.New("substituted Cursor image ancestor")
		}
		bindings = append(bindings, imageBinding{parent: parent, name: name, opened: child, initial: before})
		parent = child
	}

	finalName := parts[len(parts)-1]
	var before unix.Stat_t
	if err := fs.fstatAt(parent, finalName, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil || !secureImageFileStat(&before, image.Bytes) {
		closeImageBindings(bindings)
		return nil, errors.New("unsafe Cursor image input")
	}
	file, openErr := fs.openAt(parent, finalName, imageFileOpenFlags)
	if openErr != nil {
		closeImageBindings(bindings)
		return nil, errors.New("open Cursor image input")
	}
	bindings = append(bindings, imageBinding{parent: parent, name: finalName, opened: file, initial: before, isFinal: true})
	defer closeImageBindings(bindings)

	var opened unix.Stat_t
	if err := fs.fstat(file, &opened); err != nil || !secureImageFileStat(&opened, image.Bytes) || !sameCursorStat(&before, &opened) {
		return nil, errors.New("substituted Cursor image input")
	}
	content, readErr := io.ReadAll(io.LimitReader(imageFileReader{fs: fs, file: file}, image.Bytes+1))
	if readErr != nil || int64(len(content)) != image.Bytes {
		wipe(content)
		return nil, errors.New("read Cursor image input")
	}
	if err := verifyImageBindings(fs, workspace, workspaceParent, workspaceName, workspaceDir, &workspaceStat, bindings, &before, file, image.Bytes); err != nil {
		wipe(content)
		return nil, err
	}
	return content, nil
}

func verifyImageBindings(fs imageFilesystem, workspace string, workspaceParent *os.File, workspaceName string, workspaceDir *os.File, workspaceInitial *unix.Stat_t, bindings []imageBinding, finalInitial *unix.Stat_t, final *os.File, size int64) error {
	var workspaceCurrent, workspacePathCurrent, workspaceBindingCurrent unix.Stat_t
	if err := fs.fstat(workspaceDir, &workspaceCurrent); err != nil || !secureDirectoryStat(&workspaceCurrent) || !sameCursorStat(workspaceInitial, &workspaceCurrent) {
		return errors.New("mutated Cursor workspace")
	}
	if err := fs.fstatAt(workspaceParent, workspaceName, &workspaceBindingCurrent, unix.AT_SYMLINK_NOFOLLOW); err != nil || !secureDirectoryStat(&workspaceBindingCurrent) || !sameCursorStat(workspaceInitial, &workspaceBindingCurrent) {
		return errors.New("mutated Cursor workspace binding")
	}
	if err := fs.lstat(workspace, &workspacePathCurrent); err != nil || !secureDirectoryStat(&workspacePathCurrent) || !sameCursorStat(workspaceInitial, &workspacePathCurrent) {
		return errors.New("substituted Cursor workspace")
	}
	for _, binding := range bindings {
		var opened, named unix.Stat_t
		if err := fs.fstat(binding.opened, &opened); err != nil {
			return errors.New("stat Cursor image descriptor")
		}
		if err := fs.fstatAt(binding.parent, binding.name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return errors.New("stat Cursor image binding")
		}
		if binding.isFinal {
			if !secureImageFileStat(&opened, size) || !secureImageFileStat(&named, size) || !sameCursorStat(&opened, finalInitial) || !sameCursorStat(&named, finalInitial) || !sameCursorStat(&opened, &named) {
				return errors.New("mutated Cursor image input")
			}
		} else {
			if !secureDirectoryStat(&opened) || !secureDirectoryStat(&named) || !sameCursorStat(&opened, &binding.initial) || !sameCursorStat(&named, &binding.initial) || !sameCursorStat(&opened, &named) {
				return errors.New("mutated Cursor image ancestor")
			}
		}
	}
	// Keep the explicit descriptor check separate from the name binding check;
	// this catches a changed size even if the final name was removed/recreated.
	var finalCurrent unix.Stat_t
	if err := fs.fstat(final, &finalCurrent); err != nil || !secureImageFileStat(&finalCurrent, size) || !sameCursorStat(finalInitial, &finalCurrent) {
		return errors.New("mutated Cursor image input")
	}
	return nil
}

func closeImageBindings(bindings []imageBinding) {
	for index := len(bindings) - 1; index >= 0; index-- {
		if bindings[index].opened != nil {
			_ = bindings[index].opened.Close()
		}
	}
}

func secureDirectoryStat(stat *unix.Stat_t) bool {
	return stat != nil && stat.Mode&unix.S_IFMT == unix.S_IFDIR && stat.Uid == uint32(os.Geteuid()) && stat.Mode&0o077 == 0 && stat.Mode&0o100 != 0
}

func secureImageFileStat(stat *unix.Stat_t, size int64) bool {
	return stat != nil && stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Uid == uint32(os.Geteuid()) && uint64(stat.Nlink) == 1 && stat.Mode&0o077 == 0 && stat.Mode&0o400 != 0 && stat.Size == size
}

func sameCursorStat(left, right *unix.Stat_t) bool {
	return left != nil && right != nil && left.Mode&unix.S_IFMT == right.Mode&unix.S_IFMT && left.Mode&0o777 == right.Mode&0o777 && left.Uid == right.Uid && left.Size == right.Size && left.Nlink == right.Nlink && left.Dev == right.Dev && left.Ino == right.Ino && sameCursorStatTimes(left, right)
}

// imageFileReader keeps reads behind the same injected filesystem boundary as
// descriptor operations. This is also what lets deterministic tests verify
// that bytes are wiped when post-read metadata validation rejects them.
type imageFileReader struct {
	fs   imageFilesystem
	file *os.File
}

func (reader imageFileReader) Read(buffer []byte) (int, error) {
	return reader.fs.read(reader.file, buffer)
}

func secureDirectory(info os.FileInfo) bool {
	return info != nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o077 == 0
}

func splitPath(path string) []string {
	path = filepath.Clean(path)
	if path == "." || path == string(filepath.Separator) || path == "" {
		return nil
	}
	parts := strings.Split(path, string(filepath.Separator))
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" && part != "." && part != ".." {
			result = append(result, part)
		}
	}
	return result
}

func wipe(content []byte) {
	for index := range content {
		content[index] = 0
	}
}
