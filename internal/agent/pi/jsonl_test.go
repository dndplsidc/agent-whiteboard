package pi

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJSONLReaderHandlesPartialMultipleCRLFAndUnicodeSeparators(t *testing.T) {
	stream := &chunkReader{chunks: [][]byte{
		[]byte("{\"type\":\"one\",\"text\":\"a"),
		[]byte("\\u2028b\"}\n{\"type\":\"two\"}\r\n"),
	}}
	reader := newJSONLReader(stream, 1024)
	first, err := reader.next()
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"one","text":"a\u2028b"}`, string(first))
	second, err := reader.next()
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"two"}`, string(second))
	_, err = reader.next()
	require.ErrorIs(t, err, io.EOF)
}

func TestJSONLReaderRejectsMalformedRecords(t *testing.T) {
	cases := map[string]string{
		"empty":            "\n",
		"unterminated":     `{"type":"event"}`,
		"non object":       "[]\n",
		"duplicate root":   "{\"type\":\"a\",\"type\":\"b\"}\n",
		"duplicate nested": "{\"type\":\"a\",\"data\":{\"x\":1,\"x\":2}}\n",
		"trailing json":    "{\"type\":\"a\"} {}\n",
		"invalid utf8":     string([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}', '\n'}),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := newJSONLReader(strings.NewReader(input), 1024).next()
			require.Error(t, err)
		})
	}
}

func TestJSONLReaderBoundsRecordAndNesting(t *testing.T) {
	payload := []byte(`{"x":""}`)
	record, err := newJSONLReader(strings.NewReader(string(payload)+"\r\n"), len(payload)).next()
	require.NoError(t, err)
	require.Equal(t, payload, []byte(record))
	_, err = newJSONLReader(strings.NewReader(string(payload)+"\r\r\n"), len(payload)+1).next()
	require.Error(t, err)
	_, err = newJSONLReader(strings.NewReader("{\"type\":\"1234567890\"}\n"), 8).next()
	require.Error(t, err)
	nested := strings.Repeat("[", 65) + strings.Repeat("]", 65)
	_, err = newJSONLReader(strings.NewReader("{\"x\":"+nested+"}\n"), 1024).next()
	require.Error(t, err)
}

type chunkReader struct{ chunks [][]byte }

func (reader *chunkReader) Read(target []byte) (int, error) {
	if len(reader.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := reader.chunks[0]
	reader.chunks = reader.chunks[1:]
	n := copy(target, chunk)
	if n < len(chunk) {
		reader.chunks = append([][]byte{bytes.Clone(chunk[n:])}, reader.chunks...)
	}
	return n, nil
}
