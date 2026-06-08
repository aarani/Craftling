package runspec

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestArgv(t *testing.T) {
	cases := []struct {
		name string
		spec RunSpec
		want []string
	}{
		{"entrypoint+cmd", RunSpec{Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{"echo hi"}}, []string{"/bin/sh", "-c", "echo hi"}},
		{"cmd only", RunSpec{Cmd: []string{"server"}}, []string{"server"}},
		{"empty", RunSpec{}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.spec.Argv(); !reflect.DeepEqual(got, c.want) {
				t.Errorf("Argv() = %v, want %v", got, c.want)
			}
		})
	}
}

// mmdsServer emulates the subset of Firecracker's MMDS the init agent
// uses: an optional v2 token endpoint and path traversal over the data
// object the host published, returning leaves as application/json.
func mmdsServer(t *testing.T, data any, v2 bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == mmdsTokenPath {
			if r.Method != http.MethodPut {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if !v2 {
				// v1 mode: token endpoint is not available.
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if r.Header.Get("X-metadata-token-ttl-seconds") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte("test-token"))
			return
		}

		if v2 && r.Header.Get("X-metadata-token") != "test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Traverse data by the request path.
		node := data
		for _, seg := range strings.Split(strings.Trim(r.URL.Path, "/"), "/") {
			m, ok := node.(map[string]any)
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			node, ok = m[seg]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(node)
	}))
}

func TestMMDSRoundTrip(t *testing.T) {
	spec := RunSpec{
		Entrypoint: []string{"/usr/bin/java", "-jar"},
		Cmd:        []string{"server.jar", "--nogui"},
		Env:        []string{"PATH=/usr/bin", "EULA=true"},
		WorkingDir: "/data",
	}
	data, err := spec.MMDSData()
	if err != nil {
		t.Fatalf("MMDSData: %v", err)
	}

	for _, v2 := range []bool{true, false} {
		name := "v1"
		if v2 {
			name = "v2"
		}
		t.Run(name, func(t *testing.T) {
			srv := mmdsServer(t, data, v2)
			defer srv.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			got, err := fetchFromMMDS(ctx, srv.Client(), srv.URL)
			if err != nil {
				t.Fatalf("fetchFromMMDS: %v", err)
			}
			if !reflect.DeepEqual(*got, spec) {
				t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", *got, spec)
			}
		})
	}
}

func TestMMDSDataShape(t *testing.T) {
	spec := RunSpec{Cmd: []string{"run"}}
	data, err := spec.MMDSData()
	if err != nil {
		t.Fatalf("MMDSData: %v", err)
	}
	// The run spec must be embedded as a single JSON-string leaf so the
	// guest never depends on MMDS's handling of nested objects/arrays.
	top, ok := data.(map[string]any)[mmdsCraftlingKey].(map[string]any)
	if !ok {
		t.Fatalf("missing %q object", mmdsCraftlingKey)
	}
	leaf, ok := top[mmdsRunspecKey].(string)
	if !ok {
		t.Fatalf("%q leaf is not a JSON string: %T", mmdsRunspecKey, top[mmdsRunspecKey])
	}
	var back RunSpec
	if err := json.Unmarshal([]byte(leaf), &back); err != nil {
		t.Fatalf("leaf is not valid run spec JSON: %v", err)
	}
	if !reflect.DeepEqual(back, spec) {
		t.Errorf("leaf decode mismatch: got %+v want %+v", back, spec)
	}
}
