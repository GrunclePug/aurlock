// Package aur talks to the AUR: the RPC interface for package metadata and
// the git repositories that hold each package base. Nothing here runs a
// PKGBUILD.
package aur

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// DefaultURL is the real AUR.
const DefaultURL = "https://aur.archlinux.org"

// Package is the RPC's view of one package.
type Package struct {
	Name           string
	PackageBase    string
	Version        string
	Maintainer     string // "" if orphaned
	FirstSubmitted int64
	OutOfDate      int64 // 0 if not flagged
}

// Client queries the AUR RPC.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient returns a client for the AUR at baseURL.
func NewClient(baseURL string) *Client {
	return &Client{BaseURL: baseURL, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Info looks up packages by name. Names the AUR doesn't know are absent
// from the result.
func (c *Client) Info(ctx context.Context, names []string) (map[string]Package, error) {
	out := map[string]Package{}
	// The RPC accepts long argument lists, but URLs have limits.
	for len(names) > 0 {
		n := min(len(names), 100)
		q := url.Values{}
		for _, name := range names[:n] {
			q.Add("arg[]", name)
		}
		names = names[n:]
		if err := c.info(ctx, q, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (c *Client) info(ctx context.Context, q url.Values, out map[string]Package) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/rpc/v5/info?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("aur rpc: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("aur rpc: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("aur rpc: %s", resp.Status)
	}
	var r struct {
		Type    string
		Error   string
		Results []struct {
			Name           string
			PackageBase    string
			Version        string
			Maintainer     *string
			FirstSubmitted int64
			OutOfDate      *int64
		}
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("aur rpc: %w", err)
	}
	if r.Type == "error" {
		return fmt.Errorf("aur rpc: %s", r.Error)
	}
	for _, p := range r.Results {
		pkg := Package{Name: p.Name, PackageBase: p.PackageBase, Version: p.Version, FirstSubmitted: p.FirstSubmitted}
		if p.Maintainer != nil {
			pkg.Maintainer = *p.Maintainer
		}
		if p.OutOfDate != nil {
			pkg.OutOfDate = *p.OutOfDate
		}
		out[p.Name] = pkg
	}
	return nil
}
