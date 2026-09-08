package migrations

import (
	"strings"
	"testing"
)

func TestReportItemSearchColumnsMigrationExists(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	var body string
	for _, migration := range all {
		if migration.Version == 16 {
			body = migration.SQL
		}
	}
	if body == "" {
		t.Fatal("migration 16 is missing")
	}
	got := make([]string, 0, 5)
	for _, statement := range strings.Split(body, ";") {
		statement = strings.TrimSpace(statement)
		if statement != "" {
			got = append(got, strings.ToUpper(strings.Join(strings.Fields(statement), " ")))
		}
	}
	want := []string{
		"ALTER TABLE PUBLIC.REPORT_ITEMS ADD COLUMN SOURCE_KIND TEXT",
		"ALTER TABLE PUBLIC.REPORT_ITEMS ADD COLUMN POSTED_AT TIMESTAMPTZ",
		"ALTER TABLE PUBLIC.REPORT_ITEMS ADD COLUMN COLLECTED_AT TIMESTAMPTZ",
		"ALTER TABLE PUBLIC.REPORT_ITEMS ADD COLUMN RECORDED_AT TIMESTAMPTZ",
		"GRANT SELECT, INSERT ON TABLE PUBLIC.REPORT_ITEMS TO NAMO_RUNTIME",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("migration 16 statements = %v, want %v", got, want)
	}
}
