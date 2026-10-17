package router

import (
	"io"
	"log/slog"
	"strconv"
	"time"

	"accountant/internal/provider"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func itoa(i int) string { return strconv.Itoa(i) }

func snapNone() provider.QuotaSnapshot { return provider.QuotaSnapshot{} }

func window(util float64, reset time.Time) provider.WindowSample {
	return provider.WindowSample{
		Utilization:    util,
		HasUtilization: true,
		ResetAt:        reset,
		HasReset:       !reset.IsZero(),
	}
}

func snapWindows(fiveHour, sevenDay provider.WindowSample) provider.QuotaSnapshot {
	return provider.QuotaSnapshot{FiveHour: fiveHour, SevenDay: sevenDay, Status: "x"}
}

func verdictOK() provider.Verdict    { return provider.VerdictOK }
func verdictQuota() provider.Verdict { return provider.VerdictQuotaExhausted }
func verdictAuth() provider.Verdict  { return provider.VerdictAuthBroken }
