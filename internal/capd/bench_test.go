package capd

import (
	"context"
	"path/filepath"
	"testing"
)

// BenchmarkAdmission measures durable admissions and settlements through the writer.
func BenchmarkAdmission(b *testing.B) {
	for _, mode := range []string{"FULL", "NORMAL"} {
		b.Run(mode, func(b *testing.B) {
			l, _, err := Open(filepath.Join(b.TempDir(), "l.db"), mode)
			if err != nil {
				b.Fatal(err)
			}
			defer l.Close()
			scope := []Scope{{ID: "key:a", Cap: 1 << 62}}
			ctx := context.Background()
			b.SetParallelism(32)
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					id, _, err := l.Reserve(ctx, scope, 10)
					if err != nil {
						b.Error(err)
						return
					}
					if err := l.Settle(ctx, id, 5); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}
