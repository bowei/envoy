// Package admin is a client for the Envoy admin interface.
//
// Only the read-only endpoints envoy-view needs are implemented. The admin
// interface is unauthenticated by design and is never safe to expose publicly,
// so callers are expected to point this at a localhost or otherwise trusted
// address.
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout bounds a single admin request. Config dumps on a large Envoy
// can be tens of megabytes, so this is generous.
const DefaultTimeout = 60 * time.Second

// Client talks to one Envoy admin interface.
type Client struct {
	base *url.URL
	http *http.Client
}

// New returns a client for the admin interface at addr. addr may be given as a
// full URL ("http://127.0.0.1:9901") or as a bare host:port, in which case http
// is assumed.
func New(addr string) (*Client, error) {
	if addr == "" {
		return nil, fmt.Errorf("admin address is empty")
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil {
		return nil, fmt.Errorf("parse admin address %q: %w", addr, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("admin address %q has no host", addr)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return &Client{
		base: u,
		http: &http.Client{Timeout: DefaultTimeout},
	}, nil
}

// Addr returns the base address the client was built with.
func (c *Client) Addr() string { return c.base.String() }

// ConfigDump fetches /config_dump and returns the raw JSON body.
//
// include_eds asks Envoy to include the EndpointsConfigDump section. It is off
// by default because endpoint data dominates the response size on large
// deployments, but without it EDS clusters have no endpoints to link to.
func (c *Client) ConfigDump(ctx context.Context, includeEDS bool) ([]byte, error) {
	q := url.Values{}
	if includeEDS {
		// Envoy treats the bare presence of the key as true.
		q.Set("include_eds", "")
	}
	return c.get(ctx, "/config_dump", q)
}

// ServerInfo is the subset of /server_info envoy-view displays.
type ServerInfo struct {
	Version string `json:"version"`
	State   string `json:"state"`
	Node    struct {
		ID      string `json:"id"`
		Cluster string `json:"cluster"`
	} `json:"node"`
	UptimeCurrentEpoch string `json:"uptime_current_epoch"`
}

// ServerInfo fetches /server_info.
func (c *Client) ServerInfo(ctx context.Context) (*ServerInfo, error) {
	body, err := c.get(ctx, "/server_info", nil)
	if err != nil {
		return nil, err
	}
	var info ServerInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("decode /server_info: %w", err)
	}
	return &info, nil
}

func (c *Client) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	u := *c.base
	u.Path = c.base.Path + path
	if len(q) > 0 {
		// url.Values.Encode renders a valueless key as "k=", which Envoy
		// accepts, but the documented form is a bare "k".
		var parts []string
		for k, vs := range q {
			for _, v := range vs {
				if v == "" {
					parts = append(parts, url.QueryEscape(k))
				} else {
					parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
				}
			}
		}
		u.RawQuery = strings.Join(parts, "&")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", u.String(), err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("GET %s: read body: %w", u.String(), err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := string(body)
		if len(snippet) > 256 {
			snippet = snippet[:256] + "..."
		}
		return nil, fmt.Errorf("GET %s: %s: %s", u.String(), resp.Status, snippet)
	}
	return body, nil
}
