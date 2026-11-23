package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/utils"
)

const modelProbeTimeout = 800 * time.Millisecond

var probeOpenAICompatibleModelFunc = probeOpenAICompatibleModel

func hasModelConfiguration(prov *modelspec.Provider, m modelspec.Model) bool {
	if modelspec.IsCLIProtocol(prov.Protocol) || config.IsHumanProtocol(prov.Protocol) {
		return true
	}
	if hasLocalAPIBase(prov.BaseURL) {
		return true
	}
	return strings.TrimSpace(prov.APIKey) != ""
}

// isModelConfigured reports whether a model is currently available to use.
// Local models must be reachable; remote/API-key models only need saved config.
func isModelConfigured(prov *modelspec.Provider, m modelspec.Model) bool {
	if !hasModelConfiguration(prov, m) {
		return false
	}
	if requiresRuntimeProbe(prov) {
		return probeLocalModelAvailability(prov, m)
	}
	return true
}

func requiresRuntimeProbe(prov *modelspec.Provider) bool {
	if modelspec.IsCLIProtocol(prov.Protocol) {
		return true
	}
	return hasLocalAPIBase(prov.BaseURL)
}

func probeLocalModelAvailability(prov *modelspec.Provider, m modelspec.Model) bool {
	// CLI providers authenticate out-of-band and are treated as available.
	if modelspec.IsCLIProtocol(prov.Protocol) {
		return true
	}
	apiBase := normalizeModelProbeAPIBase(prov.BaseURL)
	if hasLocalAPIBase(apiBase) {
		return probeOpenAICompatibleModelFunc(apiBase, m.Model)
	}
	return false
}

func normalizeModelProbeAPIBase(raw string) string {
	u, err := parseAPIBase(raw)
	if err != nil {
		return strings.TrimSpace(raw)
	}

	switch strings.ToLower(u.Hostname()) {
	case "0.0.0.0":
		u.Host = net.JoinHostPort("127.0.0.1", u.Port())
	case "::":
		u.Host = net.JoinHostPort("::1", u.Port())
	default:
		return strings.TrimSpace(raw)
	}

	if u.Port() == "" {
		u.Host = u.Hostname()
	}

	return u.String()
}

func hasLocalAPIBase(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}

	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		u, err = url.Parse("//" + raw)
		if err != nil {
			return false
		}
	}

	switch strings.ToLower(u.Hostname()) {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	default:
		return false
	}
}

func probeOpenAICompatibleModel(apiBase, modelID string) bool {
	if strings.TrimSpace(apiBase) == "" {
		return false
	}

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := getJSON(strings.TrimRight(strings.TrimSpace(apiBase), "/")+"/models", &resp); err != nil {
		return false
	}

	for _, model := range resp.Data {
		if strings.EqualFold(strings.TrimSpace(model.ID), modelID) {
			return true
		}
	}
	return false
}

func getJSON(rawURL string, out any) error {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: modelProbeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { utils.CloseQuietly(resp.Body) }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

func parseAPIBase(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty api base")
	}

	u, err := url.Parse(raw)
	if err == nil && u.Hostname() != "" {
		return u, nil
	}

	u, err = url.Parse("//" + raw)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("invalid api base %q", raw)
	}
	if u.Scheme == "" {
		u.Scheme = "http"
	}
	return u, nil
}
