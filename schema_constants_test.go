package scout

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	keelschema "github.com/nauticana/keel/schema"
)

var (
	seedTable    = regexp.MustCompile(`- table: (\w+)\n\s+columns: \[(.*)\]\n\s+rows:\n((?:\s+- \[.*\]\n?)+)`)
	seedRow      = regexp.MustCompile(`- \[(.*)\]`)
	tableName    = regexp.MustCompile(`(?m)^table: (\w+)`)
	columnTypeAt = `(?m)^\s+- name: %s\n\s+type: (\w+)(?:\((\d+)\))?`
)

// keelGroups are the keel schema groups Scout's DDL is generated with.
var keelGroups = []string{"core", "geo", "tenant_management", "outbox", "approval", "action_token"}

func keelSchemaDir(t *testing.T) string {
	t.Helper()
	dir, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/nauticana/keel").Output()
	if err != nil || strings.TrimSpace(string(dir)) == "" {
		t.Fatalf("locate the pinned keel module: %v", err)
	}
	return filepath.Join(strings.TrimSpace(string(dir)), "schema")
}

// keel's seed validation, which schemagen -seed also runs, holds for Scout's schema and seeds:
// seeded foreign keys resolve and every constant_lookup column has a CHECK "column IN (...)"
// listing exactly its constant_value codes.
func TestSeedsPassKeelValidation(t *testing.T) {
	root := keelSchemaDir(t)
	var dirs []string
	var seeds []*keelschema.SeedFile
	for _, group := range keelGroups {
		dirs = append(dirs, filepath.Join(root, group))
		if path := filepath.Join(root, "seed", group+".yml"); fileExists(path) {
			parsed, err := keelschema.ParseSeedPath(path)
			if err != nil {
				t.Fatal(err)
			}
			seeds = append(seeds, parsed...)
		}
	}
	modules, err := filepath.Glob("schema/*/ab_meta.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, meta := range modules {
		dirs = append(dirs, filepath.Dir(meta))
	}
	parsed, err := keelschema.ParseDirs(dirs)
	if err != nil {
		t.Fatal(err)
	}
	seedDirs, err := filepath.Glob("schema/seed/*")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range seedDirs {
		scoutSeeds, err := keelschema.ParseSeedPath(dir)
		if err != nil {
			t.Fatal(err)
		}
		seeds = append(seeds, scoutSeeds...)
	}
	if len(seedDirs) == 0 {
		t.Fatal("no Scout seed directories found")
	}
	if err := keelschema.ValidateSeeds(seeds, parsed); err != nil {
		t.Fatal(err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// What keel's validation leaves to Scout: a CHAR column holds every code of its domain, and
// Scout's seed rows write only codes of the domain a column maps.
func TestConstantCodesFitTheirColumnsAndSeeds(t *testing.T) {
	values, lookups := map[string][]string{}, map[[2]string]string{}
	var blocks [][3]string // path, table, block
	for _, pattern := range []string{filepath.Join(keelSchemaDir(t), "seed", "*.yml"), "schema/seed/*/*.yml"} {
		paths, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			scout := strings.HasPrefix(path, "schema/")
			for _, block := range seedTable.FindAllStringSubmatch(string(content), -1) {
				for _, row := range seedRow.FindAllStringSubmatch(block[3], -1) {
					fields := splitFields(row[1], 4)
					switch block[1] {
					case "constant_value":
						values[fields[0]] = append(values[fields[0]], fields[1])
					case "constant_lookup":
						lookups[[2]string{fields[1], fields[2]}] = fields[0]
					}
				}
				if scout {
					blocks = append(blocks, [3]string{path, block[1], block[0]})
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
	for key, domain := range lookups {
		schema, ok := tables[key[0]]
		if !ok {
			continue
		}
		typed := regexp.MustCompile(strings.Replace(columnTypeAt, "%s", key[1], 1)).FindStringSubmatch(schema)
		if typed == nil || typed[1] != "CHAR" {
			continue
		}
		width, _ := strconv.Atoi(typed[2])
		for _, code := range values[domain] {
			if len(code) > width {
				t.Errorf("%s.%s is CHAR(%d) but %s code %q is longer", key[0], key[1], width, domain, code)
			}
		}
	}
	for _, block := range blocks {
		parsed := seedTable.FindStringSubmatch(block[2])
		columns := splitFields(parsed[2], -1)
		for _, row := range seedRow.FindAllStringSubmatch(parsed[3], -1) {
			fields := splitFields(row[1], len(columns))
			for i, column := range columns {
				domain, ok := lookups[[2]string{block[1], column}]
				if ok && i < len(fields) && fields[i] != "" && !slices.Contains(values[domain], fields[i]) {
					t.Errorf("%s: %s.%s seeds %q, not a %s code %v", block[0], block[1], column, fields[i], domain, values[domain])
				}
			}
		}
	}
}

func splitFields(list string, n int) []string {
	fields := strings.SplitN(list, ",", n)
	for i := range fields {
		fields[i] = strings.Trim(strings.TrimSpace(fields[i]), `"`)
	}
	return fields
}
