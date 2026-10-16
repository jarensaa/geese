package goose

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/jarensaa/geese"
)

// Classify says what a SQL migration does to code already serving the schema
// before it. It reads the statements of its Up section, and lets the flock go
// on serving through those it knows keep that code working and hold their
// locks only briefly: making tables, and indexes on them; adding types,
// sequences, schemas, enum values and comments; adding a column that's
// nullable or has a constant default; adding a constraint NOT VALID, and
// validating it; and dropping tables and columns, which every goose must
// declare unused. Anything else, and anything it can't read for certain,
// pauses.
func Classify(migration []byte) geese.Change {
	up, why := upSection(string(migration))
	if why != "" {
		return geese.Change{Pause: why}
	}
	statements, why := statementsOf(up)
	if why != "" {
		return geese.Change{Pause: why}
	}
	created := map[string]bool{}
	var drops []string
	for _, statement := range statements {
		dropped, why := classify(statement, created)
		if why != "" {
			return geese.Change{Pause: why}
		}
		drops = append(drops, dropped...)
	}
	return geese.Change{Drops: drops}
}

var annotation = regexp.MustCompile(`^\s*--\s*\+goose\s+(.+?)\s*$`)

// upSection is the SQL of a migration's Up section, or why it can't be read.
func upSection(migration string) (string, string) {
	var up strings.Builder
	in, seen := false, false
	for _, line := range strings.Split(migration, "\n") {
		if match := annotation.FindStringSubmatch(line); match != nil {
			switch strings.ToUpper(match[1]) {
			case "UP":
				in, seen = true, true
			case "DOWN":
				in = false
			case "NO TRANSACTION":
				return "", "it runs outside a transaction"
			default:
				if in {
					return "", fmt.Sprintf("it has goose's %s, which geese doesn't read", match[1])
				}
			}
			continue
		}
		if in {
			up.WriteString(line)
			up.WriteByte('\n')
		}
	}
	if !seen {
		return "", "it has no -- +goose Up section"
	}
	return up.String(), ""
}

var dollarQuote = regexp.MustCompile(`^\$[A-Za-z_]*\$`)

// statementsOf splits SQL into its statements, without comments, string
// literals as '_' and whitespace as single spaces, or says why it can't.
func statementsOf(sql string) ([]string, string) {
	var statements []string
	var b strings.Builder
	flush := func() {
		if statement := strings.Join(strings.Fields(b.String()), " "); statement != "" {
			statements = append(statements, statement)
		}
		b.Reset()
	}
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case strings.HasPrefix(sql[i:], "--"):
			end := strings.IndexByte(sql[i:], '\n')
			if end < 0 {
				end = len(sql) - i
			}
			i += end
			b.WriteByte(' ')
		case strings.HasPrefix(sql[i:], "/*"):
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return nil, "it has a comment that doesn't end"
			}
			i += end + 3
			b.WriteByte(' ')
		case c == '\'':
			if i > 0 && strings.ContainsRune("eE", rune(sql[i-1])) && (i == 1 || !isWord(sql[i-2])) {
				return nil, "it has an escape string, which geese doesn't read"
			}
			end := i + 1
			for ; end < len(sql); end++ {
				if sql[end] == '\'' {
					if end+1 < len(sql) && sql[end+1] == '\'' {
						end++
						continue
					}
					break
				}
			}
			if end >= len(sql) {
				return nil, "it has a string that doesn't end"
			}
			b.WriteString("'_'")
			i = end
		case c == '"':
			end := strings.IndexByte(sql[i+1:], '"')
			if end < 0 {
				return nil, "it has a quoted name that doesn't end"
			}
			b.WriteString(sql[i : i+end+2])
			i += end + 1
		case c == '$' && dollarQuote.MatchString(sql[i:]):
			return nil, "it has dollar-quoted text, which geese doesn't read"
		case c == ';':
			flush()
		default:
			b.WriteByte(c)
		}
	}
	flush()
	return statements, ""
}

func isWord(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

const (
	ident = `(?:"[^"]+"|[A-Za-z_][\w$]*)`
	name  = ident + `(?:\.` + ident + `){0,2}`
)

var (
	createTable       = regexp.MustCompile(`(?i)^create (?:unlogged )?table (?:if not exists )?(` + name + `) ?\(`)
	inheriting        = regexp.MustCompile(`(?i)\b(?:partition of|inherits)\b`)
	createIndex       = regexp.MustCompile(`(?i)^create (?:unique )?index (?:if not exists )?(?:` + ident + ` )?on (?:only )?(` + name + `)`)
	alterTable        = regexp.MustCompile(`(?i)^alter table (?:if exists )?(?:only )?(` + name + `) (.+)$`)
	addConstraint     = regexp.MustCompile(`(?i)^add constraint ` + ident + ` (?:foreign key|check)\b.* not valid$`)
	validate          = regexp.MustCompile(`(?i)^validate constraint ` + ident + `$`)
	addColumn         = regexp.MustCompile(`(?i)^add (?:column )?(?:if not exists )?(` + ident + `) (.+)$`)
	tableConstraint   = regexp.MustCompile(`(?i)^add (?:constraint|primary key|unique|foreign key|check|exclude)\b`)
	checksRows        = regexp.MustCompile(`(?i)\b(?:primary key|unique|references|check|exclude)\b`)
	generated         = regexp.MustCompile(`(?i)\bgenerated\b`)
	columnDefault     = regexp.MustCompile(`(?i)\bdefault (.+?)(?: (?:not null|null|constraint|collate)\b|$)`)
	constant          = regexp.MustCompile(`(?i)^(?:'_'|-?\d+(?:\.\d+)?|true|false|null)(?: ?:: ?[\w ]+(?:\(\d+(?: ?, ?\d+)?\))?(?:\[\])?)?$`)
	notNull           = regexp.MustCompile(`(?i)\bnot null\b`)
	dropColumn        = regexp.MustCompile(`(?i)^drop (?:column )?(?:if exists )?(` + ident + `)(?: restrict)?$`)
	dropTable         = regexp.MustCompile(`(?i)^drop table (?:if exists )?(` + name + `(?: ?, ?` + name + `)*)(?: restrict)?$`)
	harmless          = regexp.MustCompile(`(?i)^(?:create (?:type|sequence|schema)\b|comment on\b|alter type ` + name + ` add value\b)`)
	identifierOrComma = regexp.MustCompile(`"[^"]+"|[^,."\s]+|[.,]`)
)

// classify says what one statement drops, or why the flock pauses for it.
// Tables made earlier in the migration are in created: nothing serving reads
// them yet.
func classify(statement string, created map[string]bool) ([]string, string) {
	if match := createTable.FindStringSubmatch(statement); match != nil && !inheriting.MatchString(statement) {
		created[normalize(match[1])] = true
		return nil, ""
	}
	if match := createIndex.FindStringSubmatch(statement); match != nil {
		if created[normalize(match[1])] {
			return nil, ""
		}
		return nil, fmt.Sprintf("CREATE INDEX on %s blocks writes to it while it builds", normalize(match[1]))
	}
	if match := alterTable.FindStringSubmatch(statement); match != nil {
		table, action := normalize(match[1]), match[2]
		if created[table] {
			return nil, ""
		}
		if topLevelComma(action) {
			return nil, fmt.Sprintf("it makes more than one change in a statement to %s", table)
		}
		switch {
		case addConstraint.MatchString(action), validate.MatchString(action):
			return nil, ""
		case tableConstraint.MatchString(action):
		case addColumn.MatchString(action):
			column := addColumn.FindStringSubmatch(action)
			definition := column[2]
			if generated.MatchString(definition) {
				return nil, fmt.Sprintf("adding %s.%s, a generated column, rewrites the table", table, normalize(column[1]))
			}
			if checksRows.MatchString(definition) {
				return nil, fmt.Sprintf("adding %s.%s with a constraint checks every row of the table", table, normalize(column[1]))
			}
			if value := columnDefault.FindStringSubmatch(definition); value != nil {
				if !constant.MatchString(strings.TrimSpace(value[1])) {
					return nil, fmt.Sprintf("adding %s.%s with a default that isn't a constant rewrites the table", table, normalize(column[1]))
				}
				return nil, ""
			}
			if notNull.MatchString(definition) {
				return nil, fmt.Sprintf("adding %s.%s NOT NULL without a default fails on rows already there", table, normalize(column[1]))
			}
			return nil, ""
		case dropColumn.MatchString(action):
			return []string{table + "." + normalize(dropColumn.FindStringSubmatch(action)[1])}, ""
		}
	}
	if match := dropTable.FindStringSubmatch(statement); match != nil {
		var drops []string
		for _, table := range names(match[1]) {
			if !created[table] {
				drops = append(drops, table)
			}
		}
		return drops, ""
	}
	if harmless.MatchString(statement) {
		return nil, ""
	}
	if len(statement) > 60 {
		statement = statement[:60] + "…"
	}
	return nil, fmt.Sprintf("geese can't tell code already serving keeps working through %q", statement)
}

// normalize is a name as PostgreSQL resolves it: quoted parts as they are,
// the others in lower case.
func normalize(raw string) string {
	var parts []string
	for _, token := range identifierOrComma.FindAllString(raw, -1) {
		switch {
		case token == ".":
		case strings.HasPrefix(token, `"`):
			parts = append(parts, strings.Trim(token, `"`))
		default:
			parts = append(parts, strings.ToLower(token))
		}
	}
	return strings.Join(parts, ".")
}

// names are the comma-separated names of a list.
func names(list string) []string {
	var out []string
	var current strings.Builder
	for _, token := range identifierOrComma.FindAllString(list, -1) {
		if token == "," {
			out = append(out, normalize(current.String()))
			current.Reset()
			continue
		}
		current.WriteString(token)
	}
	return append(out, normalize(current.String()))
}

// topLevelComma says whether text has a comma outside parentheses.
func topLevelComma(text string) bool {
	depth := 0
	for _, c := range text {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				return true
			}
		}
	}
	return false
}
