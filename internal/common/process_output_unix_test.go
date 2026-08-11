//go:build darwin || linux

package common

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOutputSpoolUnreadBoundIncludesPendingDrainRead(t *testing.T) {
	before := openFileDescriptors(t)
	readPipe, writePipe, err := os.Pipe()
	require.NoError(t, err)
	spool := newOutputSpool(readPipe)
	t.Cleanup(func() {
		spool.forceBoundedDrain()
		_ = writePipe.Close()
		spool.wait()
	})

	_, err = writePipe.Write([]byte{0})
	require.NoError(t, err)
	first := make([]byte, 1)
	_, err = io.ReadFull(spool, first)
	require.NoError(t, err)

	payload := make([]byte, maxUnreadOutputBytes+(64<<10))
	for index := range payload {
		payload[index] = byte((index + 1) % 251)
	}
	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := writePipe.Write(payload)
		writeDone <- writeErr
	}()

	require.Eventually(t, func() bool {
		spool.mu.Lock()
		defer spool.mu.Unlock()
		return spool.size == maxUnreadOutputBytes && spool.pending > 0
	}, time.Second, time.Millisecond)
	spool.mu.Lock()
	require.LessOrEqual(t, spool.size+spool.pending, maxOutputSpoolBufferBytes)
	require.LessOrEqual(t, cap(spool.buffer)+cap(spool.drainBuffer), maxOutputSpoolBufferBytes)
	spool.mu.Unlock()

	spool.forceBoundedDrain()
	require.NoError(t, <-writeDone)
	require.NoError(t, writePipe.Close())
	spool.wait()

	output, err := io.ReadAll(spool)
	require.ErrorIs(t, err, ErrProcessOutputOverflow)
	require.Equal(t, payload[:maxUnreadOutputBytes], output)
	select {
	case <-spool.done:
	default:
		t.Fatal("output drain goroutine remained active after source close")
	}
	requireNoNewFileDescriptors(t, before)
}

func TestOutputSpoolReportsKnownOverflowBeforeSourceEOF(t *testing.T) {
	readPipe, writePipe, err := os.Pipe()
	require.NoError(t, err)
	spool := newOutputSpool(readPipe)
	t.Cleanup(func() {
		spool.forceBoundedDrain()
		_ = writePipe.Close()
		spool.wait()
	})
	payload := make([]byte, maxUnreadOutputBytes+(64<<10))
	for index := range payload {
		payload[index] = byte(index % 251)
	}

	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := writePipe.Write(payload)
		writeDone <- writeErr
	}()
	require.NoError(t, <-writeDone)
	require.Eventually(t, func() bool {
		spool.mu.Lock()
		defer spool.mu.Unlock()
		return spool.overflow && spool.size == maxUnreadOutputBytes
	}, time.Second, time.Millisecond)

	type readResult struct {
		output []byte
		err    error
	}
	readDone := make(chan readResult, 1)
	go func() {
		output, readErr := io.ReadAll(spool)
		readDone <- readResult{output: output, err: readErr}
	}()

	select {
	case result := <-readDone:
		require.ErrorIs(t, result.err, ErrProcessOutputOverflow)
		require.Equal(t, payload[:maxUnreadOutputBytes], result.output)
	case <-time.After(time.Second):
		_ = writePipe.Close()
		<-readDone
		spool.wait()
		t.Fatal("Read hid a known overflow while the source remained open")
	}

	require.NoError(t, writePipe.Close())
	spool.wait()
}
