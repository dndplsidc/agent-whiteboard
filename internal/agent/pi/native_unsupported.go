//go:build !unix

package pi

import "errors"

const maxNativeRecordBytes = 64 << 10

func validateSessionFile(_, _, _ string) error {
	return errors.New("Pi native sessions are unsupported on this platform")
}
