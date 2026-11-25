package config

import (
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

// FormatEffective renders the effective configuration as TOML. The API key is
// redacted to "[set]" or "[not set]" and can never leak through this method.
func (c *Config) FormatEffective() string {
	if c == nil {
		return ""
	}

	redacted := "[not set]"
	if strings.TrimSpace(c.APIKey) != "" {
		redacted = "[set]"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "api_key = %q\n", redacted)
	if err := toml.NewEncoder(&b).Encode(c); err != nil {
		// Encoding the Config should be infallible; keep the output useful and
		// secret-free even if a future field breaks encoding.
		return fmt.Sprintf("api_key = %q\n", redacted)
	}
	return b.String()
}
