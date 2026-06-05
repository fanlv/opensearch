package search

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Search argument bounds.
const (
	queryMinLen      = 1
	queryMaxLen      = 2048
	defaultNumResult = 8
	minNumResult     = 1
	maxNumResult     = 20
	maxDomains       = 20
)

// Params is the parsed search argument set. Include/ExcludeDomains are normalized and deduplicated.
type Params struct {
	Query           string
	NumResults      int
	PublishedAfter  *time.Time
	PublishedBefore *time.Time
	IncludeDomains  []string
	ExcludeDomains  []string
	OutputPath      string
}

// ParseArgs parses search subcommand arguments without adding CLI dependencies.
// Failures wrap ErrInvalidArgs.
func ParseArgs(args []string) (*Params, error) {
	p := &Params{NumResults: defaultNumResult}
	var (
		numSet     bool
		afterSet   bool
		beforeSet  bool
		afterRaw   string
		beforeRaw  string
		include    []string
		exclude    []string
		queryParts []string
	)

	i := 0
	for i < len(args) {
		arg := args[i]
		// Everything after "--" is treated as a query token.
		if arg == "--" {
			queryParts = append(queryParts, args[i+1:]...)
			break
		}
		switch {
		case arg == "-n" || arg == "--num-results":
			v, ni, err := takeValue(args, i, arg)
			if err != nil {
				return nil, err
			}
			n, perr := strconv.Atoi(strings.TrimSpace(v))
			if perr != nil {
				return nil, invalidArg("--num-results must be an integer")
			}
			p.NumResults = n
			numSet = true
			i = ni
		case strings.HasPrefix(arg, "--num-results="):
			n, perr := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(arg, "--num-results=")))
			if perr != nil {
				return nil, invalidArg("--num-results must be an integer")
			}
			p.NumResults = n
			numSet = true
			i++
		case arg == "--published-after":
			v, ni, err := takeValue(args, i, arg)
			if err != nil {
				return nil, err
			}
			afterSet = true
			afterRaw = v
			i = ni
		case strings.HasPrefix(arg, "--published-after="):
			afterSet = true
			afterRaw = strings.TrimPrefix(arg, "--published-after=")
			i++
		case arg == "--published-before":
			v, ni, err := takeValue(args, i, arg)
			if err != nil {
				return nil, err
			}
			beforeSet = true
			beforeRaw = v
			i = ni
		case strings.HasPrefix(arg, "--published-before="):
			beforeSet = true
			beforeRaw = strings.TrimPrefix(arg, "--published-before=")
			i++
		case arg == "--include-domain":
			v, ni, err := takeValue(args, i, arg)
			if err != nil {
				return nil, err
			}
			include = append(include, v)
			i = ni
		case strings.HasPrefix(arg, "--include-domain="):
			include = append(include, strings.TrimPrefix(arg, "--include-domain="))
			i++
		case arg == "--exclude-domain":
			v, ni, err := takeValue(args, i, arg)
			if err != nil {
				return nil, err
			}
			exclude = append(exclude, v)
			i = ni
		case strings.HasPrefix(arg, "--exclude-domain="):
			exclude = append(exclude, strings.TrimPrefix(arg, "--exclude-domain="))
			i++
		case arg == "-o" || arg == "--output":
			v, ni, err := takeValue(args, i, arg)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(v) == "" {
				return nil, invalidArg("%s requires a non-empty path", arg)
			}
			p.OutputPath = v
			i = ni
		case strings.HasPrefix(arg, "--output="):
			p.OutputPath = strings.TrimPrefix(arg, "--output=")
			if strings.TrimSpace(p.OutputPath) == "" {
				return nil, invalidArg("--output requires a non-empty path")
			}
			i++
		case strings.HasPrefix(arg, "-") && arg != "-":
			return nil, invalidArg("unknown option %q", arg)
		default:
			queryParts = append(queryParts, arg)
			i++
		}
	}

	// Join positional tokens into the query, trim, and validate character length.
	p.Query = strings.TrimSpace(strings.Join(queryParts, " "))
	queryLen := utf8.RuneCountInString(p.Query)
	if queryLen < queryMinLen {
		return nil, invalidArg("query must not be empty")
	}
	if queryLen > queryMaxLen {
		return nil, invalidArg("query must be at most %d characters", queryMaxLen)
	}

	// num-results bounds.
	if numSet && (p.NumResults < minNumResult || p.NumResults > maxNumResult) {
		return nil, invalidArg("--num-results must be within %d-%d", minNumResult, maxNumResult)
	}

	// Published filters use RFC 3339 and start must not be later than end.
	if afterSet {
		t, err := parseRFC3339(afterRaw, "--published-after")
		if err != nil {
			return nil, err
		}
		p.PublishedAfter = &t
	}
	if beforeSet {
		t, err := parseRFC3339(beforeRaw, "--published-before")
		if err != nil {
			return nil, err
		}
		p.PublishedBefore = &t
	}
	if p.PublishedAfter != nil && p.PublishedBefore != nil && p.PublishedAfter.After(*p.PublishedBefore) {
		return nil, invalidArg("--published-after must not be later than --published-before")
	}

	// Normalize and bound domain filters.
	inc, err := normalizeDomainList(include, maxDomains, "include")
	if err != nil {
		return nil, err
	}
	exc, err := normalizeDomainList(exclude, maxDomains, "exclude")
	if err != nil {
		return nil, err
	}
	// The same normalized domain cannot be both included and excluded.
	excSet := make(map[string]struct{}, len(exc))
	for _, d := range exc {
		excSet[d] = struct{}{}
	}
	for _, d := range inc {
		if _, ok := excSet[d]; ok {
			return nil, invalidArg("domain %q cannot be both included and excluded", d)
		}
	}
	p.IncludeDomains = inc
	p.ExcludeDomains = exc

	return p, nil
}

// takeValue reads the next token as a flag value and returns the advanced index.
func takeValue(args []string, i int, flag string) (string, int, error) {
	if i+1 >= len(args) {
		return "", 0, invalidArg("%s requires a value", flag)
	}
	return args[i+1], i + 2, nil
}

// parseRFC3339 parses an RFC 3339 timestamp.
func parseRFC3339(v, flag string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
	if err != nil {
		return time.Time{}, invalidArg("%s must be an RFC 3339 timestamp", flag)
	}
	return t, nil
}
