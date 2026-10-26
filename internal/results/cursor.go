package results

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// maxPageTokenLen bounds the client-supplied token before decoding. Three
// IDs fit far below it; anything longer is not a token EncodeCursor made.
const maxPageTokenLen = 8192

// EncodeCursor returns the opaque page token for c.
func EncodeCursor(c Cursor) string {
	b, err := json.Marshal([3]string{c.JobID, c.SourceID, c.TargetID})
	if err != nil {
		// Marshaling a fixed-size string array cannot fail.
		panic(fmt.Sprintf("encode cursor: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor parses a page token. The empty token is the first page (nil).
func DecodeCursor(token string) (*Cursor, error) {
	if token == "" {
		return nil, nil
	}
	if len(token) > maxPageTokenLen {
		return nil, fmt.Errorf("%w: longer than %d bytes; pass the next_page_token from the previous response unchanged", ErrInvalidPageToken, maxPageTokenLen)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("%w: not base64url; pass the next_page_token from the previous response unchanged", ErrInvalidPageToken)
	}
	var parts [3]string
	var probe []json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil || len(probe) != 3 {
		return nil, fmt.Errorf("%w: malformed; pass the next_page_token from the previous response unchanged", ErrInvalidPageToken)
	}
	for i := range probe {
		if err := json.Unmarshal(probe[i], &parts[i]); err != nil {
			return nil, fmt.Errorf("%w: malformed; pass the next_page_token from the previous response unchanged", ErrInvalidPageToken)
		}
	}
	return &Cursor{JobID: parts[0], SourceID: parts[1], TargetID: parts[2]}, nil
}
