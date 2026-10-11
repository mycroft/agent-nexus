package web

import (
	"fmt"
	"html/template"
	"strconv"
	"strings"
	"time"

	"github.com/mycroft/agent-nexus/internal/jobs"
)

// Class names below are picked up by Tailwind (see styles/app.css @source).

var statusClasses = map[jobs.Status]string{
	jobs.Queued:     "bg-slate-100 text-slate-700 ring-slate-500/20 dark:bg-slate-500/10 dark:text-slate-300 dark:ring-slate-400/20",
	jobs.Running:    "bg-amber-50 text-amber-700 ring-amber-600/20 dark:bg-amber-500/10 dark:text-amber-400 dark:ring-amber-500/20",
	jobs.Posting:    "bg-sky-50 text-sky-700 ring-sky-600/20 dark:bg-sky-500/10 dark:text-sky-400 dark:ring-sky-500/20",
	jobs.Succeeded:  "bg-emerald-50 text-emerald-700 ring-emerald-600/20 dark:bg-emerald-500/10 dark:text-emerald-400 dark:ring-emerald-500/20",
	jobs.Failed:     "bg-red-50 text-red-700 ring-red-600/20 dark:bg-red-500/10 dark:text-red-400 dark:ring-red-500/20",
	jobs.Skipped:    "bg-zinc-50 text-zinc-600 ring-zinc-500/20 dark:bg-zinc-500/10 dark:text-zinc-400 dark:ring-zinc-400/20",
	jobs.Superseded: "bg-zinc-50 text-zinc-600 ring-zinc-500/20 dark:bg-zinc-500/10 dark:text-zinc-400 dark:ring-zinc-400/20",
}

var dotClasses = map[jobs.Status]string{
	jobs.Queued:     "bg-slate-400",
	jobs.Running:    "bg-amber-500",
	jobs.Posting:    "bg-sky-500",
	jobs.Succeeded:  "bg-emerald-500",
	jobs.Failed:     "bg-red-500",
	jobs.Skipped:    "bg-zinc-400",
	jobs.Superseded: "bg-zinc-400",
}

var roleClasses = map[string]string{
	"system":    "border-slate-200 dark:border-slate-800",
	"user":      "border-indigo-200 dark:border-indigo-500/30",
	"assistant": "border-violet-200 dark:border-violet-500/30",
	"tool":      "border-slate-200 dark:border-slate-800",
}

var roleBadges = map[string]string{
	"system":    "bg-slate-100 text-slate-600 dark:bg-slate-800 dark:text-slate-300",
	"user":      "bg-indigo-50 text-indigo-700 dark:bg-indigo-500/10 dark:text-indigo-300",
	"assistant": "bg-violet-50 text-violet-700 dark:bg-violet-500/10 dark:text-violet-300",
	"tool":      "bg-slate-100 font-mono text-slate-700 dark:bg-slate-800 dark:text-slate-300",
}

var kindLabels = map[string]string{
	"pr_review":     "Review",
	"pr_discussion": "Discussion",
}

var funcs = template.FuncMap{
	"statusClass": func(s jobs.Status) string { return statusClasses[s] },
	"dotClass":    func(s jobs.Status) string { return dotClasses[s] },
	"roleClass":   func(r string) string { return roleClasses[r] },
	"roleBadge":   func(r string) string { return roleBadges[r] },
	"kindLabel": func(k string) string {
		if l, ok := kindLabels[k]; ok {
			return l
		}
		return k
	},
	"ago":      ago,
	"duration": duration,
	"tokens":   tokens,
	"short": func(sha string) string {
		if len(sha) > 7 {
			return sha[:7]
		}
		return sha
	},
	"timestamp": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") },
	"lines":     countLines,
	"displayVersion": func(v string) string {
		if v == "" || v == "dev" {
			return "dev"
		}
		return "v" + strings.TrimPrefix(v, "v")
	},
}

// countLines says how many lines s has: "1 line", "12 lines".
func countLines(s string) string {
	n := strings.Count(s, "\n") + 1
	if n == 1 {
		return "1 line"
	}
	return strconv.Itoa(n) + " lines"
}

// ago formats the time since t: "12s", "5m", "3h", "2d".
func ago(t time.Time) string {
	s := int(time.Since(t).Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", max(s, 0))
	case s < 3600:
		return fmt.Sprintf("%dm", s/60)
	case s < 48*3600:
		return fmt.Sprintf("%dh", s/3600)
	default:
		return fmt.Sprintf("%dd", s/86400)
	}
}

// duration formats how long a job ran, or has been running: "45s", "3m 05s".
func duration(start, end *time.Time) string {
	if start == nil {
		return ""
	}
	stop := time.Now()
	if end != nil {
		stop = *end
	}
	d := stop.Sub(*start).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}

// tokens formats a token count: 950, 12.3k, 1.58M.
func tokens(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
	}
}
