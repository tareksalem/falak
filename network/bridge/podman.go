package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

// ErrPodmanNetworkNotFound signals the Podman daemon reported the network
// did not exist. Destroy paths treat this as success (reaper-idempotent).
var ErrPodmanNetworkNotFound = errors.New("bridge: podman network not found")

// PodmanNetwork mirrors the subset of a Podman network bridge.Manager uses.
type PodmanNetwork struct{ Name, Subnet, Gateway string; MTU int }

// PodmanNetworkClient is the narrow contract bridge.Manager uses to talk
// to Podman. The interface is the seam tests inject fakes through.
type PodmanNetworkClient interface {
	// CreateNetwork provisions a bridge network. Caller handles rollback on error.
	CreateNetwork(ctx context.Context, name, subnet, gateway string, mtu int) error
	// DeleteNetwork removes the named network; ErrPodmanNetworkNotFound when absent.
	DeleteNetwork(ctx context.Context, name string) error
	// InspectNetwork returns the named network or ErrPodmanNetworkNotFound when absent.
	InspectNetwork(ctx context.Context, name string) (*PodmanNetwork, error)
}

// HTTPPodmanClient talks to the Podman libpod REST API over a unix socket.
// It implements only the network endpoints bridge.Manager needs, keeping
// the network/ module free of any runtime/ import.
type HTTPPodmanClient struct {
	client     *http.Client
	socketPath string
	apiVersion string
}

// HTTPOption configures an HTTPPodmanClient.
type HTTPOption func(*HTTPPodmanClient)

// WithHTTPSocketPath overrides the auto-detected Podman socket path.
func WithHTTPSocketPath(p string) HTTPOption { return func(c *HTTPPodmanClient) { c.socketPath = p } }

// WithHTTPAPIVersion overrides the default Podman API version ("v5.0.0").
func WithHTTPAPIVersion(v string) HTTPOption { return func(c *HTTPPodmanClient) { c.apiVersion = v } }

// NewHTTPPodmanClient constructs an HTTP client bound to the Podman unix
// socket. Auto-detects rootless then rootful when no path is given.
func NewHTTPPodmanClient(opts ...HTTPOption) *HTTPPodmanClient {
	c := &HTTPPodmanClient{apiVersion: "v5.0.0"}
	for _, opt := range opts {
		opt(c)
	}
	if c.socketPath == "" {
		c.socketPath = detectPodmanSocket()
	}
	c.client = &http.Client{Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", c.socketPath)
		},
	}}
	return c
}

func detectPodmanSocket() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		sock := filepath.Join(xdg, "podman", "podman.sock")
		if _, err := os.Stat(sock); err == nil {
			return sock
		}
	}
	return "/run/podman/podman.sock"
}

// CreateNetwork posts a libpod network create payload to Podman.
func (c *HTTPPodmanClient) CreateNetwork(ctx context.Context, name, subnet, gateway string, mtu int) error {
	payload := map[string]any{
		"name": name, "driver": "bridge",
		"subnets": []map[string]string{{"subnet": subnet, "gateway": gateway}},
	}
	if mtu > 0 {
		payload["options"] = map[string]string{"mtu": strconv.Itoa(mtu)}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("bridge: marshal podman create: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/libpod/networks/create", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return statusErr("create", name, resp)
	}
	return nil
}

// DeleteNetwork removes the named libpod network.
func (c *HTTPPodmanClient) DeleteNetwork(ctx context.Context, name string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/libpod/networks/"+name, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrPodmanNetworkNotFound
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return statusErr("delete", name, resp)
	}
	return nil
}

// InspectNetwork queries the named libpod network.
func (c *HTTPPodmanClient) InspectNetwork(ctx context.Context, name string) (*PodmanNetwork, error) {
	resp, err := c.do(ctx, http.MethodGet, "/libpod/networks/"+name+"/json", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrPodmanNetworkNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("inspect", name, resp)
	}
	var raw struct {
		Name    string                       `json:"name"`
		Subnets []struct{ Subnet, Gateway string } `json:"subnets"`
		Options map[string]string            `json:"options"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("bridge: podman inspect decode: %w", err)
	}
	out := &PodmanNetwork{Name: raw.Name}
	if len(raw.Subnets) > 0 {
		out.Subnet, out.Gateway = raw.Subnets[0].Subnet, raw.Subnets[0].Gateway
	}
	if mtu, err := strconv.Atoi(raw.Options["mtu"]); err == nil {
		out.MTU = mtu
	}
	return out, nil
}

func (c *HTTPPodmanClient) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://d/"+c.apiVersion+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("bridge: podman %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bridge: podman %s %s: %w", method, path, err)
	}
	return resp, nil
}

func statusErr(op, name string, resp *http.Response) error {
	b, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("bridge: podman %s %s: status %d: %s", op, name, resp.StatusCode, b)
}
