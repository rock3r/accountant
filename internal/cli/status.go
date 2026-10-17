package cli

import (
	"fmt"
	"io"
	"sort"
	"time"

	"accountant/internal/config"
	"accountant/internal/router"
	"accountant/internal/state"
)

func cmdStatus(paths config.Paths, stdout, stderr io.Writer) int {
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	st, err := state.Load(paths.StateFile)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	now := time.Now()
	if len(cfg.Accounts) == 0 {
		fmt.Fprintln(stdout, "No accounts configured.")
		return 0
	}
	sorted := append([]config.Account(nil), cfg.Accounts...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Priority != sorted[j].Priority {
			return sorted[i].Priority < sorted[j].Priority
		}
		return sorted[i].ID < sorted[j].ID
	})
	for _, a := range sorted {
		as := st.Accounts[a.ID]
		fmt.Fprintf(stdout, "%s (%s, priority %d%s)\n", a.ID, a.Label, a.Priority, enabledSuffix(a))
		printWindow(stdout, "5h", windowOf(as, true), now, cfg.UtilizationScale)
		printWindow(stdout, "7d", windowOf(as, false), now, cfg.UtilizationScale)
		switch {
		case as == nil:
			fmt.Fprintln(stdout, "  state:     no traffic recorded yet")
		case as.AuthBroken:
			fmt.Fprintln(stdout, "  state:     AUTH-BROKEN (run: accountant account test "+a.ID+")")
		case as.CoolingUntil.After(now):
			fmt.Fprintf(stdout, "  state:     cooling until %s\n", as.CoolingUntil.Local().Format("2006-01-02 15:04:05"))
		default:
			fmt.Fprintln(stdout, "  state:     eligible")
		}
		pins := 0
		for _, p := range st.Pins {
			if p.AccountID == a.ID {
				pins++
			}
		}
		fmt.Fprintf(stdout, "  sessions:  %d pinned\n", pins)
		if minted, ok := a.MintedTime(); ok {
			expires := minted.AddDate(1, 0, 0)
			if now.After(expires.AddDate(0, 0, -30)) {
				fmt.Fprintf(stdout, "  WARNING:   token expires ~%s; mint a new one with `claude setup-token` and re-add\n",
					expires.Format("2006-01-02"))
			}
		}
	}
	return 0
}

func enabledSuffix(a config.Account) string {
	if a.IsEnabled() {
		return ""
	}
	return ", disabled"
}

func windowOf(as *state.AccountState, fiveHour bool) *state.Window {
	if as == nil {
		return nil
	}
	if fiveHour {
		return &as.FiveHour
	}
	return &as.SevenDay
}

func printWindow(stdout io.Writer, name string, w *state.Window, now time.Time, scale string) {
	if w == nil || !w.HasSample {
		fmt.Fprintf(stdout, "  %-10s unknown\n", name+":")
		return
	}
	util := router.NormalizeUtilization(w.Utilization, scale)
	line := fmt.Sprintf("  %-10s %.1f%% used", name+":", util*100)
	if !w.ResetAt.IsZero() {
		line += fmt.Sprintf(", resets %s", w.ResetAt.Local().Format("2006-01-02 15:04"))
		if !w.ResetAt.After(now) {
			line += " (passed)"
		}
	}
	fmt.Fprintln(stdout, line)
}
