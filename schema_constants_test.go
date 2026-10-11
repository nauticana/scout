package scout

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var (
	seedBlock    = regexp.MustCompile(`- table: (constant_value|constant_lookup)\n\s+columns:.*\n\s+rows:\n((?:\s+- \[.*\]\n?)+)`)
	seedRow      = regexp.MustCompile(`- \[(.*)\]`)
	tableName    = regexp.MustCompile(`(?m)^table: (\w+)`)
	checkLine    = regexp.MustCompile(`(?m)^\s+expression: (.*)$`)
	quotedValue  = regexp.MustCompile(`'([^']*)'`)
	columnTypeAt = `(?m)^\s+- name: %s\n\s+type: (\w+)(?:\((\d+)\))?`
)

// Every column a constant_lookup row maps to has a CHECK listing exactly the codes of its
// constant_value domain, and a CHAR column holds every code.
func TestConstantLookupColumnsCheckTheirCodes(t *testing.T) {
	values, lookups := map[string][]string{}, [][3]string{}
	seeds, err := filepath.Glob("schema/seed/*/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range seeds {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, block := range seedBlock.FindAllStringSubmatch(string(content), -1) {
			for _, row := range seedRow.FindAllStringSubmatch(block[2], -1) {
				fields := strings.SplitN(row[1], ",", 4)
				for i := range fields {
					fields[i] = strings.Trim(strings.TrimSpace(fields[i]), `"`)
				}
				if block[1] == "constant_value" {
					values[fields[0]] = append(values[fields[0]], fields[1])
				} else {
					lookups = append(lookups, [3]string{fields[0], fields[1], fields[2]})
				}
			}
		}
	}
	tables := map[string]string{}
	schemas, err := filepath.Glob("schema/*/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range schemas {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if name := tableName.FindStringSubmatch(string(content)); name != nil {
			tables[name[1]] = string(content)
		}
	}
	if len(lookups) == 0 {
		t.Fatal("no constant_lookup rows found")
	}
	for _, lookup := range lookups {
		domain, table, column := lookup[0], lookup[1], lookup[2]
		codes := slices.Sorted(slices.Values(values[domain]))
		schema, ok := tables[table]
		if !ok || len(codes) == 0 {
			t.Errorf("%s.%s [%s]: table or constant values not found", table, column, domain)
			continue
		}
		in := regexp.MustCompile(`\b` + column + ` IN \(([^)]*)\)`)
		matched := false
		for _, check := range checkLine.FindAllStringSubmatch(schema, -1) {
			for _, list := range in.FindAllStringSubmatch(check[1], -1) {
				var listed []string
				for _, value := range quotedValue.FindAllStringSubmatch(list[1], -1) {
					listed = append(listed, value[1])
				}
				matched = matched || slices.Equal(slices.Sorted(slices.Values(listed)), codes)
			}
		}
		if !matched {
			t.Errorf("%s.%s has no CHECK listing exactly the %s codes %v", table, column, domain, codes)
		}
		if typed := regexp.MustCompile(strings.Replace(columnTypeAt, "%s", column, 1)).FindStringSubmatch(schema); typed != nil && typed[1] == "CHAR" {
			width, _ := strconv.Atoi(typed[2])
			for _, code := range codes {
				if len(code) > width {
					t.Errorf("%s.%s is CHAR(%d) but %s code %q is longer", table, column, width, domain, code)
				}
			}
		}
	}
}
