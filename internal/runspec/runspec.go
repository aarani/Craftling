// Package runspec is the contract between the image converter, the
// host-side Firecracker driver, and the in-VM init agent.
//
// The converter (internal/image) distills the OCI image config into a
// RunSpec. The host driver (internal/agent/firecracker) publishes that
// RunSpec into the microVM's MMDS (Firecracker's metadata service) at
// boot. The init agent (cmd/init) fetches it back from MMDS over HTTP
// at the link-local address and execs the entrypoint.
//
// Delivering the spec via MMDS rather than baking it into the rootfs
// keeps the rootfs image immutable and content-addressed: the same
// squashfs can boot with different commands, env, or working dirs by
// changing only the metadata the host pushes. MMDS is the
// Firecracker-native channel for exactly this — mutable, per-VM,
// host-controlled data the guest reads with no shared filesystem.
//
// The package is deliberately tiny and stdlib-only so the init binary —
// which ships inside every rootfs and wants to stay small — can depend
// on it without dragging in go-containerregistry or anything else the
// converter needs.
package runspec

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// InstallDir is the pipeline-owned namespace inside the rootfs. The
// converter strips any copy the user's image ships and writes its own
// init binary here. The kernel boots with init=/.craftling/init.
const InstallDir = "/.craftling"

// InitPath is where the init binary is dropped inside the rootfs.
const InitPath = InstallDir + "/init"

// MMDS addressing and the guest's link-local network. 169.254.169.254
// is Firecracker's default MMDS address (the same EC2 IMDS uses). The
// guest's own address shares the 169.254.0.0/16 link-local block so the
// MMDS address is on-link — no route needed. Host and guest both import
// these so the kernel ip= boot arg the driver emits and the interface
// the init agent configures can't drift apart.
const (
	// MMDSIPv4 is the link-local address the MMDS answers on.
	MMDSIPv4 = "169.254.169.254"

	// GuestIPv4 is the static address the guest's MMDS interface (eth0)
	// takes. GuestNetmask / GuestPrefixLen describe its /16 link-local
	// subnet; GuestGatewayIPv4 is a nominal gateway for the ip= boot arg
	// (unused for on-link MMDS traffic).
	GuestIPv4        = "169.254.169.2"
	GuestGatewayIPv4 = "169.254.169.1"
	GuestNetmask     = "255.255.0.0"
	GuestPrefixLen   = 16

	// MMDSInterface is the guest network-interface name the kernel and
	// init agent expect for MMDS.
	MMDSInterface = "eth0"

	// MMDSBaseURL is the root of the metadata service as seen by the
	// guest.
	MMDSBaseURL = "http://" + MMDSIPv4

	// MMDSRunspecPath is the MMDS path the RunSpec is published at. It
	// mirrors the nested object the host PUTs: {craftling:{runspec:…}}.
	MMDSRunspecPath = "/" + mmdsCraftlingKey + "/" + mmdsRunspecKey

	// mmdsTokenPath is the MMDS v2 session-token endpoint.
	mmdsTokenPath = "/latest/api/token"

	// mmdsTokenTTL is the requested lifetime (seconds) of an MMDS v2
	// session token. The init agent uses the token once, immediately.
	mmdsTokenTTL = "60"

	mmdsCraftlingKey = "craftling"
	mmdsRunspecKey   = "runspec"
)

// RunSpec is the subset of an OCI image's runtime configuration the
// init agent needs to launch the workload. Field semantics match the
// OCI image-spec config block.
type RunSpec struct {
	// Entrypoint is the fixed prefix of the command. May be empty.
	Entrypoint []string `json:"entrypoint,omitempty"`

	// Cmd is the default arguments appended after Entrypoint. When
	// Entrypoint is empty, Cmd is the whole command.
	Cmd []string `json:"cmd,omitempty"`

	// Env is the process environment as "KEY=VALUE" entries, copied
	// verbatim from the image config.
	Env []string `json:"env,omitempty"`

	// WorkingDir is the directory the init agent chdirs into before
	// exec. Empty means "/".
	WorkingDir string `json:"working_dir,omitempty"`
}

// Argv returns the full command line (Entrypoint followed by Cmd).
// Returns nil when both are empty — the init agent treats that as
// "nothing to run".
func (s *RunSpec) Argv() []string {
	argv := make([]string, 0, len(s.Entrypoint)+len(s.Cmd))
	argv = append(argv, s.Entrypoint...)
	argv = append(argv, s.Cmd...)
	if len(argv) == 0 {
		return nil
	}
	return argv
}

// MMDSData builds the object the host PUTs into MMDS. The RunSpec is
// embedded as a single JSON-string leaf rather than as a nested object
// so the guest never depends on MMDS's handling of typed leaves or
// arrays — it fetches one string and unmarshals it. The shape is
// {"craftling": {"runspec": "<json>"}}, fetched by the guest at
// MMDSRunspecPath.
func (s *RunSpec) MMDSData() (any, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("marshal run spec: %w", err)
	}
	return map[string]any{
		mmdsCraftlingKey: map[string]any{
			mmdsRunspecKey: string(raw),
		},
	}, nil
}

// FetchFromMMDS retrieves and parses the RunSpec from the guest's MMDS
// endpoint. It negotiates an MMDS v2 session token first and falls back
// to token-less v1 access if the host configured MMDS in v1 mode (the
// token endpoint then answers 404/405). client supplies the timeout
// and transport; ctx bounds the whole exchange.
func FetchFromMMDS(ctx context.Context, client *http.Client) (*RunSpec, error) {
	return fetchFromMMDS(ctx, client, MMDSBaseURL)
}

// fetchFromMMDS is FetchFromMMDS with the base URL injected, so tests
// can point it at an httptest server.
func fetchFromMMDS(ctx context.Context, client *http.Client, baseURL string) (*RunSpec, error) {
	token, err := mmdsToken(ctx, client, baseURL)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+MMDSRunspecPath, nil)
	if err != nil {
		return nil, fmt.Errorf("build mmds request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("X-metadata-token", token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get mmds %s: %w", MMDSRunspecPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read mmds body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mmds %s: status %d: %s", MMDSRunspecPath, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// The leaf is a JSON string whose contents are the RunSpec JSON.
	var inner string
	if err := json.Unmarshal(body, &inner); err != nil {
		return nil, fmt.Errorf("decode mmds leaf: %w", err)
	}
	var spec RunSpec
	if err := json.Unmarshal([]byte(inner), &spec); err != nil {
		return nil, fmt.Errorf("parse run spec from mmds: %w", err)
	}
	return &spec, nil
}

// mmdsToken requests an MMDS v2 session token. An empty token with a
// nil error means the host runs MMDS v1 (no token required); any other
// failure is returned.
func mmdsToken(ctx context.Context, client *http.Client, baseURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, baseURL+mmdsTokenPath, nil)
	if err != nil {
		return "", fmt.Errorf("build mmds token request: %w", err)
	}
	req.Header.Set("X-metadata-token-ttl-seconds", mmdsTokenTTL)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request mmds token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return "", fmt.Errorf("read mmds token: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return strings.TrimSpace(string(body)), nil
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		// MMDS v1: tokens are not used.
		return "", nil
	default:
		return "", fmt.Errorf("mmds token: status %d", resp.StatusCode)
	}
}
