package timing

import (
	"fmt"
	"strings"
)

// headerSets are the column headers from fullest to tersest. A narrower
// terminal gets a terser set; columns are never dropped.
var headerSets = [][]string{
	{"Repo", "Kind", "Jobs", "Wait h", "Med wait m", "P90 wait m", "Run h", "Never-started (n / h)", "Reruns", "Kicks", "Wasted h", "Total h"},
	{"Repo", "Kind", "Jobs", "Wait h", "Med m", "P90 m", "Run h", "Never n/h", "Rerun", "Kick", "Waste h", "Total h"},
	{"Repo", "K", "Jb", "Wt", "Md", "P9", "Rn", "NS n/h", "Rr", "Ki", "Ws", "Tot"},
}

// compactFrom is the first header set that uses one-space gaps and "n/h".
const compactFrom = 2

func tableCells(s RepoStats, compact bool) []string {
	never := fmt.Sprintf("%d / %.1f", s.NeverStarted, s.NeverStartedWaitH)
	if compact {
		never = strings.ReplaceAll(never, " ", "")
	}
	return []string{s.Repo, s.Kind, fmt.Sprint(s.Jobs), fmt.Sprintf("%.1f", s.WaitH),
		fmt.Sprintf("%.1f", s.MedianWaitM), fmt.Sprintf("%.1f", s.P90WaitM), fmt.Sprintf("%.1f", s.RunH),
		never, fmt.Sprint(s.Reruns),
		fmt.Sprint(s.ManualKicks), fmt.Sprintf("%.1f", s.WastedH), fmt.Sprintf("%.1f", s.TotalH)}
}

func runeLen(s string) int { return len([]rune(s)) }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// Table renders the per-repo table, one row per (repo, kind) in the order
// given. width <= 0 means no limit. ANSI (bold header) only when tty.
func Table(stats []RepoStats, width int, tty bool) string {
	var cells [][]string
	var hdr []string
	var w []int
	sep := "  "
	for level, h := range headerSets {
		hdr, sep = h, "  "
		if level >= compactFrom {
			sep = " "
		}
		cells = make([][]string, len(stats))
		for i, s := range stats {
			cells[i] = tableCells(s, level >= compactFrom)
		}
		w = make([]int, len(h))
		for c := range h {
			w[c] = runeLen(h[c])
			for _, r := range cells {
				if n := runeLen(r[c]); n > w[c] {
					w[c] = n
				}
			}
		}
		if width <= 0 || tableWidth(w, sep) <= width {
			break
		}
	}
	// Still too wide: shorten repo names rather than drop a column.
	if over := tableWidth(w, sep) - width; width > 0 && over > 0 {
		if w[0] -= over; w[0] < 4 {
			w[0] = 4
		}
	}
	line := func(row []string, bold bool) string {
		var b strings.Builder
		for c, cell := range row {
			if c > 0 {
				b.WriteString(sep)
			}
			cell = truncate(cell, w[c])
			pad := strings.Repeat(" ", w[c]-runeLen(cell))
			if c < 2 {
				b.WriteString(cell + pad)
			} else {
				b.WriteString(pad + cell)
			}
		}
		s := strings.TrimRight(b.String(), " ")
		if bold && tty {
			s = "\x1b[1m" + s + "\x1b[0m"
		}
		return s + "\n"
	}
	var out strings.Builder
	out.WriteString(line(hdr, true))
	for _, r := range cells {
		out.WriteString(line(r, false))
	}
	return out.String()
}

func tableWidth(w []int, sep string) int {
	t := len(sep) * (len(w) - 1)
	for _, n := range w {
		t += n
	}
	return t
}
