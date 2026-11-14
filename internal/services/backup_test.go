package services

import (
	"reflect"
	"testing"
	"time"
)

func name(t time.Time) string { return t.UTC().Format(backupTimeFormat) + ".sql.gz" }

func TestSelectBackupsKeepsDailyAndWeekly(t *testing.T) {
	// Two backups a day for 60 days, ending on Wed 2026-11-11.
	end := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	var names []string
	for d := 0; d < 60; d++ {
		day := end.AddDate(0, 0, -d)
		names = append(names, name(day), name(day.Add(-6*time.Hour)))
	}
	names = append(names, "notes.txt", "x.sql.gz") // not ours to judge: ignored
	keep, drop := SelectBackups(names)

	days := map[string]int{}
	for _, k := range keep {
		tm, ok := parseBackupTime(k)
		if !ok {
			t.Fatalf("kept a non-backup %q", k)
		}
		days[tm.Format("2006-01-02")]++
	}
	// The last 7 days are covered, one backup each (the newest of the day).
	for d := 0; d < 7; d++ {
		day := end.AddDate(0, 0, -d).Format("2006-01-02")
		if days[day] != 1 {
			t.Errorf("day %s: kept %d, want 1", day, days[day])
		}
	}
	// Weekly ones add at most 4 more, and nothing from before 4 weeks back survives.
	if len(keep) < 7 || len(keep) > 11 {
		t.Errorf("kept %d backups, want between 7 and 11", len(keep))
	}
	oldest := end.AddDate(0, 0, -29)
	for _, k := range keep {
		if tm, _ := parseBackupTime(k); tm.Before(oldest) {
			t.Errorf("kept %s, older than 4 weeks", k)
		}
	}
	for _, d := range drop {
		if _, ok := parseBackupTime(d); !ok {
			t.Errorf("would delete %q, which is not a backup", d)
		}
	}
	if len(keep)+len(drop) != 120 {
		t.Errorf("keep+drop = %d, want 120", len(keep)+len(drop))
	}
}

func TestSelectBackupsFewFiles(t *testing.T) {
	a := name(time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC))
	keep, drop := SelectBackups([]string{a})
	if !reflect.DeepEqual(keep, []string{a}) || len(drop) != 0 {
		t.Errorf("keep=%v drop=%v", keep, drop)
	}
}

func TestIdent(t *testing.T) {
	if got := Ident("My-Shop.2"); got != "app_my_shop_2" {
		t.Errorf("Ident = %q", got)
	}
}
