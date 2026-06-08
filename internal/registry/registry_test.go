package registry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const manifestBody = `{"image_name":"itzg/minecraft-server","image_tag":"java21","eula_needed":true}`

// newTestClient stands up a registry whose index advertises a single template
// whose manifest lives on the same server, and returns the client plus per-path
// hit counters so caching can be asserted.
func newTestClient(t *testing.T) (c *Client, indexHits, manifestHits *int32) {
	t.Helper()
	var idx, man int32

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	index := `[{"template_id":"vanilla-1","template_name":"Vanilla Minecraft",` +
		`"thumbnail_url":"` + srv.URL + `/vanilla.png",` +
		`"template_url":"` + srv.URL + `/vanilla.json"}]`

	mux.HandleFunc("/index.json", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&idx, 1)
		_, _ = w.Write([]byte(index))
	})
	mux.HandleFunc("/vanilla.json", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&man, 1)
		_, _ = w.Write([]byte(manifestBody))
	})

	return New(srv.URL+"/index.json", srv.Client()), &idx, &man
}

func TestIndexAndManifest(t *testing.T) {
	c, _, _ := newTestClient(t)
	ctx := context.Background()

	idx, err := c.Index(ctx)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(idx) != 1 || idx[0].TemplateName != "Vanilla Minecraft" {
		t.Fatalf("unexpected index: %+v", idx)
	}

	body, err := c.Manifest(ctx, "vanilla-1")
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if string(body) != manifestBody {
		t.Fatalf("manifest passthrough mismatch: %s", body)
	}

	if _, err := c.Manifest(ctx, "does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestCaching(t *testing.T) {
	c, indexHits, manifestHits := newTestClient(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := c.Index(ctx); err != nil {
			t.Fatalf("Index #%d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(indexHits); got != 1 {
		t.Fatalf("expected 1 upstream index hit (cached), got %d", got)
	}

	for i := 0; i < 3; i++ {
		if _, err := c.Manifest(ctx, "vanilla-1"); err != nil {
			t.Fatalf("Manifest #%d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(manifestHits); got != 1 {
		t.Fatalf("expected 1 upstream manifest hit (cached), got %d", got)
	}
}

func TestUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, srv.Client())
	if _, err := c.Index(context.Background()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected upstream 500 error, got %v", err)
	}
}
