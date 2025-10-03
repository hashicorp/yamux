package yamux_test

import (
	"testing"
)

func BenchmarkPing(b *testing.B) {
	client, _ := testClientServer(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := client.Ping(); err != nil {
			b.Fatalf("err: %v", err)
		}
	}
}
