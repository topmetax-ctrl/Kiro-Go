package apikey

import (
	"fmt"
	"path/filepath"
	"testing"
)

func benchService(b *testing.B, n int) (*Service, []string) {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "apikeys.db"), []byte("bench-pepper-32-bytes-long!!!!!"), Options{})
	if err != nil {
		b.Fatal(err)
	}
	secrets := make([]string, n)
	for i := 0; i < n; i++ {
		rec, secret, err := s.Create(CreateInput{Name: fmt.Sprintf("k-%d", i), Enabled: true})
		if err != nil {
			b.Fatal(err)
		}
		_ = rec
		secrets[i] = secret
	}
	return s, secrets
}

func benchLookupN(b *testing.B, n int) {
	s, secrets := benchService(b, n)
	defer s.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Lookup(secrets[i%n]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLookup100(b *testing.B)   { benchLookupN(b, 100) }
func BenchmarkLookup1000(b *testing.B)  { benchLookupN(b, 1000) }
func BenchmarkLookup10000(b *testing.B) { benchLookupN(b, 10000) }
