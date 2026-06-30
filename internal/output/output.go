package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github-gitea-sync/internal/reconcile"
)

type Mode string

const (
	ModeText Mode = "text"
	ModeJSON Mode = "json"
)

func Write(w io.Writer, mode Mode, report reconcile.Report) error {
	if mode == ModeJSON {
		encoder := json.NewEncoder(w)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(report)
	}
	return writeText(w, report)
}

func writeText(w io.Writer, report reconcile.Report) error {
	if _, err := fmt.Fprintf(w, "command: %s\n", report.Command); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "started_at: %s\n", report.StartedAt.Format("2006-01-02T15:04:05Z07:00")); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "finished_at: %s\n", report.FinishedAt.Format("2006-01-02T15:04:05Z07:00")); err != nil {
		return err
	}
	if report.GiteaBaseURL != "" {
		if _, err := fmt.Fprintf(w, "gitea_base_url: %s\n", report.GiteaBaseURL); err != nil {
			return err
		}
	}
	summary := report.Summary
	if _, err := fmt.Fprintf(
		w,
		"summary: total=%d ok=%d actions=%d created=%d skipped=%d warnings=%d blocked=%d errors=%d\n",
		summary.Total,
		summary.OK,
		summary.Actions,
		summary.Created,
		summary.Skipped,
		summary.Warnings,
		summary.Blocked,
		summary.Errors,
	); err != nil {
		return err
	}
	if len(summary.ByStatus) > 0 {
		var parts []string
		for _, status := range reconcile.SortStatusKeys(summary.ByStatus) {
			parts = append(parts, fmt.Sprintf("%s=%d", status, summary.ByStatus[status]))
		}
		if _, err := fmt.Fprintf(w, "statuses: %s\n", strings.Join(parts, " ")); err != nil {
			return err
		}
	}
	if len(report.Results) == 0 {
		_, err := fmt.Fprintln(w, "results: none")
		return err
	}
	if _, err := fmt.Fprintln(w, "results:"); err != nil {
		return err
	}
	for _, entry := range report.Results {
		if _, err := fmt.Fprintf(w, "- %s", entry.Status); err != nil {
			return err
		}
		if entry.Owner != "" || entry.Repository != "" {
			target := entry.Owner
			if entry.Repository != "" {
				target = strings.Trim(target+"/"+entry.Repository, "/")
			}
			if _, err := fmt.Fprintf(w, " %s", target); err != nil {
				return err
			}
		}
		if entry.Message != "" {
			if _, err := fmt.Fprintf(w, ": %s", entry.Message); err != nil {
				return err
			}
		}
		if entry.Error != "" {
			if _, err := fmt.Fprintf(w, " (%s)", entry.Error); err != nil {
				return err
			}
		}
		if entry.ExpectedSource != "" && entry.ObservedSource != "" && entry.ExpectedSource != entry.ObservedSource {
			if _, err := fmt.Fprintf(w, " expected=%s observed=%s", entry.ExpectedSource, entry.ObservedSource); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	return nil
}
