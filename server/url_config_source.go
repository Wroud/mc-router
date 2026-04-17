package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"
)

// URLConfigSource periodically fetches routes config from a remote URL
// and reloads routes, treating the URL as the source of truth.
type URLConfigSource struct {
	url      string
	apiKey   string
	interval time.Duration
	client   *http.Client
}

// NewURLConfigSource creates a new URLConfigSource.
func NewURLConfigSource(url, apiKey string, interval time.Duration) *URLConfigSource {
	return &URLConfigSource{
		url:      url,
		apiKey:   apiKey,
		interval: interval,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Start begins polling the URL on the configured interval.
// It blocks until the context is cancelled.
func (u *URLConfigSource) Start(ctx context.Context) {
	log := logrus.WithField("source", u.url)
	log.WithField("interval", u.interval).Info("Starting URL config source polling")

	// Initial fetch
	if err := u.reload(); err != nil {
		log.WithError(err).Warn("Initial URL config fetch failed, will retry")
	}

	ticker := time.NewTicker(u.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := u.reload(); err != nil {
				log.WithError(err).Warn("URL config fetch failed, keeping previous config")
			}
		case <-ctx.Done():
			log.Info("URL config source polling stopped")
			return
		}
	}
}

func (u *URLConfigSource) reload() error {
	req, err := http.NewRequest("GET", u.url, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	if u.apiKey != "" {
		req.Header.Set("x-api-key", u.apiKey)
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetching config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	var config RoutesConfigSchema
	if err := json.Unmarshal(body, &config); err != nil {
		return fmt.Errorf("parsing config JSON: %w", err)
	}

	// Full reload — reset and re-apply, same as RoutesConfigLoader.Reload()
	Routes.Reset()
	Routes.RegisterAll(config.Mappings)
	if config.DefaultServer != "" {
		Routes.SetDefaultRoute(config.DefaultServer, "", nil, nil, "", "")
	}
	if config.FallbackServer != "" {
		Routes.SetFallbackRoute(config.FallbackServer)
	}

	logrus.WithField("count", len(config.Mappings)).Debug("Reloaded routes config from URL")
	return nil
}
