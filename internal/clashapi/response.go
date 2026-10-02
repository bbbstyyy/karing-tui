package clashapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const maxAPIResponseBytes = 8 << 20

var errAPIResponseTooLarge = errors.New("clash API response exceeds 8 MiB")

func decodeAPIResponse(body io.Reader, out any) error {
	limited := &io.LimitedReader{R: body, N: maxAPIResponseBytes + 1}
	decoder := json.NewDecoder(limited)
	err := decoder.Decode(out)
	if limited.N == 0 {
		return errAPIResponseTooLarge
	}
	if err != nil {
		return err
	}
	// Consume trailing whitespace and require exactly one JSON document. This
	// also lets successful responses reuse their HTTP connection.
	_, err = decoder.Token()
	if limited.N == 0 {
		return errAPIResponseTooLarge
	}
	if err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("unexpected data after clash API response")
	}
	return nil
}

// connectionCount discards metadata instead of constructing an entire graph of
// maps, slices and strings for a dashboard that only needs a connection count.
type connectionCount int

func (c *connectionCount) UnmarshalJSON(data []byte) error {
	*c = 0
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('[') {
		return fmt.Errorf("connections must be an array")
	}
	for decoder.More() {
		var discard struct{}
		if err := decoder.Decode(&discard); err != nil {
			return err
		}
		*c++
	}
	_, err = decoder.Token()
	return err
}
