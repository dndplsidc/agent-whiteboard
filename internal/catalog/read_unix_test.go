//go:build darwin || linux

package catalog

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A real root with a controlled replacement at the filesystem inspection
// boundary reproduces the interleaving between List and an atomic update.
type replacingRecordRoot struct {
	*os.Root
	replace func()
}

func (root replacingRecordRoot) Lstat(name string) (os.FileInfo, error) {
	info, err := root.Root.Lstat(name)
	root.replace()
	return info, err
}

func TestReadRecordDuringAtomicReplacement(t *testing.T) {
	for _, replacement := range []string{"record", "symlink", "public file"} {
		t.Run(replacement, func(t *testing.T) {
			store := newTestStore(t, time.Unix(2_000_000_000, 0))
			record := validRecord("https://example.test", KindMarkdown, markdownID, "Old", "Summary", "one.md", store.clock.Now(), StateCreated)
			require.NoError(t, store.SaveCreated(context.Background(), record))
			root, err := store.openKindRoot(record.Identity(), false)
			require.NoError(t, err)
			defer root.Close()
			record.Title = "New complete record"
			name := record.ID + ".json"
			boundary := replacingRecordRoot{Root: root, replace: func() {
				require.NoError(t, writeRecord(root, record))
				switch replacement {
				case "symlink":
					require.NoError(t, root.Rename(name, "target"))
					require.NoError(t, root.Symlink("target", name))
				case "public file":
					require.NoError(t, root.Chmod(name, 0o644))
				}
			}}
			got, err := readRecord(boundary, name, record.Identity())
			if replacement == "record" {
				require.NoError(t, err)
				require.Equal(t, record, got)
			} else {
				require.Error(t, err)
			}
		})
	}
}
