package main

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// GGUF writer (test-only)
// ---------------------------------------------------------------------------

type ggufWriter struct {
	buf     bytes.Buffer
	entries bytes.Buffer
	count   uint64
}

func (w *ggufWriter) key(k string) {
	binary.Write(&w.entries, binary.LittleEndian, uint64(len(k)))
	w.entries.WriteString(k)
}

func (w *ggufWriter) u32(k string, v uint32) {
	w.key(k)
	binary.Write(&w.entries, binary.LittleEndian, ggufUint32)
	binary.Write(&w.entries, binary.LittleEndian, v)
	w.count++
}

func (w *ggufWriter) str(k, v string) {
	w.key(k)
	binary.Write(&w.entries, binary.LittleEndian, ggufString)
	binary.Write(&w.entries, binary.LittleEndian, uint64(len(v)))
	w.entries.WriteString(v)
	w.count++
}

func (w *ggufWriter) f32(k string, v float32) {
	w.key(k)
	binary.Write(&w.entries, binary.LittleEndian, ggufFloat32)
	binary.Write(&w.entries, binary.LittleEndian, math.Float32bits(v))
	w.count++
}

func (w *ggufWriter) u32Array(k string, vals []uint32) {
	w.key(k)
	binary.Write(&w.entries, binary.LittleEndian, ggufArray)
	binary.Write(&w.entries, binary.LittleEndian, ggufUint32)
	binary.Write(&w.entries, binary.LittleEndian, uint64(len(vals)))
	for _, v := range vals {
		binary.Write(&w.entries, binary.LittleEndian, v)
	}
	w.count++
}

func (w *ggufWriter) boolArray(k string, vals []bool) {
	w.key(k)
	binary.Write(&w.entries, binary.LittleEndian, ggufArray)
	binary.Write(&w.entries, binary.LittleEndian, ggufBool)
	binary.Write(&w.entries, binary.LittleEndian, uint64(len(vals)))
	for _, v := range vals {
		var b byte
		if v {
			b = 1
		}
		w.entries.WriteByte(b)
	}
	w.count++
}

func (w *ggufWriter) bytes() []byte {
	w.buf.Reset()
	w.buf.WriteString("GGUF")
	binary.Write(&w.buf, binary.LittleEndian, uint32(3)) // version
	binary.Write(&w.buf, binary.LittleEndian, uint64(0)) // tensor_count
	binary.Write(&w.buf, binary.LittleEndian, w.count)
	w.buf.Write(w.entries.Bytes())
	return w.buf.Bytes()
}

func writeTestGGUF(t *testing.T, w *ggufWriter) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(path, w.bytes(), 0o644); err != nil {
		t.Fatalf("write test gguf: %v", err)
	}
	return path
}

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

func TestReadGGUFMetadata_RoundTrip(t *testing.T) {
	w := &ggufWriter{}
	w.str("general.architecture", "testarch")
	w.u32("testarch.block_count", 40)
	w.u32("testarch.attention.head_count_kv", 8)
	w.f32("testarch.rope.freq_base", 1e7)
	w.u32Array("testarch.per_layer", []uint32{1, 2, 3})
	w.boolArray("testarch.flags", []bool{true, false, true})

	md, err := ReadGGUFMetadata(writeTestGGUF(t, w))
	if err != nil {
		t.Fatalf("ReadGGUFMetadata: %v", err)
	}

	if got := md.arch(); got != "testarch" {
		t.Errorf("arch = %q, want testarch", got)
	}
	if got, ok := md.archInt("block_count"); !ok || got != 40 {
		t.Errorf("block_count = %d (ok=%v), want 40", got, ok)
	}
	// A scalar must widen to a one-element slice so callers can treat scalar
	// and per-layer GQA uniformly.
	if got, ok := md.archIntSlice("attention.head_count_kv"); !ok || len(got) != 1 || got[0] != 8 {
		t.Errorf("head_count_kv = %v (ok=%v), want [8]", got, ok)
	}
	if got, ok := md.archIntSlice("per_layer"); !ok || len(got) != 3 || got[2] != 3 {
		t.Errorf("per_layer = %v, want [1 2 3]", got)
	}
	if got, ok := md.archBoolSlice("flags"); !ok || len(got) != 3 || got[1] {
		t.Errorf("flags = %v, want [true false true]", got)
	}
}

func TestReadGGUFMetadata_RejectsNonGGUF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-model.gguf")
	if err := os.WriteFile(path, []byte("this is not a gguf file at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadGGUFMetadata(path); err == nil {
		t.Fatal("expected an error for a file without the GGUF magic")
	}
}

func TestReadGGUFMetadata_RejectsTruncated(t *testing.T) {
	w := &ggufWriter{}
	w.str("general.architecture", "testarch")
	w.u32("testarch.block_count", 40)

	full := w.bytes()
	path := filepath.Join(t.TempDir(), "truncated.gguf")
	// Keep the header (which claims 2 KV entries) but cut the entries short.
	if err := os.WriteFile(path, full[:len(full)-12], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadGGUFMetadata(path); err == nil {
		t.Fatal("expected an error for a truncated metadata section")
	}
}

// ---------------------------------------------------------------------------
// KV cache math
// ---------------------------------------------------------------------------

func TestKVCacheBytesPerElem(t *testing.T) {
	tests := []struct {
		cacheType string
		want      float64
	}{
		{"f16", 2.0},
		{"", 2.0}, // llama-server's default
		{"f32", 4.0},
		{"q8_0", 34.0 / 32.0}, // 32 quants + one f16 scale per block
		{"Q8_0", 34.0 / 32.0}, // case-insensitive
		{"q4_0", 18.0 / 32.0},
		// An unrecognized type must over-reserve rather than count as free.
		{"totally-made-up", 2.0},
	}
	for _, tc := range tests {
		if got := kvCacheBytesPerElem(tc.cacheType); got != tc.want {
			t.Errorf("kvCacheBytesPerElem(%q) = %v, want %v", tc.cacheType, got, tc.want)
		}
	}
}

func TestGGUFKVCacheBytes_UniformGQA(t *testing.T) {
	md := ggufMetadata{
		"general.architecture":            "uniform",
		"uniform.block_count":             int64(4),
		"uniform.attention.head_count_kv": int64(8),
		"uniform.attention.key_length":    int64(128),
		"uniform.attention.value_length":  int64(128),
	}

	// 4 layers x 8 kv heads x 1024 tokens x (128 K + 128 V) x q8_0 (34/32 B).
	const want = 4 * 8 * 1024 * (128 + 128) * 34 / 32

	got, err := ggufKVCacheBytes(md, 1024, "q8_0", "q8_0")
	if err != nil {
		t.Fatalf("ggufKVCacheBytes: %v", err)
	}
	if got != want {
		t.Errorf("kv = %d, want %d", got, want)
	}
}

func TestGGUFKVCacheBytes_SlidingWindowLayersUseTheWindow(t *testing.T) {
	// Gemma-shaped: 5 windowed layers then 1 global, repeated once. The
	// windowed layers hold 1024 tokens at head_dim 256, the global layers hold
	// the full context at head_dim 512 with a single KV head.
	md := ggufMetadata{
		"general.architecture":                       "gemmalike",
		"gemmalike.block_count":                      int64(6),
		"gemmalike.attention.head_count_kv":          []int64{8, 8, 8, 8, 8, 1},
		"gemmalike.attention.key_length":             int64(512),
		"gemmalike.attention.value_length":           int64(512),
		"gemmalike.attention.key_length_swa":         int64(256),
		"gemmalike.attention.value_length_swa":       int64(256),
		"gemmalike.attention.sliding_window":         int64(1024),
		"gemmalike.attention.sliding_window_pattern": []bool{true, true, true, true, true, false},
	}

	const ctx = 4096
	// 5 SWA layers: 1024 tokens x 8 heads x (256+256) x f16
	const swa = 5 * 1024 * 8 * (256 + 256) * 2
	// 1 global layer: 4096 tokens x 1 head x (512+512) x f16
	const global = 1 * 4096 * 1 * (512 + 512) * 2

	got, err := ggufKVCacheBytes(md, ctx, "f16", "f16")
	if err != nil {
		t.Fatalf("ggufKVCacheBytes: %v", err)
	}
	if got != swa+global {
		t.Errorf("kv = %d, want %d (swa=%d global=%d)", got, swa+global, swa, global)
	}

	// The naive full-context calculation is the bug this guards against: it
	// would report roughly 25x the real figure for a 12B Gemma at 32k.
	naive := int64(6 * ctx * 8 * (512 + 512) * 2)
	if got >= naive {
		t.Errorf("kv = %d is not smaller than the naive estimate %d; SWA was not applied", got, naive)
	}
}

func TestGGUFKVCacheBytes_WindowLargerThanContext(t *testing.T) {
	// A 1024-token window with a 256-token context must cost 256 tokens, not
	// 1024 — the window is a cap, not an allocation.
	md := ggufMetadata{
		"general.architecture":                   "small",
		"small.block_count":                      int64(1),
		"small.attention.head_count_kv":          []int64{4},
		"small.attention.key_length":             int64(64),
		"small.attention.value_length":           int64(64),
		"small.attention.sliding_window":         int64(1024),
		"small.attention.sliding_window_pattern": []bool{true},
	}

	got, err := ggufKVCacheBytes(md, 256, "f16", "f16")
	if err != nil {
		t.Fatalf("ggufKVCacheBytes: %v", err)
	}
	const want = 1 * 256 * 4 * (64 + 64) * 2
	if got != want {
		t.Errorf("kv = %d, want %d", got, want)
	}
}

func TestGGUFKVCacheBytes_FallsBackToEmbeddingOverHeads(t *testing.T) {
	// Older conversions omit key_length/value_length; head_dim is then
	// embedding_length / head_count.
	md := ggufMetadata{
		"general.architecture":           "legacy",
		"legacy.block_count":             int64(2),
		"legacy.attention.head_count_kv": int64(4),
		"legacy.attention.head_count":    int64(16),
		"legacy.embedding_length":        int64(2048), // head_dim = 128
	}

	got, err := ggufKVCacheBytes(md, 512, "f16", "f16")
	if err != nil {
		t.Fatalf("ggufKVCacheBytes: %v", err)
	}
	const want = 2 * 512 * 4 * (128 + 128) * 2
	if got != want {
		t.Errorf("kv = %d, want %d", got, want)
	}
}

func TestGGUFKVCacheBytes_MissingGeometryErrors(t *testing.T) {
	md := ggufMetadata{
		"general.architecture": "broken",
		"broken.block_count":   int64(4),
		// no head_count_kv, and no way to derive a head dimension
	}
	if _, err := ggufKVCacheBytes(md, 1024, "f16", "f16"); err == nil {
		t.Fatal("expected an error when the attention geometry is missing")
	}
}

// ---------------------------------------------------------------------------
// Estimation
// ---------------------------------------------------------------------------

func TestEstimateModelMemory_ExplicitOverrideWins(t *testing.T) {
	cfg := ServerModelConfig{
		Alias: "override",
		Args:  map[string]any{"memoryGB": 26.0, "model": "/does/not/exist.gguf"},
	}
	got := estimateModelMemory(llamaProfile, cfg, 10)
	want := int64(26 * bytesPerGB)
	if got != want {
		t.Errorf("estimate = %d (%s), want %d", got, formatGB(got), want)
	}
}

func TestEstimateModelMemory_UnreadableModelIsUnknown(t *testing.T) {
	cfg := ServerModelConfig{
		Alias: "missing",
		Args:  map[string]any{"model": "/does/not/exist.gguf"},
	}
	// Unknown must be 0 — the manager treats that as "cannot be size-budgeted"
	// rather than guessing a number that would silently mis-admit.
	if got := estimateModelMemory(llamaProfile, cfg, 10); got != 0 {
		t.Errorf("estimate = %d, want 0 for an unreadable model", got)
	}
}

func TestEstimateModelMemory_AppliesHeadroom(t *testing.T) {
	w := &ggufWriter{}
	w.str("general.architecture", "tiny")
	w.u32("tiny.block_count", 1)
	w.u32("tiny.attention.head_count_kv", 1)
	w.u32("tiny.attention.key_length", 64)
	w.u32("tiny.attention.value_length", 64)
	path := writeTestGGUF(t, w)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	cfg := ServerModelConfig{
		Alias: "tiny",
		Args:  map[string]any{"model": path, "ctx-size": 128.0},
	}

	const kv = 1 * 128 * 1 * (64 + 64) * 2 // f16 default
	base := info.Size() + kv

	got := estimateModelMemory(llamaProfile, cfg, 50)
	want := base + base/2
	if got != want {
		t.Errorf("estimate = %d, want %d (base %d + 50%% headroom)", got, want, base)
	}
}

func TestFormatGB(t *testing.T) {
	gb := 26.14
	if got := formatGB(int64(gb * float64(bytesPerGB))); got != "26.14GB" {
		t.Errorf("formatGB = %q, want 26.14GB", got)
	}
	if got := formatGB(0); got != "0.00GB" {
		t.Errorf("formatGB(0) = %q, want 0.00GB", got)
	}
}
