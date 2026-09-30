# Upstream to keel — shared backend work

| ID | Pri | Gap | Proposed API | Blocks |
|---|---:|---|---|---|
| SK-K2 | P3 | `schemagen -seed` reads directories only, but keel seeds one `<group>.yml` per group in a shared directory; since seeded foreign keys are validated per run, a downstream installing some keel groups must copy those files into a staging directory to seed exactly them | `-seed` accepts YAML files as well as directories | README schema generation stages the selected groups' seed files into a temporary directory, as other downstream generation scripts already do |
