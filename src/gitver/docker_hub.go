package gitver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// TagInfo holds metadata for a single Docker Hub tag.
type TagInfo struct {
	Size        int64
	Digest      string
	LastUpdated time.Time
}

// DockerHubInfo holds metadata fetched from the Docker Hub API.
type DockerHubInfo struct {
	Pulls  int64              // total pull count
	Stars  int                // star count
	Size   int64              // compressed size of latest tag in bytes
	Latest string             // digest of latest tag (sha256:...)
	Tags   map[string]TagInfo // per-tag metadata
}

// FetchDockerHubInfo retrieves repository metadata from Docker Hub.
// namespace/repo format: "prplanit/stagefreight".
func FetchDockerHubInfo(namespace, repo string) (*DockerHubInfo, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	info := &DockerHubInfo{}

	// Fetch repository info (pulls, stars).
	repoURL := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/%s/", namespace, repo)
	resp, err := client.Get(repoURL)
	if err != nil {
		return nil, fmt.Errorf("docker hub repo: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker hub repo: %s", resp.Status)
	}

	var repoData struct {
		PullCount int64 `json:"pull_count"`
		StarCount int   `json:"star_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&repoData); err != nil {
		return nil, fmt.Errorf("docker hub repo decode: %w", err)
	}
	info.Pulls = repoData.PullCount
	info.Stars = repoData.StarCount

	// Fetch latest tag info (size, digest).
	tagURL := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/%s/tags/latest", namespace, repo)
	tagResp, err := client.Get(tagURL)
	if err == nil {
		defer tagResp.Body.Close()
		if tagResp.StatusCode == http.StatusOK {
			var tagData struct {
				FullSize int64  `json:"full_size"`
				Digest   string `json:"digest"`
				Images   []struct {
					Size   int64  `json:"size"`
					Digest string `json:"digest"`
				} `json:"images"`
			}
			if err := json.NewDecoder(tagResp.Body).Decode(&tagData); err == nil {
				info.Size = tagData.FullSize
				info.Latest = tagData.Digest
				// If no top-level size, sum from images.
				if info.Size == 0 && len(tagData.Images) > 0 {
					for _, img := range tagData.Images {
						info.Size += img.Size
					}
				}
				if info.Latest == "" && len(tagData.Images) > 0 {
					info.Latest = tagData.Images[0].Digest
				}
			}
		}
	}

	return info, nil
}

// FetchTagInfo retrieves metadata for specific tags from Docker Hub.
// Best-effort: tags that 404 or error are silently skipped.
func FetchTagInfo(client *http.Client, namespace, repo string, tags []string) map[string]TagInfo {
	result := make(map[string]TagInfo, len(tags))
	for _, tag := range tags {
		tagURL := fmt.Sprintf(
			"https://hub.docker.com/v2/repositories/%s/%s/tags/%s",
			namespace, repo, url.PathEscape(tag),
		)
		resp, err := client.Get(tagURL)
		if err != nil {
			continue
		}
		func() {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return
			}
			var data struct {
				FullSize    int64  `json:"full_size"`
				Digest      string `json:"digest"`
				LastUpdated string `json:"last_updated"`
				Images      []struct {
					Size   int64  `json:"size"`
					Digest string `json:"digest"`
				} `json:"images"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
				return
			}
			ti := TagInfo{
				Size:   data.FullSize,
				Digest: data.Digest,
			}
			if ti.Size == 0 && len(data.Images) > 0 {
				for _, img := range data.Images {
					ti.Size += img.Size
				}
			}
			if ti.Digest == "" && len(data.Images) > 0 {
				ti.Digest = data.Images[0].Digest
			}
			if data.LastUpdated != "" {
				if t, err := time.Parse(time.RFC3339Nano, data.LastUpdated); err == nil {
					ti.LastUpdated = t
				}
			}
			result[tag] = ti
		}()
	}
	return result
}

// ResolveDockerTemplates replaces {docker.*} templates with values from Docker Hub.
// Returns s unchanged if info is nil or no {docker.} templates are present.
func ResolveDockerTemplates(s string, info *DockerHubInfo) string {
	if info == nil || !strings.Contains(s, "{docker.") {
		return s
	}

	s = strings.ReplaceAll(s, "{docker.pulls:raw}", strconv.FormatInt(info.Pulls, 10))
	s = strings.ReplaceAll(s, "{docker.pulls}", formatCount(info.Pulls))
	s = strings.ReplaceAll(s, "{docker.stars}", strconv.Itoa(info.Stars))
	s = strings.ReplaceAll(s, "{docker.size:raw}", strconv.FormatInt(info.Size, 10))
	s = strings.ReplaceAll(s, "{docker.size}", formatBytes(info.Size))
	s = strings.ReplaceAll(s, "{docker.latest}", shortDigest(info.Latest))

	s = resolveTagTemplates(s, info.Tags)

	return s
}

// tagSuffix maps a known placeholder suffix to its formatting function.
type tagSuffix struct {
	pattern string
	format  func(TagInfo) string
}

// knownTagSuffixes lists recognized {docker.tag.TAG.FIELD} suffixes.
// Order matters: longer/more-specific patterns first (size:raw before size).
var knownTagSuffixes = []tagSuffix{
	{".size:raw}", func(ti TagInfo) string { return strconv.FormatInt(ti.Size, 10) }},
	{".size}", func(ti TagInfo) string { return formatBytes(ti.Size) }},
	{".updated}", func(ti TagInfo) string {
		if ti.LastUpdated.IsZero() {
			return ""
		}
		return ti.LastUpdated.Format("2006-01-02")
	}},
	{".digest}", func(ti TagInfo) string { return shortDigest(ti.Digest) }},
}

// resolveTagTemplates replaces {docker.tag.TAG.FIELD} patterns with per-tag values.
// Truly suffix-anchored: finds closing } first, then tests the bounded content
// against known suffixes from the right. Handles dots in tag names (e.g., v0.2.1).
func resolveTagTemplates(s string, tags map[string]TagInfo) string {
	if tags == nil {
		return s
	}

	const prefix = "{docker.tag."

	var out strings.Builder
	for {
		idx := strings.Index(s, prefix)
		if idx == -1 {
			out.WriteString(s)
			break
		}
		out.WriteString(s[:idx])
		rest := s[idx+len(prefix):]

		// Find the closing brace — bounds this single placeholder
		closeIdx := strings.Index(rest, "}")
		if closeIdx == -1 {
			// Unclosed placeholder — write literally and stop
			out.WriteString(prefix)
			out.WriteString(rest)
			break
		}

		// inner is everything between "{docker.tag." and "}" (inclusive of "}")
		inner := rest[:closeIdx+1]

		matched := false
		for _, sf := range knownTagSuffixes {
			if strings.HasSuffix(inner, sf.pattern) {
				tagName := inner[:len(inner)-len(sf.pattern)]
				if tagName == "" {
					continue
				}
				ti, ok := tags[tagName]
				if ok {
					out.WriteString(sf.format(ti))
				}
				// tag not found → empty string (no output)
				matched = true
				break
			}
		}
		if !matched {
			// Known prefix but unrecognized suffix — write literally
			out.WriteString(prefix)
			out.WriteString(inner)
		}
		s = rest[closeIdx+1:]
	}
	return out.String()
}

// ExtractDockerTagNames scans strings for {docker.tag.TAGNAME.FIELD} patterns
// and returns deduplicated tag names. Uses the same suffix-anchored parsing
// as resolveTagTemplates.
func ExtractDockerTagNames(values []string) []string {
	const prefix = "{docker.tag."

	seen := make(map[string]bool)
	for _, v := range values {
		s := v
		for {
			idx := strings.Index(s, prefix)
			if idx == -1 {
				break
			}
			rest := s[idx+len(prefix):]

			closeIdx := strings.Index(rest, "}")
			if closeIdx == -1 {
				break
			}

			inner := rest[:closeIdx+1]
			for _, sf := range knownTagSuffixes {
				if strings.HasSuffix(inner, sf.pattern) {
					tagName := inner[:len(inner)-len(sf.pattern)]
					if tagName != "" {
						seen[tagName] = true
					}
					break
				}
			}
			s = rest[closeIdx+1:]
		}
	}

	tags := make([]string, 0, len(seen))
	for tag := range seen {
		tags = append(tags, tag)
	}
	return tags
}

// formatCount formats a number for human display: 1247 → "1.2k", 1234567 → "1.2M".
func formatCount(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
}

// formatBytes formats bytes for human display: 75890432 → "72.4 MB".
func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// shortDigest returns the first 12 hex characters of a sha256:... digest.
func shortDigest(digest string) string {
	digest = strings.TrimPrefix(digest, "sha256:")
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}
