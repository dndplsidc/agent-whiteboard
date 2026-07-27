package pi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const maxRPCRecordBytes = 72 << 20

type jsonlReader struct {
	reader *bufio.Reader
	limit  int
}

func newJSONLReader(reader io.Reader, limit int) *jsonlReader {
	if limit <= 0 {
		limit = maxRPCRecordBytes
	}
	return &jsonlReader{reader: bufio.NewReaderSize(reader, 64<<10), limit: limit}
}

func (reader *jsonlReader) next() (json.RawMessage, error) {
	var record []byte
	for {
		fragment, err := reader.reader.ReadSlice('\n')
		if len(record)+len(fragment) > reader.limit+2 {
			wipe(record)
			return nil, errors.New("Pi RPC record exceeds byte limit")
		}
		record = append(record, fragment...)
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			wipe(record)
			if len(record) == 0 {
				return nil, io.EOF
			}
			return nil, errors.New("unterminated Pi RPC record")
		}
		wipe(record)
		return nil, errors.New("read Pi RPC record")
	}
	record = record[:len(record)-1]
	if len(record) > 0 && record[len(record)-1] == '\r' {
		record = record[:len(record)-1]
		if len(record) > 0 && record[len(record)-1] == '\r' {
			wipe(record)
			return nil, errors.New("invalid Pi RPC record delimiter")
		}
	}
	if len(record) == 0 || len(record) > reader.limit || !utf8.Valid(record) {
		wipe(record)
		return nil, errors.New("invalid Pi RPC record")
	}
	if err := validateJSONObject(record); err != nil {
		wipe(record)
		return nil, err
	}
	return json.RawMessage(record), nil
}

func validateJSONObject(record []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(record))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return errors.New("Pi RPC record is not an object")
	}
	if err := validateObject(decoder, 1); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("Pi RPC record has trailing JSON")
	}
	return nil
}

func validateObject(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("Pi RPC JSON nesting exceeds limit")
	}
	keys := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return errors.New("malformed Pi RPC object")
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("malformed Pi RPC object key")
		}
		if _, duplicate := keys[key]; duplicate {
			return fmt.Errorf("duplicate Pi RPC object key")
		}
		keys[key] = struct{}{}
		if err := validateJSONValue(decoder, depth); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return errors.New("malformed Pi RPC object")
	}
	return nil
}

func validateJSONValue(decoder *json.Decoder, depth int) error {
	token, err := decoder.Token()
	if err != nil {
		return errors.New("malformed Pi RPC value")
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		return validateObject(decoder, depth+1)
	case '[':
		if depth >= 64 {
			return errors.New("Pi RPC JSON nesting exceeds limit")
		}
		for decoder.More() {
			if err := validateJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("malformed Pi RPC array")
		}
		return nil
	default:
		return errors.New("malformed Pi RPC value")
	}
}
