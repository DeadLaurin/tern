package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const cfDeployTimeout = 120 * time.Second

// CloudflareDeploy describes the latest Cloudflare Pages/Workers deployment.
type CloudflareDeploy struct {
	ID          string `json:"id,omitempty"`
	URL         string `json:"url,omitempty"`
	Status      string `json:"status,omitempty"`
	Environment string `json:"environment,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// CloudflareStatus reports detected Cloudflare Pages/Workers configuration.
type CloudflareStatus struct {
	Detected          bool              `json:"detected"`
	Kind              string            `json:"kind,omitempty"` // "pages" or "workers"
	ProjectName       string            `json:"project_name,omitempty"`
	ConfigFile        string            `json:"config_file,omitempty"`
	PagesDir          string            `json:"pages_dir,omitempty"`
	WranglerAvailable bool              `json:"wrangler_available"`
	LatestDeploy      *CloudflareDeploy `json:"latest_deploy,omitempty"`
	Error             string            `json:"error,omitempty"`
}

var (
	tomlNameRe     = regexp.MustCompile(`(?m)^\s*name\s*=\s*"([^"]+)"`)
	tomlPagesDirRe = regexp.MustCompile(`(?m)^\s*pages_build_output_dir\s*=\s*"([^"]+)"`)
)

// DetectCloudflare inspects workDir for wrangler.toml / wrangler.json /
// wrangler.jsonc (or Cloudflare Pages configuration) and optional latest
// deployment status.
func DetectCloudflare(ctx context.Context, workDir string) CloudflareStatus {
	st := CloudflareStatus{}
	if strings.TrimSpace(workDir) == "" {
		return st
	}
	_, wErr := exec.LookPath("wrangler")
	st.WranglerAvailable = wErr == nil

	candidates := []string{"wrangler.toml", "wrangler.json", "wrangler.jsonc"}
	for _, name := range candidates {
		full := filepath.Join(workDir, name)
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		st.Detected = true
		st.ConfigFile = name
		if strings.HasSuffix(name, ".toml") {
			if m := tomlNameRe.FindSubmatch(data); len(m) == 2 {
				st.ProjectName = string(m[1])
			}
			if m := tomlPagesDirRe.FindSubmatch(data); len(m) == 2 {
				st.PagesDir = string(m[1])
			}
		} else {
			cleaned := stripJSONComments(string(data))
			var cfg struct {
				Name                string `json:"name"`
				PagesBuildOutputDir string `json:"pages_build_output_dir"`
			}
			if json.Unmarshal([]byte(cleaned), &cfg) == nil {
				st.ProjectName = strings.TrimSpace(cfg.Name)
				st.PagesDir = strings.TrimSpace(cfg.PagesBuildOutputDir)
			}
		}
		if st.PagesDir != "" {
			st.Kind = "pages"
		} else {
			st.Kind = "workers"
		}
		break
	}

	if !st.Detected {
		return st
	}

	// If CLOUDFLARE_API_TOKEN & CLOUDFLARE_ACCOUNT_ID are set and we have a
	// Pages project name, fetch the latest deployment via Cloudflare REST API.
	token := strings.TrimSpace(os.Getenv("CLOUDFLARE_API_TOKEN"))
	accountID := strings.TrimSpace(os.Getenv("CLOUDFLARE_ACCOUNT_ID"))
	if token != "" && accountID != "" && st.ProjectName != "" && st.Kind == "pages" {
		if dep, err := fetchLatestPagesDeployment(ctx, accountID, token, st.ProjectName); err == nil && dep != nil {
			st.LatestDeploy = dep
		}
	}
	return st
}

func fetchLatestPagesDeployment(ctx context.Context, accountID, token, project string) (*CloudflareDeploy, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/pages/projects/%s/deployments?per_page=1", accountID, project)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cloudflare api %d", resp.StatusCode)
	}
	var payload struct {
		Result []struct {
			ID          string `json:"id"`
			URL         string `json:"url"`
			Environment string `json:"environment"`
			CreatedOn   string `json:"created_on"`
			LatestStage struct {
				Status string `json:"status"`
			} `json:"latest_stage"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || len(payload.Result) == 0 {
		return nil, err
	}
	d := payload.Result[0]
	return &CloudflareDeploy{
		ID:          d.ID,
		URL:         d.URL,
		Status:      d.LatestStage.Status,
		Environment: d.Environment,
		CreatedAt:   d.CreatedOn,
	}, nil
}

// DeployCloudflare deploys the detected Cloudflare Pages or Workers project
// using `wrangler` (or `npx wrangler`).
func DeployCloudflare(ctx context.Context, workDir string) (string, error) {
	st := DetectCloudflare(ctx, workDir)
	if !st.Detected {
		return "", errors.New("no wrangler.toml, wrangler.json, or wrangler.jsonc found in workspace")
	}
	bin := "wrangler"
	var args []string
	if !st.WranglerAvailable {
		if _, err := exec.LookPath("npx"); err != nil {
			return "", errors.New("wrangler CLI not found (install with: npm i -g wrangler)")
		}
		bin = "npx"
		args = append(args, "--yes", "wrangler")
	}
	if st.Kind == "pages" && st.PagesDir != "" {
		args = append(args, "pages", "deploy", st.PagesDir)
		if st.ProjectName != "" {
			args = append(args, "--project-name", st.ProjectName)
		}
	} else {
		args = append(args, "deploy")
	}
	out, err := runCmd(ctx, cfDeployTimeout, workDir, bin, args...)
	if err != nil {
		return out, err
	}
	if out == "" {
		return "Cloudflare deployment succeeded.", nil
	}
	return out, nil
}

// FormatCloudflareSummary builds a compact human-readable report for /cf.
func FormatCloudflareSummary(st CloudflareStatus) string {
	if !st.Detected {
		return "No Cloudflare project detected (add wrangler.toml, wrangler.json, or wrangler.jsonc)."
	}
	var b strings.Builder
	name := st.ProjectName
	if name == "" {
		name = "(unnamed)"
	}
	fmt.Fprintf(&b, "Cloudflare %s: %s (%s)", st.Kind, name, st.ConfigFile)
	if st.PagesDir != "" {
		fmt.Fprintf(&b, "\nOutput dir: %s", st.PagesDir)
	}
	if st.WranglerAvailable {
		b.WriteString("\nCLI: wrangler available")
	} else {
		b.WriteString("\nCLI: wrangler not on PATH (will try npx wrangler)")
	}
	if st.LatestDeploy != nil {
		fmt.Fprintf(&b, "\nLatest deploy: %s (%s)", st.LatestDeploy.Status, st.LatestDeploy.URL)
	}
	return b.String()
}

// stripJSONComments strips // and /* */ comments from JSONC while keeping
// strings intact.
func stripJSONComments(src string) string {
	var out strings.Builder
	inString := false
	escaped := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inString {
			out.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(src) {
			if src[i+1] == '/' {
				for i < len(src) && src[i] != '\n' {
					i++
				}
				if i < len(src) {
					out.WriteByte('\n')
				}
				continue
			}
			if src[i+1] == '*' {
				i += 2
				for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
					i++
				}
				i++
				continue
			}
		}
		out.WriteByte(c)
	}
	return out.String()
}
