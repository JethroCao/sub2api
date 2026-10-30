package service

import (
	"encoding/json"
	"strings"
)

// A streaming JSON projection keeps only the control/usage schema. In
// particular, strings in output (including image base64) are scanned without
// buffering. json.Decoder.Token cannot provide this bound: it allocates the
// complete string before returning a token.
const (
	homeJSONMaxDepth    = 64
	homeJSONScalarLimit = 4096
	homeJSONKeyLimit    = 512
)

type homeJSONSchema struct {
	fields      map[string]*homeJSONSchema
	scalarLimit int
}

var homeJSONObservationSchema = func() *homeJSONSchema {
	root := &homeJSONSchema{scalarLimit: 64}
	// Keep all inputs used by extractUpstreamErrorMessage and the stream error
	// classifier, including ChatGPT's scalar detail and structured detail.code.
	paths := []string{"type", "status", "status_code", "message", "code", "detail", "detail.code"}
	for _, prefix := range []string{"", "response."} {
		paths = append(paths, prefix+"status")
		for _, field := range []string{"type", "code", "message", "status", "status_code", "resets_at", "resets_in_seconds"} {
			paths = append(paths, prefix+"error."+field)
		}
		for _, field := range []string{"input_tokens", "output_tokens", "total_tokens", "input_tokens_details.cached_tokens", "output_tokens_details.reasoning_tokens"} {
			paths = append(paths, prefix+"usage."+field)
		}
	}
	for _, path := range paths {
		node := root
		for _, key := range strings.Split(path, ".") {
			if node.fields == nil {
				node.fields = map[string]*homeJSONSchema{}
			}
			next := node.fields[key]
			if next == nil {
				next = &homeJSONSchema{scalarLimit: 64}
				node.fields[key] = next
			}
			node = next
		}
		if strings.HasSuffix(path, "message") || path == "detail" {
			node.scalarLimit = homeJSONScalarLimit
		} else if strings.HasSuffix(path, "type") || strings.HasSuffix(path, "code") {
			node.scalarLimit = 128
		}
	}
	return root
}()

type homeJSONFrame struct {
	kind   byte
	state  byte // object: key/colon/value/comma/key-required; array: value/comma/value-required
	key    string
	schema *homeJSONSchema
	out    map[string]any
}
type homeJSONProjector struct {
	stack                      [homeJSONMaxDepth]homeJSONFrame
	depth                      int
	started, complete, invalid bool
	root                       any
	mode                       byte // string, number, literal
	keyToken                   bool
	escaped                    bool
	unicodeLeft                int
	numberState                byte
	literal                    string
	literalPos                 int
	token                      []byte
	tokenLimit                 int
	overflow                   bool
	target                     map[string]any
	targetKey                  string
	selected                   bool
}

func (p *homeJSONProjector) feed(raw []byte) {
	for _, ch := range raw {
		p.byte(ch)
	}
}
func (p *homeJSONProjector) byte(ch byte) {
	if p.invalid {
		return
	}
	switch p.mode {
	case 's':
		p.capture(ch)
		if p.unicodeLeft > 0 {
			if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
				p.invalid = true
				return
			}
			p.unicodeLeft--
			return
		}
		if p.escaped {
			p.escaped = false
			switch ch {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				p.unicodeLeft = 4
			default:
				p.invalid = true
			}
			return
		}
		if ch == '\\' {
			p.escaped = true
			return
		}
		if ch == '"' {
			p.mode = 0
			if p.keyToken {
				f := &p.stack[p.depth-1]
				f.key = ""
				if !p.overflow && p.tokenLimit > 0 {
					if json.Unmarshal(p.token, &f.key) != nil {
						p.invalid = true
					}
				}
				f.state = 1
			} else {
				p.finishScalar()
			}
			return
		}
		if ch < 0x20 {
			p.invalid = true
		}
		return
	case 'l':
		if p.literalPos >= len(p.literal) || ch != p.literal[p.literalPos] {
			p.invalid = true
			return
		}
		p.capture(ch)
		p.literalPos++
		if p.literalPos == len(p.literal) {
			p.mode = 0
			p.finishScalar()
		}
		return
	case 'n':
		if p.numberByte(ch) {
			p.capture(ch)
			return
		}
		if !p.numberComplete() {
			p.invalid = true
			return
		}
		p.mode = 0
		p.finishScalar()
		// A number ends at punctuation/whitespace, which still belongs to grammar.
	}
	if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' {
		return
	}
	if p.complete {
		p.invalid = true
		return
	}
	if p.depth > 0 {
		f := &p.stack[p.depth-1]
		if f.kind == '{' {
			switch f.state {
			case 0, 4:
				if ch == '}' && f.state == 0 {
					p.close('}')
					return
				}
				if ch != '"' {
					p.invalid = true
					return
				}
				p.startString(true, f.schema, nil, "")
				return
			case 1:
				if ch != ':' {
					p.invalid = true
				} else {
					f.state = 2
				}
				return
			case 3:
				if ch == '}' {
					p.close(ch)
				} else if ch == ',' {
					f.state = 4
				} else {
					p.invalid = true
				}
				return
			}
		} else {
			if f.state == 1 {
				if ch == ']' {
					p.close(ch)
				} else if ch == ',' {
					f.state = 2
				} else {
					p.invalid = true
				}
				return
			}
			if f.state == 0 && ch == ']' {
				p.close(ch)
				return
			}
		}
	}
	schema, target, key := p.takeValue()
	if p.invalid {
		return
	}
	switch ch {
	case '{', '[':
		if p.depth >= homeJSONMaxDepth {
			p.invalid = true
			return
		}
		f := homeJSONFrame{kind: ch, schema: schema}
		if ch == '{' && schema != nil {
			f.out = map[string]any{}
			p.put(target, key, f.out)
		} else if ch == '[' {
			if schema != nil {
				p.put(target, key, []any{})
			}
			f.schema = nil
		}
		p.stack[p.depth] = f
		p.depth++
	case '"':
		p.startString(false, schema, target, key)
	case 't', 'f', 'n':
		p.startScalar('l', schema != nil, target, key)
		switch ch {
		case 't':
			p.literal = "true"
		case 'f':
			p.literal = "false"
		case 'n':
			p.literal = "null"
		}
		p.literalPos = 1
		p.capture(ch)
	default:
		if ch != '-' && (ch < '0' || ch > '9') {
			p.invalid = true
			return
		}
		p.startScalar('n', schema != nil, target, key)
		switch {
		case ch == '-':
			p.numberState = '-'
		case ch == '0':
			p.numberState = '0'
		default:
			p.numberState = 'i'
		}
		p.capture(ch)
	}
}
func (p *homeJSONProjector) takeValue() (*homeJSONSchema, map[string]any, string) {
	if p.depth == 0 {
		if p.started {
			p.invalid = true
			return nil, nil, ""
		}
		p.started = true
		return homeJSONObservationSchema, nil, ""
	}
	f := &p.stack[p.depth-1]
	if f.kind == '[' {
		if f.state != 0 && f.state != 2 {
			p.invalid = true
		}
		f.state = 1
		return nil, nil, ""
	}
	if f.state != 2 {
		p.invalid = true
		return nil, nil, ""
	}
	f.state = 3
	var schema *homeJSONSchema
	if f.schema != nil {
		schema = f.schema.fields[f.key]
	}
	return schema, f.out, f.key
}
func (p *homeJSONProjector) close(ch byte) {
	if p.depth == 0 {
		p.invalid = true
		return
	}
	f := &p.stack[p.depth-1]
	if (ch == '}' && f.kind != '{') || (ch == ']' && f.kind != '[') {
		p.invalid = true
		return
	}
	*f = homeJSONFrame{}
	p.depth--
	if p.depth == 0 {
		p.complete = true
	}
}
func (p *homeJSONProjector) startScalar(mode byte, selected bool, target map[string]any, key string) {
	p.mode = mode
	p.keyToken = false
	p.selected = selected
	p.target = target
	p.targetKey = key
	p.token = p.token[:0]
	p.overflow = false
	p.tokenLimit = 0
	if selected {
		p.tokenLimit = homeJSONScalarLimit
	}
	if mode != 's' && p.tokenLimit > 64 {
		p.tokenLimit = 64
	}
}
func (p *homeJSONProjector) startString(key bool, schema *homeJSONSchema, target map[string]any, name string) {
	selected := schema != nil
	p.startScalar('s', selected, target, name)
	if selected {
		p.tokenLimit = schema.scalarLimit
	}
	p.keyToken = key
	p.escaped = false
	p.unicodeLeft = 0
	if key && selected {
		p.tokenLimit = homeJSONKeyLimit
	}
	p.capture('"')
}
func (p *homeJSONProjector) capture(ch byte) {
	if p.tokenLimit == 0 || p.overflow {
		return
	}
	if len(p.token) >= p.tokenLimit {
		p.token = p.token[:0]
		p.overflow = true
		return
	}
	p.token = append(p.token, ch)
}
func (p *homeJSONProjector) finishScalar() {
	if p.selected {
		if !p.overflow {
			p.put(p.target, p.targetKey, json.RawMessage(append([]byte(nil), p.token...)))
		} else {
			// Keep field presence: an oversized error scalar is still an error.
			p.put(p.target, p.targetKey, json.RawMessage(`""`))
		}
	}
	if p.depth == 0 {
		p.complete = true
	}
}
func (p *homeJSONProjector) put(target map[string]any, key string, value any) {
	if target != nil {
		target[key] = value
	} else if p.depth == 0 {
		p.root = value
	}
}

// numberByte consumes a JSON-number character, independently of token size.
func (p *homeJSONProjector) numberByte(ch byte) bool {
	digit := ch >= '0' && ch <= '9'
	switch p.numberState {
	case '-':
		if ch == '0' {
			p.numberState = '0'
			return true
		}
		if ch >= '1' && ch <= '9' {
			p.numberState = 'i'
			return true
		}
	case '0', 'i':
		if digit && p.numberState == 'i' {
			return true
		}
		if ch == '.' {
			p.numberState = '.'
			return true
		}
		if ch == 'e' || ch == 'E' {
			p.numberState = 'e'
			return true
		}
	case '.':
		if digit {
			p.numberState = 'f'
			return true
		}
	case 'f':
		if digit {
			return true
		}
		if ch == 'e' || ch == 'E' {
			p.numberState = 'e'
			return true
		}
	case 'e':
		if ch == '+' || ch == '-' {
			p.numberState = '+'
			return true
		}
		if digit {
			p.numberState = 'x'
			return true
		}
	case '+':
		if digit {
			p.numberState = 'x'
			return true
		}
	case 'x':
		if digit {
			return true
		}
	}
	return false
}
func (p *homeJSONProjector) numberComplete() bool {
	return p.numberState == '0' || p.numberState == 'i' || p.numberState == 'f' || p.numberState == 'x'
}
func (p *homeJSONProjector) finish() []byte {
	if p.mode == 'n' && p.numberComplete() {
		p.mode = 0
		p.finishScalar()
	}
	if p.invalid || !p.complete || p.depth != 0 || p.mode != 0 {
		return nil
	}
	raw, err := json.Marshal(p.root)
	if err != nil {
		return nil
	}
	return raw
}
