package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/tern-agent/tern/internal/config"
)

type providerStatusRow struct {
	Name         string `json:"name"`
	DisplayName  string `json:"display_name"`
	BaseURL      string `json:"base_url"`
	EnvVar       string `json:"env_var,omitempty"`
	DefaultModel string `json:"default_model"`
	KeyStatus    string `json:"key_status"`
	DataNotice   string `json:"data_notice,omitempty"`
}

func runProvidersCommand(args []string, o runOptions) int {
	fs := flag.NewFlagSet("tern providers", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonOut, help bool
	fs.BoolVar(&jsonOut, "json", false, "emit JSON")
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(o.Out, "Usage: tern providers [--json]")
			return 0
		}
		fmt.Fprintf(o.Err, "tern providers: %v\n", err)
		return 2
	}
	if help {
		fmt.Fprintln(o.Out, "Usage: tern providers [--json]")
		return 0
	}

	configDir, _ := config.ConfigDir()
	presets := config.BuiltinProviders()
	rows := make([]providerStatusRow, 0, len(presets))
	for _, p := range presets {
		status := "[not set]"
		switch {
		case !p.RequiresKey:
			status = "[local]"
		case configDir != "":
			if key, err := config.LoadAPIKeyForProvider(configDir, p.Name); err == nil && strings.TrimSpace(key) != "" {
				status = "[set]"
			}
		}
		rows = append(rows, providerStatusRow{
			Name:         p.Name,
			DisplayName:  p.DisplayName,
			BaseURL:      p.BaseURL,
			EnvVar:       p.EnvVar,
			DefaultModel: p.DefaultModel,
			KeyStatus:    status,
			DataNotice:   p.DataNotice,
		})
	}

	if jsonOut {
		enc := json.NewEncoder(o.Out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rows)
		return 0
	}

	tw := tabwriter.NewWriter(o.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tKEY\tENV VAR\tDEFAULT MODEL\tBASE URL")
	for _, r := range rows {
		env := r.EnvVar
		if env == "" {
			env = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.KeyStatus, env, r.DefaultModel, r.BaseURL)
	}
	_ = tw.Flush()
	fmt.Fprintln(o.Out, "\nNote: Free tiers (Google Gemini AI Studio, OpenRouter ':free' models) may log prompts/outputs for provider training — do not send secrets or private code.")
	fmt.Fprintln(o.Out, "Check remaining balance: tern balance   |   Switch provider: tern --provider <name>")
	return 0
}
