package scrape

import (
	"strconv"
	"strings"
)

// ParseArgs parses and validates scrape command options. URL syntax itself is
// intentionally left to BuildInputResults so invalid URL inputs become per-item
// failures rather than command-level INVALID_ARGUMENT.
func ParseArgs(args []string, defaultConcurrency int) (*Params, error) {
	p := &Params{
		Format:            FormatMarkdown,
		MainContent:       true,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
		TotalTimeoutSecs:  DefaultTotalTimeoutSeconds,
		Concurrency:       defaultConcurrency,
	}

	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--" {
			p.URLs = append(p.URLs, args[i+1:]...)
			break
		}

		switch {
		case arg == "--format":
			v, ni, err := takeValue(args, i, arg)
			if err != nil {
				return nil, err
			}
			p.Format = strings.TrimSpace(v)
			i = ni
		case strings.HasPrefix(arg, "--format="):
			p.Format = trimOptionValue(arg, "--format=")
			i++
		case arg == "--main-content":
			p.MainContent = true
			i++
		case arg == "--no-main-content":
			p.MainContent = false
			i++
		case arg == "--timeout":
			v, ni, err := takeValue(args, i, arg)
			if err != nil {
				return nil, err
			}
			n, err := parseIntFlag(v, "--timeout")
			if err != nil {
				return nil, err
			}
			p.PerURLTimeoutSecs = n
			i = ni
		case strings.HasPrefix(arg, "--timeout="):
			n, err := parseIntFlag(trimOptionValue(arg, "--timeout="), "--timeout")
			if err != nil {
				return nil, err
			}
			p.PerURLTimeoutSecs = n
			i++
		case arg == "--total-timeout":
			v, ni, err := takeValue(args, i, arg)
			if err != nil {
				return nil, err
			}
			n, err := parseIntFlag(v, "--total-timeout")
			if err != nil {
				return nil, err
			}
			p.TotalTimeoutSecs = n
			i = ni
		case strings.HasPrefix(arg, "--total-timeout="):
			n, err := parseIntFlag(trimOptionValue(arg, "--total-timeout="), "--total-timeout")
			if err != nil {
				return nil, err
			}
			p.TotalTimeoutSecs = n
			i++
		case arg == "--concurrency":
			v, ni, err := takeValue(args, i, arg)
			if err != nil {
				return nil, err
			}
			n, err := parseIntFlag(v, "--concurrency")
			if err != nil {
				return nil, err
			}
			p.Concurrency = n
			i = ni
		case strings.HasPrefix(arg, "--concurrency="):
			n, err := parseIntFlag(trimOptionValue(arg, "--concurrency="), "--concurrency")
			if err != nil {
				return nil, err
			}
			p.Concurrency = n
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
			p.URLs = append(p.URLs, arg)
			i++
		}
	}

	if err := validateFormat(p.Format); err != nil {
		return nil, err
	}
	if p.PerURLTimeoutSecs < MinPerURLTimeoutSeconds || p.PerURLTimeoutSecs > MaxPerURLTimeoutSeconds {
		return nil, invalidArg("--timeout must be within %d-%d", MinPerURLTimeoutSeconds, MaxPerURLTimeoutSeconds)
	}
	if p.TotalTimeoutSecs < MinTotalTimeoutSeconds || p.TotalTimeoutSecs > MaxTotalTimeoutSeconds {
		return nil, invalidArg("--total-timeout must be within %d-%d", MinTotalTimeoutSeconds, MaxTotalTimeoutSeconds)
	}
	if err := validateConcurrency(p.Concurrency); err != nil {
		return nil, err
	}
	if len(p.URLs) < MinURLs || len(p.URLs) > MaxURLs {
		return nil, invalidArg("scrape requires %d-%d URL inputs", MinURLs, MaxURLs)
	}

	return p, nil
}

func parseIntFlag(v, flag string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, invalidArg("%s must be an integer", flag)
	}
	return n, nil
}
