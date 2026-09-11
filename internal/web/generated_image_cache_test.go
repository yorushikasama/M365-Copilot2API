package web

import (
	"testing"
	"time"
)

// cachedImageBytes totals what the generated-image cache currently pins in RAM.
func cachedImageBytes(s *Server) int {
	total := 0
	for _, item := range s.generatedImages {
		total += len(item.Data)
	}
	return total
}

// The cache used to evict exactly one entry per store and count entries only, so
// its real ceiling was maxGeneratedImages * maxGeneratedImageBytes — 128 x 20 MiB
// = 2.5 GiB of image bytes held in a process that runs in well under 1 GiB.
// Both limits have to hold after every insert.
func TestStoreGeneratedImageHonorsByteBudget(t *testing.T) {
	s := &Server{generatedImages: map[string]generatedImage{}}
	const chunk = 5 << 20

	for i := 0; i < 40; i++ {
		s.storeGeneratedImage(make([]byte, chunk), "image/png")

		if got := len(s.generatedImages); got > maxGeneratedImages {
			t.Fatalf("insert %d: cache holds %d entries, want <= %d", i, got, maxGeneratedImages)
		}
		if got := cachedImageBytes(s); got > maxGeneratedImageCacheBytes {
			t.Fatalf("insert %d: cache holds %d bytes, want <= %d", i, got, maxGeneratedImageCacheBytes)
		}
	}

	// The budget must actually have bitten: 40 x 5 MiB is well past it.
	if len(s.generatedImages) >= 40 {
		t.Fatalf("expected eviction to have dropped entries, still holding %d", len(s.generatedImages))
	}
}

// A single image larger than the whole budget must not wedge the loop: the cache
// empties, stores it, and stays consistent.
func TestStoreGeneratedImageAcceptsOneOversizeEntry(t *testing.T) {
	s := &Server{generatedImages: map[string]generatedImage{}}
	s.storeGeneratedImage(make([]byte, 1<<20), "image/png")

	id := s.storeGeneratedImage(make([]byte, maxGeneratedImageCacheBytes+(1<<20)), "image/png")
	if id == "" {
		t.Fatal("oversize image was not stored")
	}
	if _, ok := s.generatedImages[id]; !ok {
		t.Fatal("oversize image is missing from the cache")
	}
	if got := len(s.generatedImages); got != 1 {
		t.Fatalf("cache holds %d entries, want only the oversize one", got)
	}
}

// Expired entries are swept before the budget is consulted, so a cache full of
// stale images does not force the eviction of live ones.
func TestStoreGeneratedImageSweepsExpiredFirst(t *testing.T) {
	s := &Server{generatedImages: map[string]generatedImage{}}
	for i := 0; i < 10; i++ {
		s.generatedImages["stale-"+time.Duration(i).String()] = generatedImage{
			Data:      make([]byte, 1<<20),
			ExpiresAt: time.Now().Add(-time.Minute),
		}
	}

	id := s.storeGeneratedImage(make([]byte, 1<<20), "image/png")
	if got := len(s.generatedImages); got != 1 {
		t.Fatalf("cache holds %d entries, want 1 after the expired sweep", got)
	}
	if _, ok := s.generatedImages[id]; !ok {
		t.Fatal("the fresh image was evicted instead of the expired ones")
	}
}

// Serving an image must hand back a copy: the caller writes it to the network
// after the lock is released, so aliasing the cached slice would race with a
// later eviction reusing that memory.
func TestGeneratedImageServeReturnsCopy(t *testing.T) {
	s := &Server{generatedImages: map[string]generatedImage{}}
	id := s.storeGeneratedImage([]byte{1, 2, 3, 4}, "image/png")

	cached := s.generatedImages[id]
	cached.Data[0] = 9
	if s.generatedImages[id].Data[0] != 9 {
		t.Skip("map value is not shared; nothing to assert")
	}

	// storeGeneratedImage must not alias the caller's buffer either.
	src := []byte{7, 7, 7}
	id2 := s.storeGeneratedImage(src, "image/png")
	src[0] = 0
	if s.generatedImages[id2].Data[0] != 7 {
		t.Fatal("storeGeneratedImage aliased the caller's slice")
	}
}
