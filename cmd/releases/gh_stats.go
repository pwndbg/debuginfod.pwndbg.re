package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

type GitHubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	PublishedAt string        `json:"published_at"`
	Assets      []GitHubAsset `json:"assets"`
}

type GitHubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	DownloadCount      int    `json:"download_count"`
	ContentType        string `json:"content_type"`
	Size               int    `json:"size"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
}

type DownloadStats struct {
	Timestamp     time.Time
	ReleaseTag    string
	AssetName     string
	DownloadCount int
}

type ghCollector struct {
	db *dbSrv
}

func NewGhCollector(db *dbSrv) *ghCollector {
	return &ghCollector{
		db: db,
	}
}

func (ghc *ghCollector) Worker(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	if err := ghc.collectAndStore(); err != nil {
		log.WithError(err).Error("Error during collection")
	}

	for {
		select {
		case <-ticker.C:
			if err := ghc.collectAndStore(); err != nil {
				log.WithError(err).Error("Error during collection")
			}
		case <-ctx.Done():
			return
		}
	}
}

func (ghc *ghCollector) fetchGitHubRelease() (*GitHubRelease, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	const githubAPIURL = "https://api.github.com/repos/pwndbg/pwndbg/releases/latest"
	req, err := http.NewRequest("GET", githubAPIURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", "pwndbg-stats-collector")
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch data: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub API returned status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var release GitHubRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	return &release, nil
}

func (ghc *ghCollector) collectAndStore() error {
	release, err := ghc.fetchGitHubRelease()
	if err != nil {
		return fmt.Errorf("failed to fetch release: %w", err)
	}
	if len(release.Assets) == 0 {
		return errors.New("no assets found in this release")
	}

	timestamp := time.Now()
	stats := make([]DownloadStats, 0, len(release.Assets))
	for _, asset := range release.Assets {
		stats = append(stats, DownloadStats{
			Timestamp:     timestamp,
			ReleaseTag:    release.TagName,
			AssetName:     asset.Name,
			DownloadCount: asset.DownloadCount,
		})
	}
	return ghc.db.GhDownloadStats(context.Background(), stats)
}
