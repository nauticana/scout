package scout

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	lookupBlock  = regexp.MustCompile(`- table: foreign_key_lookup\n\s+columns:.*\n\s+rows:\n((?:\s+- \[.*\]\n?)+)`)
	foreignKeyAt = regexp.MustCompile(`- name: (\w+)\n\s+columns: \[[^\]]*\]\n\s+references:\n\s+table: (\w+)`)
	columnName   = regexp.MustCompile(`(?m)^\s+- name: (\w+)\n\s+type:`)
)

// Every foreign_key_lookup row names a declared foreign key, and a named display column
// exists on the table that key references.
func TestForeignKeyLookupsNameDeclaredKeysAndColumns(t *testing.T) {
	references, columns := map[string]string{}, map[string]map[string]bool{}
	schemas, err := filepath.Glob("schema/*/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range schemas {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		name := tableName.FindStringSubmatch(string(content))
		if name == nil {
			continue
		}
		columns[name[1]] = map[string]bool{}
		for _, column := range columnName.FindAllStringSubmatch(string(content), -1) {
			columns[name[1]][column[1]] = true
		}
		for _, key := range foreignKeyAt.FindAllStringSubmatch(string(content), -1) {
			references[key[1]] = key[2]
		}
	}
	seeds, err := filepath.Glob("schema/seed/*/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, path := range seeds {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, block := range lookupBlock.FindAllStringSubmatch(string(content), -1) {
			for _, row := range seedRow.FindAllStringSubmatch(block[1], -1) {
				fields := strings.Split(row[1], ",")
				constraint, display := strings.TrimSpace(fields[0]), strings.Trim(strings.TrimSpace(fields[2]), `"`)
				checked++
				target, ok := references[constraint]
				if !ok {
					t.Errorf("%s: foreign_key_lookup %s names no declared foreign key", path, constraint)
					continue
				}
				if display != "*" && !columns[target][display] {
					t.Errorf("%s: foreign_key_lookup %s displays %s, which %s does not have", path, constraint, display, target)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no foreign_key_lookup rows found")
	}
}
