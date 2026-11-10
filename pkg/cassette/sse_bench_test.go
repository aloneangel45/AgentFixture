package cassette

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func BenchmarkDecodeSSE_AnthropicStream(b *testing.B) {
	raw := []byte(strings.Repeat(sampleAnthropicSSE, 4))
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := DecodeSSE(bytes.NewReader(raw))
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReconstructStreamSummary(b *testing.B) {
	events, err := DecodeSSE(strings.NewReader(sampleAnthropicSSE))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		s := ReconstructStreamSummary(events)
		if s.Model == "" {
			b.Fatal("empty model")
		}
	}
}

func BenchmarkReplaySSEStream_Instant(b *testing.B) {
	events, err := DecodeSSE(strings.NewReader(sampleAnthropicSSE))
	if err != nil {
		b.Fatal(err)
	}
	encoded := EncodeSSEStream(events)
	b.SetBytes(int64(len(encoded)))
	b.ReportAllocs()
	ctx := context.Background()
	cfg := SSETimingConfig{Mode: SSETimingInstant}
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := ReplaySSEStream(ctx, io.Discard, nil, events, cfg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeSSEStream(b *testing.B) {
	events, err := DecodeSSE(strings.NewReader(sampleAnthropicSSE))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		out := EncodeSSEStream(events)
		if len(out) == 0 {
			b.Fatal("empty encoded stream")
		}
	}
}
