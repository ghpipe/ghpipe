package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// flagKind says whether a flag carries a value.
type flagKind int

const (
	boolFlag flagKind = iota
	valueFlag
)

// parsedFlags is the result of parsing one subcommand's arguments. The parser
// is deliberately tiny and strict: an unknown flag is an error, never a value
// that silently reaches the next layer (docs/design.md 3: 禁止任意 flag 透传).
type parsedFlags struct {
	values map[string]string
	bools  map[string]bool
	rest   []string
}

func parseFlags(args []string, spec map[string]flagKind) (parsedFlags, error) {
	out := parsedFlags{values: map[string]string{}, bools: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			out.rest = append(out.rest, args[i+1:]...)
			return out, nil
		}
		if !strings.HasPrefix(arg, "-") {
			out.rest = append(out.rest, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		value := ""
		hasValue := false
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name, value, hasValue = name[:eq], name[eq+1:], true
		}
		kind, known := spec[name]
		if !known {
			return out, fmt.Errorf("unknown flag %q", arg)
		}
		if kind == boolFlag {
			if hasValue {
				return out, fmt.Errorf("flag --%s does not take a value", name)
			}
			out.bools[name] = true
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				return out, fmt.Errorf("missing value for --%s", name)
			}
			i++
			value = args[i]
		}
		out.values[name] = value
	}
	return out, nil
}

// value returns a value flag, or "" when it was not given.
func (f parsedFlags) value(name string) string { return f.values[name] }

// has reports whether a boolean flag was given.
func (f parsedFlags) has(name string) bool { return f.bools[name] }

// positiveInt parses a required positive number such as an issue number.
func positiveInt(flag, raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, fmt.Errorf("--%s needs a number", flag)
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("--%s must be a positive integer", flag)
	}
	return value, nil
}
