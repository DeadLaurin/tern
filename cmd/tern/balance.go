package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/tern-agent/tern/internal/config"
	"github.com/tern-agent/tern/internal/provider"
)

func runBalanceCommand(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("tern balance", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var asJSON bool
	var provFlag string
	fs.BoolVar(&asJSON, "json", false, "output JSON")
	fs.StringVar(&provFlag, "provider", "", "provider name to check")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(out, "Usage: tern balance [--provider <name>] [--json]")
			return 0
		}
		fmt.Fprintf(errOut, "tern balance: %v\n", err)
		return 2
	}

	workDir, _ := os.Getwd()
	flags := &config.FlagOverrides{}
	if strings.TrimSpace(provFlag) != "" {
		p := strings.TrimSpace(provFlag)
		flags.Provider = &p
	}
	cfg, err := config.Load(config.LoadOptions{WorkDir: workDir, Flags: flags})
	if err != nil {
		fmt.Fprintf(errOut, "tern balance: %v\n", err)
		return 1
	}

	client := newProviderClient(cfg)
	checker, ok := client.(provider.BalanceChecker)
	if !ok {
		fmt.Fprintf(errOut, "tern balance: balance checking is not supported for %q\n", cfg.Provider)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	bal, err := checker.GetBalance(ctx)
	if err != nil {
		if errors.Is(err, provider.ErrBalanceUnsupported) {
			if asJSON {
				_ = json.NewEncoder(out).Encode(map[string]any{
					"provider":  cfg.Provider,
					"supported": false,
					"note":      "This provider does not expose a balance endpoint.",
				})
				return 0
			}
			fmt.Fprintf(out, "Provider %s does not expose a balance endpoint.\n", cfg.Provider)
			return 0
		}
		fmt.Fprintf(errOut, "tern balance: %v\n", err)
		return 1
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(bal)
		return 0
	}

	fmt.Fprintf(out, "Provider:  %s\n", bal.Provider)
	fmt.Fprintf(out, "Balance:   %s\n", bal.FormatCompact())
	if len(bal.Balances) > 0 {
		for _, cb := range bal.Balances {
			fmt.Fprintf(out, "  %-4s     %.4f (granted %.4f · topped up %.4f)\n", cb.Currency, cb.Total, cb.Granted, cb.ToppedUp)
		}
	} else {
		if bal.Total > 0 {
			fmt.Fprintf(out, "Total:     %.4f %s\n", bal.Total, bal.Currency)
		}
		if bal.Used > 0 {
			fmt.Fprintf(out, "Used:      %.4f %s\n", bal.Used, bal.Currency)
		}
		if bal.Granted > 0 || bal.ToppedUp > 0 {
			fmt.Fprintf(out, "Breakdown: granted %.4f · topped up %.4f %s\n", bal.Granted, bal.ToppedUp, bal.Currency)
		}
	}
	if bal.Note != "" {
		fmt.Fprintf(out, "Note:      %s\n", bal.Note)
	}
	return 0
}
