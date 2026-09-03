package cursor

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"golang.org/x/sys/unix"
)

func TestReadImageInputRequiresSecureWorkspaceBoundRegularFile(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Chmod(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	attachments := filepath.Join(workspace, "attachments")
	if err := os.Mkdir(attachments, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(attachments, "image.png")
	content := []byte("private image")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	image := provider.ImageInput{ID: turnID, Name: "image.png", MediaType: "image/png", Bytes: int64(len(content)), Path: path}
	read, err := readImageInput(workspace, image)
	if err != nil || string(read) != string(content) {
		t.Fatalf("read = %q, err = %v", read, err)
	}
	wipe(read)

	outside := filepath.Join(t.TempDir(), "outside.png")
	if err = os.WriteFile(outside, content, 0o600); err != nil {
		t.Fatal(err)
	}
	image.Path = outside
	if _, err = readImageInput(workspace, image); err == nil {
		t.Fatal("outside image accepted")
	}

	link := filepath.Join(attachments, "link.png")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	image.Path = link
	if _, err = readImageInput(workspace, image); err == nil {
		t.Fatal("symlink image accepted")
	}
}

func TestReadImageInputRejectsInsecureAncestorAndHardLink(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Chmod(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	insecure := filepath.Join(workspace, "insecure")
	if err := os.Mkdir(insecure, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(insecure, "image.png")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	image := provider.ImageInput{ID: turnID, Name: "image.png", MediaType: "image/png", Bytes: 1, Path: path}
	if _, err := readImageInput(workspace, image); err == nil {
		t.Fatal("insecure ancestor accepted")
	}
	if err := os.Chmod(insecure, 0o700); err != nil {
		t.Fatal(err)
	}
	hardLink := filepath.Join(insecure, "hard.png")
	if err := os.Link(path, hardLink); err != nil {
		t.Fatal(err)
	}
	if _, err := readImageInput(workspace, image); err == nil {
		t.Fatal("multiply linked image accepted")
	}
}

type postOpenImageFS struct {
	imageFilesystem
	target    string
	afterOpen func()
	opened    bool
}

type postReadImageFS struct {
	imageFilesystem
	mutate  func()
	mutated bool
	buffers [][]byte
}

func (fs *postReadImageFS) read(file *os.File, buffer []byte) (int, error) {
	n, err := fs.imageFilesystem.read(file, buffer)
	if n > 0 && !fs.mutated {
		fs.buffers = append(fs.buffers, buffer[:n])
		fs.mutated = true
		fs.mutate()
	}
	return n, err
}

type wrongOwnerImageFS struct {
	imageFilesystem
	target string
}

func (fs *wrongOwnerImageFS) fstat(file *os.File, stat *unix.Stat_t) error {
	err := fs.imageFilesystem.fstat(file, stat)
	if err == nil && fs.target == "workspace" {
		stat.Uid++
	}
	return err
}

func (fs *wrongOwnerImageFS) fstatAt(parent *os.File, name string, stat *unix.Stat_t, flags int) error {
	err := fs.imageFilesystem.fstatAt(parent, name, stat, flags)
	if err == nil && ((fs.target == "ancestor" && name == "attachments") || (fs.target == "final" && name == "image.png")) {
		stat.Uid++
	}
	return err
}

func (fs *postOpenImageFS) openAt(parent *os.File, name string, flags int) (*os.File, error) {
	file, err := fs.imageFilesystem.openAt(parent, name, flags)
	if err == nil && !fs.opened && name == fs.target {
		fs.opened = true
		fs.afterOpen()
	}
	return file, err
}

func TestReadImageInputRejectsPostOpenNameAndAncestorSubstitution(t *testing.T) {
	newFixture := func(t *testing.T) (string, string, provider.ImageInput) {
		t.Helper()
		workspace := t.TempDir()
		attachments := filepath.Join(workspace, "attachments")
		if err := os.Mkdir(attachments, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(attachments, "image.png")
		content := []byte("old")
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		return workspace, path, provider.ImageInput{ID: turnID, Name: "image.png", MediaType: "image/png", Bytes: int64(len(content)), Path: path}
	}

	t.Run("final name", func(t *testing.T) {
		workspace, path, image := newFixture(t)
		fs := &postOpenImageFS{imageFilesystem: unixImageFilesystem{}, target: "image.png", afterOpen: func() {
			if err := os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
		}}
		if _, err := readImageInputWithFS(workspace, image, fs); err == nil {
			t.Fatal("final name substitution accepted")
		}
	})

	t.Run("ancestor", func(t *testing.T) {
		workspace, path, image := newFixture(t)
		attachments := filepath.Dir(path)
		fs := &postOpenImageFS{imageFilesystem: unixImageFilesystem{}, target: "attachments", afterOpen: func() {
			old := attachments + ".old"
			if err := os.Rename(attachments, old); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(attachments, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(attachments, "image.png"), []byte("new"), 0o600); err != nil {
				t.Fatal(err)
			}
		}}
		if _, err := readImageInputWithFS(workspace, image, fs); err == nil {
			t.Fatal("ancestor substitution accepted")
		}
	})
}

func TestReadImageInputRejectsInjectedWrongOwnerStat(t *testing.T) {
	for _, target := range []string{"workspace", "ancestor", "final"} {
		t.Run(target, func(t *testing.T) {
			workspace := t.TempDir()
			attachments := filepath.Join(workspace, "attachments")
			if err := os.Mkdir(attachments, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(attachments, "image.png")
			content := []byte("old")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			image := provider.ImageInput{ID: turnID, Name: "image.png", MediaType: "image/png", Bytes: int64(len(content)), Path: path}
			fs := &wrongOwnerImageFS{imageFilesystem: unixImageFilesystem{}, target: target}
			if _, err := readImageInputWithFS(workspace, image, fs); err == nil {
				t.Fatal("wrong-owner stat accepted")
			}
		})
	}
}

func TestReadImageInputRejectsWorkspaceNameBindingSubstitution(t *testing.T) {
	workspace := t.TempDir()
	attachments := filepath.Join(workspace, "attachments")
	if err := os.Mkdir(attachments, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(attachments, "image.png")
	content := []byte("old")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	image := provider.ImageInput{ID: turnID, Name: "image.png", MediaType: "image/png", Bytes: int64(len(content)), Path: path}
	fs := &postOpenImageFS{imageFilesystem: unixImageFilesystem{}, target: filepath.Base(workspace), afterOpen: func() {
		old := workspace + ".old"
		if err := os.Rename(workspace, old); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(workspace, 0o700); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := readImageInputWithFS(workspace, image, fs); err == nil {
		t.Fatal("workspace-name substitution accepted")
	}
}

func TestReadImageInputWipesBytesAfterPostReadMutation(t *testing.T) {
	workspace := t.TempDir()
	attachments := filepath.Join(workspace, "attachments")
	if err := os.Mkdir(attachments, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(attachments, "image.png")
	content := []byte("old")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	image := provider.ImageInput{ID: turnID, Name: "image.png", MediaType: "image/png", Bytes: int64(len(content)), Path: path}
	fs := &postReadImageFS{imageFilesystem: unixImageFilesystem{}, mutate: func() {
		if err := os.WriteFile(path, []byte("new"), 0o600); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := readImageInputWithFS(workspace, image, fs); err == nil {
		t.Fatal("same-size rewrite accepted")
	}
	for _, buffer := range fs.buffers {
		if !bytes.Equal(buffer, make([]byte, len(buffer))) {
			t.Fatalf("raw image bytes not wiped: %q", buffer)
		}
	}
}

func TestReadImageInputWipesBytesAfterPostReadSizeChange(t *testing.T) {
	workspace := t.TempDir()
	attachments := filepath.Join(workspace, "attachments")
	if err := os.Mkdir(attachments, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(attachments, "image.png")
	content := []byte("old")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	image := provider.ImageInput{ID: turnID, Name: "image.png", MediaType: "image/png", Bytes: int64(len(content)), Path: path}
	fs := &postReadImageFS{imageFilesystem: unixImageFilesystem{}, mutate: func() {
		if err := os.Truncate(path, 1); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := readImageInputWithFS(workspace, image, fs); err == nil {
		t.Fatal("post-read size change accepted")
	}
	for _, buffer := range fs.buffers {
		if !bytes.Equal(buffer, make([]byte, len(buffer))) {
			t.Fatalf("raw image bytes not wiped: %q", buffer)
		}
	}
}

func TestReadImageInputRejectsPostOpenMetadataMutation(t *testing.T) {
	cases := []struct {
		name   string
		target string
		mutate func(*testing.T, string)
	}{
		{name: "chmod file", target: "image.png", mutate: func(t *testing.T, path string) {
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "link file", target: "image.png", mutate: func(t *testing.T, path string) {
			if err := os.Link(path, path+".alias"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "chmod ancestor", target: "attachments", mutate: func(t *testing.T, path string) {
			if err := os.Chmod(path, 0o750); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			attachments := filepath.Join(workspace, "attachments")
			if err := os.Mkdir(attachments, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(attachments, "image.png")
			content := []byte("old")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			image := provider.ImageInput{ID: turnID, Name: "image.png", MediaType: "image/png", Bytes: int64(len(content)), Path: path}
			mutatePath := path
			if tc.target == "attachments" {
				mutatePath = attachments
			}
			fs := &postOpenImageFS{imageFilesystem: unixImageFilesystem{}, target: tc.target, afterOpen: func() { tc.mutate(t, mutatePath) }}
			if _, err := readImageInputWithFS(workspace, image, fs); err == nil {
				t.Fatal("post-open metadata mutation accepted")
			}
		})
	}
}
