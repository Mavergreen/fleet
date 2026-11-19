package timing

import (
	"strings"
	"testing"
)

func TestGanttFitsWidth(t *testing.T) {
	jobs := []Job{
		mac("a-very-long-job-name-that-keeps-going-and-going-past-any-sane-column", at(0), at(30), at(90)),
		mac("short", at(0), at(1), at(2)),
	}
	jobs[1].Attempt = 2
	for _, w := range []int{60, 80, 0, 120} {
		lines := Gantt(jobs, w)
		limit := w
		if w <= 0 {
			limit = 80
		}
		if len(lines) != 3 {
			t.Fatalf("want 2 jobs + footer, got %d lines", len(lines))
		}
		for _, l := range lines {
			if n := len([]rune(l)); n > limit {
				t.Errorf("width %d: %d runes: %q", w, n, l)
			}
		}
	}
	if l := Gantt(jobs, 60)[1]; !strings.Contains(l, "…") {
		t.Errorf("long name not truncated with an ellipsis: %q", l)
	}
	if l := Gantt(jobs, 120)[0]; !strings.Contains(l, "(attempt 2)") {
		t.Errorf("attempt label missing: %q", l)
	}
}

func TestGanttShowsWaitThenRun(t *testing.T) {
	lines := Gantt([]Job{mac("build", at(0), at(30), at(60))}, 60)
	l := lines[0]
	w, r := strings.Index(l, "░"), strings.Index(l, "█")
	if w < 0 || r < 0 || w >= r || strings.LastIndex(l, "░") > r {
		t.Fatalf("wait must precede run: %q", l)
	}
	if !strings.Contains(lines[len(lines)-1], "1h0m0s") {
		t.Errorf("footer lacks the axis duration: %q", lines[len(lines)-1])
	}
}

func TestCriticalPath(t *testing.T) {
	jobs := []Job{
		mac("test", at(10), at(11), at(30)),
		mac("build", at(0), at(1), at(10)),
		mac("package", at(30), at(31), at(40)),
		mac("lint", at(0), at(1), at(5)),
	}
	got := CriticalPath(jobs)
	if strings.Join(got, ",") != "build,test,package" {
		t.Fatalf("%v", got)
	}
	lines := Gantt(jobs, 80)
	for _, l := range lines[:4] {
		star := strings.HasPrefix(l, "*")
		on := strings.Contains(l, "build") || strings.Contains(l, "test") || strings.Contains(l, "package")
		if star != on {
			t.Errorf("marker wrong: %q", l)
		}
	}
}
