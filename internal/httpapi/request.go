package httpapi

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
)

// maxTakeFieldBytes bounds the raw JSON string content of the take metadata
// fields. Every legitimate value is far smaller: identifiers fit 64 bytes and
// names or notes a few thousand runes.
const maxTakeFieldBytes = 1 << 20

// maxTakeKeyBytes bounds object keys; every known key is shorter than this.
const maxTakeKeyBytes = 512

// decodeTakeRequest parses the create-take JSON object as a stream. The two
// base64 media fields are decoded straight into bytes while they are read, so
// the request never holds the encoded strings, the decoded payloads, and a
// decoder buffer at the same time. The accepted document shape, the size
// budgets, and the stable error codes match the previous buffered decoder.
func decodeTakeRequest(w http.ResponseWriter, request *http.Request) (createTakeRequest, error) {
	var payload createTakeRequest
	request.Body = http.MaxBytesReader(w, request.Body, domain.MaxRequestBytes)
	parser := &takeParser{reader: bufio.NewReader(request.Body)}
	if err := parser.parseObject(&payload); err != nil {
		return createTakeRequest{}, err
	}
	return payload, nil
}

type takeParser struct {
	reader *bufio.Reader
}

func (p *takeParser) parseObject(payload *createTakeRequest) error {
	if err := p.skipSpace(); err != nil {
		if errors.Is(err, io.EOF) {
			return domain.NewError("invalid_request", 400, "request body is required")
		}
		return err
	}
	char, err := p.readByte()
	if err != nil {
		return unexpectedEOF(err)
	}
	if char != '{' {
		return invalidJSON("request body is not valid JSON: expected an object")
	}
	for {
		if err := p.skipSpaceStrict(); err != nil {
			return err
		}
		char, err := p.readByte()
		if err != nil {
			return unexpectedEOF(err)
		}
		if char == '}' {
			break
		}
		if char != '"' {
			return invalidJSON("request body is not valid JSON: expected an object key")
		}
		key, err := p.readStringContent(maxTakeKeyBytes)
		if err != nil {
			return err
		}
		if err := p.skipSpaceStrict(); err != nil {
			return err
		}
		if char, err := p.readByte(); err != nil {
			return unexpectedEOF(err)
		} else if char != ':' {
			return invalidJSON("request body is not valid JSON: expected ':' after object key")
		}
		if err := p.skipSpaceStrict(); err != nil {
			return err
		}
		if err := p.readField(payload, key); err != nil {
			return err
		}
		if err := p.skipSpaceStrict(); err != nil {
			return err
		}
		char, err = p.readByte()
		if err != nil {
			return unexpectedEOF(err)
		}
		switch char {
		case ',':
			continue
		case '}':
			return p.finish()
		default:
			return invalidJSON("request body is not valid JSON: expected ',' or '}' in object")
		}
	}
	return p.finish()
}

// readField consumes one value for a known key, or rejects the document.
func (p *takeParser) readField(payload *createTakeRequest, key string) error {
	isNull, err := p.consumeNull()
	if err != nil {
		return err
	}
	if isNull {
		return nil
	}
	switch key {
	case "take_id":
		value, err := p.readString(maxTakeFieldBytes)
		payload.TakeID = value
		return err
	case "name":
		value, err := p.readString(maxTakeFieldBytes)
		payload.Name = value
		return err
	case "note":
		value, err := p.readString(maxTakeFieldBytes)
		payload.Note = value
		return err
	case "audio_base64":
		data, err := p.readBase64("audio_base64", domain.MaxAudioBytes)
		payload.Audio = data
		return err
	case "video_base64":
		data, err := p.readBase64("video_base64", domain.MaxVideoBytes)
		payload.Video = data
		return err
	default:
		return invalidJSON("request body is not valid JSON: json: unknown field %q", key)
	}
}

// finish verifies the object is the only value in the body.
func (p *takeParser) finish() error {
	if err := p.skipSpace(); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	if _, err := p.readByte(); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return invalidJSON("request body must contain one JSON object")
}

// consumeNull reports whether the upcoming value is a JSON null and consumes
// it when it is. A null leaves the target field at its zero value, matching
// encoding/json semantics for string fields.
func (p *takeParser) consumeNull() (bool, error) {
	peek, err := p.reader.Peek(1)
	if err != nil {
		return false, unexpectedEOF(err)
	}
	if peek[0] != 'n' {
		return false, nil
	}
	for _, want := range []byte("null") {
		char, err := p.readByte()
		if err != nil {
			return false, unexpectedEOF(err)
		}
		if char != want {
			return false, invalidJSON("request body is not valid JSON: unexpected value")
		}
	}
	return true, nil
}

// readString consumes a JSON string starting at the opening quote and returns
// its decoded content, bounded by limit bytes.
func (p *takeParser) readString(limit int) (string, error) {
	char, err := p.readByte()
	if err != nil {
		return "", unexpectedEOF(err)
	}
	if char != '"' {
		return "", invalidJSON("request body is not valid JSON: expected a string value")
	}
	return p.readStringContent(limit)
}

// readStringContent decodes a JSON string whose opening quote has already
// been consumed.
func (p *takeParser) readStringContent(limit int) (string, error) {
	var content []byte
	reader := &jsonStringReader{parser: p}
	buffer := make([]byte, 4096)
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			content = append(content, buffer[:n]...)
			if len(content) > limit {
				return "", domain.NewError("invalid_request", 400, "request field is too long")
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return string(content), nil
			}
			return "", readErr
		}
	}
}

// readBase64 consumes a JSON string and decodes its base64 content while
// reading, enforcing the same strict alphabet, emptiness, and size rules as
// the previous buffered decoder.
func (p *takeParser) readBase64(field string, maximum int) ([]byte, error) {
	char, err := p.readByte()
	if err != nil {
		return nil, unexpectedEOF(err)
	}
	if char != '"' {
		return nil, invalidJSON("request body is not valid JSON: expected a string value")
	}
	reader := &jsonStringReader{parser: p}
	decoder := base64.NewDecoder(base64.StdEncoding.Strict(), reader)
	data, err := io.ReadAll(io.LimitReader(decoder, int64(maximum)+1))
	if reader.parseErr != nil {
		return nil, reader.parseErr
	}
	if err != nil {
		return nil, domain.NewError("invalid_base64", 400, "%s is not valid base64", field)
	}
	if len(data) == 0 {
		return nil, domain.NewError("invalid_request", 400, "%s is empty", field)
	}
	if len(data) > maximum {
		return nil, domain.NewError(
			"payload_too_large",
			413,
			"%s exceeds the %d byte limit",
			field,
			maximum,
		)
	}
	return data, nil
}

// jsonStringReader yields the decoded content of one JSON string until the
// closing quote. Escape sequences are unescaped with the same rules
// encoding/json applies.
type jsonStringReader struct {
	parser   *takeParser
	pending  []byte
	done     bool
	parseErr error
}

func (r *jsonStringReader) Read(buffer []byte) (int, error) {
	if r.parseErr != nil {
		return 0, r.parseErr
	}
	if r.done {
		return 0, io.EOF
	}
	total := 0
	for total < len(buffer) {
		if len(r.pending) > 0 {
			copied := copy(buffer[total:], r.pending)
			total += copied
			r.pending = r.pending[copied:]
			continue
		}
		char, err := r.parser.readByte()
		if err != nil {
			r.parseErr = unexpectedEOF(err)
			return r.finishRead(total)
		}
		switch {
		case char == '"':
			r.done = true
			return r.finishRead(total)
		case char == '\\':
			decoded, err := r.readEscape()
			if err != nil {
				r.parseErr = err
				return r.finishRead(total)
			}
			r.pending = decoded
		case char < 0x20:
			r.parseErr = invalidJSON("request body is not valid JSON: invalid control character in string")
			return r.finishRead(total)
		default:
			buffer[total] = char
			total++
		}
	}
	return total, nil
}

// finishRead returns the bytes gathered so far, deferring any terminal error
// to the next Read call once the buffer is drained.
func (r *jsonStringReader) finishRead(total int) (int, error) {
	if total > 0 {
		return total, nil
	}
	if r.parseErr != nil {
		return 0, r.parseErr
	}
	return 0, io.EOF
}

// readEscape decodes one escape sequence after a backslash.
func (r *jsonStringReader) readEscape() ([]byte, error) {
	char, err := r.parser.readByte()
	if err != nil {
		return nil, unexpectedEOF(err)
	}
	switch char {
	case '"', '\\', '/':
		return []byte{char}, nil
	case 'b':
		return []byte{'\b'}, nil
	case 'f':
		return []byte{'\f'}, nil
	case 'n':
		return []byte{'\n'}, nil
	case 'r':
		return []byte{'\r'}, nil
	case 't':
		return []byte{'\t'}, nil
	case 'u':
		return r.readUnicodeEscape()
	default:
		return nil, invalidJSON("request body is not valid JSON: invalid escape sequence in string")
	}
}

// readUnicodeEscape decodes \uXXXX with surrogate pair handling, matching
// encoding/json: an unpaired surrogate decodes to U+FFFD.
func (r *jsonStringReader) readUnicodeEscape() ([]byte, error) {
	first, err := r.readHex4()
	if err != nil {
		return nil, err
	}
	runeValue := rune(first)
	if utf16.IsSurrogate(runeValue) {
		paired := false
		if lookahead, err := r.parser.reader.Peek(6); err == nil &&
			lookahead[0] == '\\' && lookahead[1] == 'u' {
			if second, ok := parseHex4(lookahead[2:6]); ok {
				if combined := utf16.DecodeRune(rune(first), rune(second)); combined != utf8.RuneError {
					if _, err := r.parser.reader.Discard(6); err != nil {
						return nil, unexpectedEOF(err)
					}
					runeValue = combined
					paired = true
				}
			}
		}
		if !paired {
			runeValue = utf8.RuneError
		}
	}
	encoded := make([]byte, utf8.RuneLen(runeValue))
	utf8.EncodeRune(encoded, runeValue)
	return encoded, nil
}

func (r *jsonStringReader) readHex4() (int, error) {
	raw := make([]byte, 4)
	for index := range raw {
		char, err := r.parser.readByte()
		if err != nil {
			return 0, unexpectedEOF(err)
		}
		raw[index] = char
	}
	value, ok := parseHex4(raw)
	if !ok {
		return 0, invalidJSON("request body is not valid JSON: invalid unicode escape in string")
	}
	return value, nil
}

func parseHex4(raw []byte) (int, bool) {
	if len(raw) != 4 {
		return 0, false
	}
	value := 0
	for _, char := range raw {
		var digit int
		switch {
		case char >= '0' && char <= '9':
			digit = int(char - '0')
		case char >= 'a' && char <= 'f':
			digit = int(char-'a') + 10
		case char >= 'A' && char <= 'F':
			digit = int(char-'A') + 10
		default:
			return 0, false
		}
		value = value*16 + digit
	}
	return value, true
}

// skipSpace consumes JSON whitespace. A bare io.EOF is returned unchanged so
// callers can decide how to map the end of the body.
func (p *takeParser) skipSpace() error {
	for {
		char, err := p.reader.ReadByte()
		if err != nil {
			return err
		}
		switch char {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return p.reader.UnreadByte()
		}
	}
}

// skipSpaceStrict consumes JSON whitespace and maps an unexpected end of the
// body to a stable invalid_json error.
func (p *takeParser) skipSpaceStrict() error {
	return unexpectedEOF(p.skipSpace())
}

func (p *takeParser) readByte() (byte, error) {
	char, err := p.reader.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return 0, io.EOF
		}
		return 0, invalidJSON("request body is not valid JSON: %s", err)
	}
	return char, nil
}

func unexpectedEOF(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) {
		return invalidJSON("request body is not valid JSON: unexpected EOF")
	}
	return err
}

func invalidJSON(format string, args ...any) error {
	return domain.NewError("invalid_json", 400, fmt.Sprintf(format, args...))
}
