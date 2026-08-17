package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
)

// GGUF metadata reader. Reads only the key-value header at the front of a
// .gguf file — never the tensor data — so it is cheap enough to run on every
// configured model at startup.
//
// Format (little-endian): magic "GGUF", uint32 version, uint64 tensor_count,
// uint64 kv_count, then kv_count entries of {string key, uint32 type, value}.
// Strings are a uint64 length followed by raw UTF-8 bytes. Arrays are a
// uint32 element type, a uint64 count, then the elements.
//
// We need this because a managed model's resident memory is weights (the file,
// which llama.cpp mmaps) plus the KV cache, and the KV cache is a function of
// architecture geometry that only the header knows. See estimateLlamaMemory.

// GGUF value type tags.
const (
	ggufUint8 uint32 = iota
	ggufInt8
	ggufUint16
	ggufInt16
	ggufUint32
	ggufInt32
	ggufFloat32
	ggufBool
	ggufString
	ggufArray
	ggufUint64
	ggufInt64
	ggufFloat64
)

// Sanity bounds. A malformed or truncated file must fail rather than make us
// allocate gigabytes from an attacker- (or corruption-) controlled length.
const (
	ggufMaxKVCount     = 1 << 20
	ggufMaxStringLen   = 1 << 24 // 16 MiB
	ggufMaxArrayLen    = 1 << 24
	ggufMaxHeaderBytes = 1 << 28 // 256 MiB of metadata is already absurd
)

// ggufMetadata holds the parsed header. Integer values are normalized to
// int64, floats to float64, and arrays to []int64 / []float64 / []bool /
// []string, so callers get one type per shape rather than thirteen.
type ggufMetadata map[string]any

type ggufReader struct {
	r    *bufio.Reader
	read int64 // bytes consumed, checked against ggufMaxHeaderBytes
}

func (g *ggufReader) bytes(n int64) ([]byte, error) {
	if n < 0 || g.read+n > ggufMaxHeaderBytes {
		return nil, fmt.Errorf("gguf: header exceeds %d bytes", ggufMaxHeaderBytes)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(g.r, buf); err != nil {
		return nil, fmt.Errorf("gguf: read %d bytes: %w", n, err)
	}
	g.read += n
	return buf, nil
}

func (g *ggufReader) u32() (uint32, error) {
	b, err := g.bytes(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (g *ggufReader) u64() (uint64, error) {
	b, err := g.bytes(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

func (g *ggufReader) str() (string, error) {
	n, err := g.u64()
	if err != nil {
		return "", err
	}
	if n > ggufMaxStringLen {
		return "", fmt.Errorf("gguf: string length %d exceeds limit", n)
	}
	b, err := g.bytes(int64(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// scalar reads one non-string, non-array value and normalizes its type.
func (g *ggufReader) scalar(t uint32) (any, error) {
	width := map[uint32]int64{
		ggufUint8: 1, ggufInt8: 1, ggufBool: 1,
		ggufUint16: 2, ggufInt16: 2,
		ggufUint32: 4, ggufInt32: 4, ggufFloat32: 4,
		ggufUint64: 8, ggufInt64: 8, ggufFloat64: 8,
	}[t]
	if width == 0 {
		return nil, fmt.Errorf("gguf: unknown value type %d", t)
	}
	b, err := g.bytes(width)
	if err != nil {
		return nil, err
	}
	switch t {
	case ggufUint8:
		return int64(b[0]), nil
	case ggufInt8:
		return int64(int8(b[0])), nil
	case ggufBool:
		return b[0] != 0, nil
	case ggufUint16:
		return int64(binary.LittleEndian.Uint16(b)), nil
	case ggufInt16:
		return int64(int16(binary.LittleEndian.Uint16(b))), nil
	case ggufUint32:
		return int64(binary.LittleEndian.Uint32(b)), nil
	case ggufInt32:
		return int64(int32(binary.LittleEndian.Uint32(b))), nil
	case ggufFloat32:
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(b))), nil
	case ggufUint64:
		return int64(binary.LittleEndian.Uint64(b)), nil
	case ggufInt64:
		return int64(binary.LittleEndian.Uint64(b)), nil
	case ggufFloat64:
		return math.Float64frombits(binary.LittleEndian.Uint64(b)), nil
	}
	return nil, fmt.Errorf("gguf: unhandled value type %d", t)
}

// value reads a full value of the given type, including strings and arrays.
func (g *ggufReader) value(t uint32) (any, error) {
	switch t {
	case ggufString:
		return g.str()
	case ggufArray:
		elemType, err := g.u32()
		if err != nil {
			return nil, err
		}
		n, err := g.u64()
		if err != nil {
			return nil, err
		}
		if n > ggufMaxArrayLen {
			return nil, fmt.Errorf("gguf: array length %d exceeds limit", n)
		}
		return g.array(elemType, int(n))
	default:
		return g.scalar(t)
	}
}

func (g *ggufReader) array(elemType uint32, n int) (any, error) {
	switch elemType {
	case ggufString:
		out := make([]string, n)
		for i := range out {
			s, err := g.str()
			if err != nil {
				return nil, err
			}
			out[i] = s
		}
		return out, nil
	case ggufArray:
		// Nested arrays are legal in the spec but unused by llama.cpp models,
		// and supporting them would complicate the normalized return types.
		return nil, fmt.Errorf("gguf: nested arrays not supported")
	case ggufBool:
		out := make([]bool, n)
		for i := range out {
			v, err := g.scalar(elemType)
			if err != nil {
				return nil, err
			}
			out[i] = v.(bool)
		}
		return out, nil
	case ggufFloat32, ggufFloat64:
		out := make([]float64, n)
		for i := range out {
			v, err := g.scalar(elemType)
			if err != nil {
				return nil, err
			}
			out[i] = v.(float64)
		}
		return out, nil
	default:
		out := make([]int64, n)
		for i := range out {
			v, err := g.scalar(elemType)
			if err != nil {
				return nil, err
			}
			iv, ok := v.(int64)
			if !ok {
				return nil, fmt.Errorf("gguf: array element type %d is not integral", elemType)
			}
			out[i] = iv
		}
		return out, nil
	}
}

// ReadGGUFMetadata parses the key-value header of a .gguf file.
func ReadGGUFMetadata(path string) (ggufMetadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("gguf: open %s: %w", path, err)
	}
	defer f.Close()

	g := &ggufReader{r: bufio.NewReaderSize(f, 1<<16)}

	magic, err := g.bytes(4)
	if err != nil {
		return nil, err
	}
	if string(magic) != "GGUF" {
		return nil, fmt.Errorf("gguf: %s is not a GGUF file (magic %q)", path, magic)
	}
	if _, err := g.u32(); err != nil { // version
		return nil, err
	}
	if _, err := g.u64(); err != nil { // tensor_count
		return nil, err
	}
	kvCount, err := g.u64()
	if err != nil {
		return nil, err
	}
	if kvCount > ggufMaxKVCount {
		return nil, fmt.Errorf("gguf: kv count %d exceeds limit", kvCount)
	}

	md := make(ggufMetadata, kvCount)
	for i := uint64(0); i < kvCount; i++ {
		key, err := g.str()
		if err != nil {
			return nil, fmt.Errorf("gguf: key %d: %w", i, err)
		}
		t, err := g.u32()
		if err != nil {
			return nil, fmt.Errorf("gguf: %s: %w", key, err)
		}
		v, err := g.value(t)
		if err != nil {
			return nil, fmt.Errorf("gguf: %s: %w", key, err)
		}
		md[key] = v
	}
	return md, nil
}

// int returns an integer metadata value, or ok=false when absent or non-integral.
func (md ggufMetadata) int(key string) (int64, bool) {
	v, ok := md[key].(int64)
	return v, ok
}

// intSlice returns an integer-array metadata value. A scalar is widened to a
// one-element slice so callers can treat "one value for all layers" and
// "one value per layer" uniformly.
func (md ggufMetadata) intSlice(key string) ([]int64, bool) {
	switch v := md[key].(type) {
	case []int64:
		return v, true
	case int64:
		return []int64{v}, true
	}
	return nil, false
}

func (md ggufMetadata) boolSlice(key string) ([]bool, bool) {
	v, ok := md[key].([]bool)
	return v, ok
}

func (md ggufMetadata) str(key string) string {
	v, _ := md[key].(string)
	return v
}

// arch returns the general.architecture value, which prefixes every
// architecture-specific metadata key (e.g. "qwen35moe.block_count").
func (md ggufMetadata) arch() string {
	return md.str("general.architecture")
}

// archInt reads an architecture-prefixed integer, e.g. archInt("block_count")
// resolves to "<arch>.block_count".
func (md ggufMetadata) archInt(suffix string) (int64, bool) {
	return md.int(md.arch() + "." + suffix)
}

func (md ggufMetadata) archIntSlice(suffix string) ([]int64, bool) {
	return md.intSlice(md.arch() + "." + suffix)
}

func (md ggufMetadata) archBoolSlice(suffix string) ([]bool, bool) {
	return md.boolSlice(md.arch() + "." + suffix)
}

// kvCacheBytesPerElem maps a llama-server --cache-type-{k,v} value to bytes
// per element. Quantized types carry a per-block scale, so the cost is the
// block payload plus its scale divided by the block size — q8_0 is 32 quants
// (32 B) plus one f16 scale (2 B) over 32 elements.
func kvCacheBytesPerElem(cacheType string) float64 {
	switch strings.ToLower(strings.TrimSpace(cacheType)) {
	case "", "f16", "bf16":
		return 2.0
	case "f32":
		return 4.0
	case "q8_0":
		return 34.0 / 32.0
	case "q5_1":
		return 24.0 / 32.0
	case "q5_0":
		return 22.0 / 32.0
	case "q4_1":
		return 20.0 / 32.0
	case "q4_0", "iq4_nl":
		return 18.0 / 32.0
	default:
		// Unknown type: assume f16 rather than zero, so an unrecognized
		// setting over-reserves instead of silently under-counting.
		return 2.0
	}
}

// ggufKVCacheBytes computes the KV cache size for a context of ctx tokens.
//
// The per-layer sum matters for two real architectures in the wild:
//
//   - Per-layer GQA: attention.head_count_kv can be an array, so different
//     layers hold different numbers of KV heads (Gemma alternates 8 and 1).
//   - Sliding-window attention: layers flagged in attention.sliding_window_pattern
//     only retain a window of tokens, not the full context, and use the
//     separate key_length_swa / value_length_swa head dimensions. Ignoring
//     this over-estimates Gemma 4 12B at 32k context by roughly 25x.
func ggufKVCacheBytes(md ggufMetadata, ctx int64, cacheTypeK, cacheTypeV string) (int64, error) {
	nLayer, ok := md.archInt("block_count")
	if !ok || nLayer <= 0 {
		return 0, fmt.Errorf("gguf: missing %s.block_count", md.arch())
	}

	kvHeads, ok := md.archIntSlice("attention.head_count_kv")
	if !ok || len(kvHeads) == 0 {
		return 0, fmt.Errorf("gguf: missing %s.attention.head_count_kv", md.arch())
	}

	keyLen, hasKey := md.archInt("attention.key_length")
	valLen, hasVal := md.archInt("attention.value_length")
	if !hasKey || !hasVal {
		// Fall back to the classic assumption: head_dim = n_embd / n_head.
		embed, okE := md.archInt("embedding_length")
		heads, okH := md.archInt("attention.head_count")
		if !okE || !okH || heads == 0 {
			return 0, fmt.Errorf("gguf: cannot derive head dimension for %s", md.arch())
		}
		keyLen, valLen = embed/heads, embed/heads
	}

	// Sliding-window layers cap their KV at the window instead of the context.
	window, _ := md.archInt("attention.sliding_window")
	swaPattern, _ := md.archBoolSlice("attention.sliding_window_pattern")
	keyLenSWA, okKS := md.archInt("attention.key_length_swa")
	valLenSWA, okVS := md.archInt("attention.value_length_swa")
	if !okKS {
		keyLenSWA = keyLen
	}
	if !okVS {
		valLenSWA = valLen
	}

	bk := kvCacheBytesPerElem(cacheTypeK)
	bv := kvCacheBytesPerElem(cacheTypeV)

	var total float64
	for i := int64(0); i < nLayer; i++ {
		// A scalar head_count_kv applies to every layer; an array is per-layer.
		heads := kvHeads[0]
		if len(kvHeads) > 1 {
			if int(i) >= len(kvHeads) {
				return 0, fmt.Errorf("gguf: head_count_kv has %d entries for %d layers", len(kvHeads), nLayer)
			}
			heads = kvHeads[i]
		}

		tokens, kl, vl := ctx, keyLen, valLen
		if window > 0 && int(i) < len(swaPattern) && swaPattern[i] {
			tokens = min(window, ctx)
			kl, vl = keyLenSWA, valLenSWA
		}
		total += float64(tokens) * float64(heads) * (float64(kl)*bk + float64(vl)*bv)
	}
	return int64(total), nil
}
